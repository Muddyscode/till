package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/Muddyscode/till/internal/ledger"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ---- merchant settings ----

type SettingsPatch struct {
	VATRegistered         *bool
	VATInclusiveBps       *int
	MaxDiscountBpsCashier *int
	MaxDiscountBpsManager *int
}

func (s *Store) UpdateMerchantSettings(ctx context.Context, tx pgx.Tx, id uuid.UUID, p SettingsPatch) (Merchant, error) {
	return scanMerchant(tx.QueryRow(ctx, `
		UPDATE merchants SET
			vat_registered = COALESCE($2, vat_registered),
			vat_inclusive_bps = COALESCE($3, vat_inclusive_bps),
			max_discount_bps_cashier = COALESCE($4, max_discount_bps_cashier),
			max_discount_bps_manager = COALESCE($5, max_discount_bps_manager)
		WHERE id = $1
		RETURNING `+merchantCols+`
	`, id, p.VATRegistered, p.VATInclusiveBps, p.MaxDiscountBpsCashier, p.MaxDiscountBpsManager))
}

// ---- deposits account (2400) ----

// EnsureDepositsAccount returns the id of "Deposits held" (2400), creating it
// on first use. Cash and wallet tenders of a split order that is still waiting
// for its transfer portion sit here.
func (s *Store) EnsureDepositsAccount(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID) (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO accounts (id, merchant_id, code, name, type, currency, allow_negative, status, balance_minor)
		VALUES ($1, $2, '2400', 'Deposits held', 'liability', 'NGN', false, 'open', 0)
		ON CONFLICT (merchant_id, code) DO NOTHING
	`, id, merchantID); err != nil {
		return uuid.Nil, err
	}
	var out uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM accounts WHERE merchant_id = $1 AND code = '2400'`, merchantID).Scan(&out)
	return out, err
}

// DepositsAccountID returns the 2400 account id if it exists.
func (s *Store) DepositsAccountID(ctx context.Context, db DBTX, merchantID uuid.UUID) (uuid.UUID, bool, error) {
	var id uuid.UUID
	err := db.QueryRow(ctx, `SELECT id FROM accounts WHERE merchant_id = $1 AND code = '2400'`, merchantID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	return id, err == nil, err
}

// ---- payments (tenders) ----

type Payment struct {
	ID               uuid.UUID
	OrderID          uuid.UUID
	Method           string
	Status           string
	AmountMinor      int64
	RefundedMinor    int64
	DepositJournalID *uuid.UUID
}

const paymentCols = `id, order_id, method, status, amount_minor, refunded_minor, deposit_journal_id`

func scanPayment(sc interface{ Scan(...any) error }) (Payment, error) {
	var p Payment
	err := sc.Scan(&p.ID, &p.OrderID, &p.Method, &p.Status, &p.AmountMinor, &p.RefundedMinor, &p.DepositJournalID)
	return p, err
}

func (s *Store) loadPayments(ctx context.Context, db DBTX, orders []Order) ([]Order, error) {
	if len(orders) == 0 {
		return orders, nil
	}
	ids := make([]uuid.UUID, len(orders))
	byID := map[uuid.UUID]int{}
	for i, o := range orders {
		ids[i] = o.ID
		byID[o.ID] = i
	}
	rows, err := db.Query(ctx, `
		SELECT `+paymentCols+` FROM payments WHERE order_id = ANY($1) AND status <> 'failed'
		ORDER BY order_id, CASE method WHEN 'cash' THEN 1 WHEN 'wallet' THEN 2 ELSE 3 END, created_at
	`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return nil, err
		}
		i := byID[p.OrderID]
		orders[i].Payments = append(orders[i].Payments, p)
	}
	return orders, rows.Err()
}

// OrderPayments returns an order's tenders (failed ones excluded).
func (s *Store) OrderPayments(ctx context.Context, db DBTX, orderID uuid.UUID) ([]Payment, error) {
	got, err := s.loadPayments(ctx, db, []Order{{ID: orderID}})
	if err != nil {
		return nil, err
	}
	return got[0].Payments, nil
}

// InsertTender writes one payment row with its deposit journal (held tenders).
func (s *Store) InsertTender(ctx context.Context, tx pgx.Tx, p Payment, provider string, journalID *uuid.UUID) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO payments (id, order_id, method, status, amount_minor, provider, journal_id, deposit_journal_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
	`, p.ID, p.OrderID, p.Method, p.Status, p.AmountMinor, provider, journalID, p.DepositJournalID)
	return err
}

func (s *Store) HeldPayments(ctx context.Context, tx pgx.Tx, orderID uuid.UUID) ([]Payment, error) {
	rows, err := tx.Query(ctx, `SELECT `+paymentCols+` FROM payments WHERE order_id = $1 AND status = 'held' ORDER BY method`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Payment
	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PendingTransferMinor is the transfer amount still owed on an order.
func (s *Store) PendingTransferMinor(ctx context.Context, db DBTX, orderID uuid.UUID) (int64, bool, error) {
	var n int64
	err := db.QueryRow(ctx, `
		SELECT amount_minor FROM payments WHERE order_id = $1 AND method = 'transfer' AND status = 'pending'
	`, orderID).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return n, err == nil, err
}

// ConfirmHeld turns the held tenders into confirmed ones when the sale posts.
func (s *Store) ConfirmHeld(ctx context.Context, tx pgx.Tx, orderID, journalID uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE payments SET status = 'confirmed', journal_id = $2 WHERE order_id = $1 AND status = 'held'`, orderID, journalID)
	return err
}

func (s *Store) ReturnHeld(ctx context.Context, tx pgx.Tx, orderID, journalID uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE payments SET status = 'returned', journal_id = $2 WHERE order_id = $1 AND status = 'held'`, orderID, journalID)
	return err
}

func (s *Store) FailPendingPayments(ctx context.Context, tx pgx.Tx, orderID uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE payments SET status = 'failed' WHERE order_id = $1 AND status = 'pending'`, orderID)
	return err
}

func (s *Store) AddPaymentRefunded(ctx context.Context, tx pgx.Tx, paymentID uuid.UUID, amount int64) error {
	_, err := tx.Exec(ctx, `UPDATE payments SET refunded_minor = refunded_minor + $2 WHERE id = $1`, paymentID, amount)
	return err
}

// ---- order lines and status ----

// SetLineCosts stores each line's cost of goods (set when the sale posts).
func (s *Store) SetLineCosts(ctx context.Context, tx pgx.Tx, lines []OrderLine) error {
	for _, ln := range lines {
		if _, err := tx.Exec(ctx, `UPDATE order_lines SET cost_minor = $2 WHERE id = $1`, ln.ID, ln.CostMinor); err != nil {
			return err
		}
	}
	return nil
}

// MarkAllRefunded marks every unit and tender of an order as refunded (the
// whole-order reversal path).
func (s *Store) MarkAllRefunded(ctx context.Context, tx pgx.Tx, orderID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `UPDATE order_lines SET refunded_qty = qty WHERE order_id = $1`, orderID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE payments SET refunded_minor = amount_minor WHERE order_id = $1 AND status = 'confirmed'`, orderID)
	return err
}

func (s *Store) AddLineRefundedQty(ctx context.Context, tx pgx.Tx, lineID uuid.UUID, qty int) error {
	tag, err := tx.Exec(ctx, `UPDATE order_lines SET refunded_qty = refunded_qty + $2 WHERE id = $1 AND refunded_qty + $2 <= qty`, lineID, qty)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ledger.Err("refund_exceeds_order", "refund quantity exceeds what is left on the order")
	}
	return nil
}

// ApplyRefund records a refund on the order: running total, latest refund
// fields and the new status (partially_refunded or refunded).
func (s *Store) ApplyRefund(ctx context.Context, tx pgx.Tx, orderID, journalID, staffID uuid.UUID, reason string, at time.Time, amount int64, status string) error {
	tag, err := tx.Exec(ctx, `
		UPDATE orders
		SET status = $2, refund_journal_id = $3, refunded_by = $4, refund_reason = NULLIF($5, ''),
		    refunded_at = $6, refunded_minor = refunded_minor + $7
		WHERE id = $1 AND status IN ('paid', 'partially_refunded')
	`, orderID, status, journalID, staffID, reason, at, amount)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ledger.Err(ledger.CodeInvalidOrder, "only a paid order can be refunded")
	}
	return nil
}

func (s *Store) MarkVoided(ctx context.Context, tx pgx.Tx, orderID uuid.UUID) error {
	tag, err := tx.Exec(ctx, `UPDATE orders SET status = 'voided' WHERE id = $1 AND status = 'pending_payment'`, orderID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ledger.Err(ledger.CodeInvalidOrder, "order is not pending")
	}
	return nil
}

// ---- order refunds ----

type OrderRefund struct {
	ID          uuid.UUID
	MerchantID  uuid.UUID
	OrderID     uuid.UUID
	OutletID    uuid.UUID
	JournalID   *uuid.UUID
	AmountMinor int64
	NetMinor    int64
	VATMinor    int64
	CostMinor   int64
	Lines       []byte // JSON
	Tenders     []byte // JSON
	Reason      string
	StaffID     uuid.UUID
	CreatedAt   time.Time
}

const refundCols = `id, merchant_id, order_id, outlet_id, journal_id, amount_minor, net_minor, vat_minor, cost_minor,
	lines, tenders, COALESCE(reason, ''), staff_id, created_at`

func (s *Store) InsertOrderRefund(ctx context.Context, tx pgx.Tx, r OrderRefund) error {
	if len(r.Lines) == 0 {
		r.Lines = []byte(`[]`)
	}
	if len(r.Tenders) == 0 {
		r.Tenders = []byte(`{}`)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO order_refunds (id, merchant_id, order_id, outlet_id, journal_id, amount_minor, net_minor, vat_minor,
			cost_minor, lines, tenders, reason, staff_id, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NULLIF($12,''),$13,$14)
	`, r.ID, r.MerchantID, r.OrderID, r.OutletID, r.JournalID, r.AmountMinor, r.NetMinor, r.VATMinor, r.CostMinor,
		r.Lines, r.Tenders, r.Reason, r.StaffID, r.CreatedAt)
	return err
}

func (s *Store) ListOrderRefunds(ctx context.Context, db DBTX, orderID uuid.UUID) ([]OrderRefund, error) {
	rows, err := db.Query(ctx, `SELECT `+refundCols+` FROM order_refunds WHERE order_id = $1 ORDER BY created_at, id`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OrderRefund{}
	for rows.Next() {
		var r OrderRefund
		if err := rows.Scan(&r.ID, &r.MerchantID, &r.OrderID, &r.OutletID, &r.JournalID, &r.AmountMinor, &r.NetMinor,
			&r.VATMinor, &r.CostMinor, &r.Lines, &r.Tenders, &r.Reason, &r.StaffID, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- daily report ----

type MethodTotal struct {
	Method     string
	OrderCount int
	TotalMinor int64
	VATMinor   int64
	NetMinor   int64
}

type CashierTotal struct {
	StaffID       uuid.UUID
	Email         string
	OrderCount    int
	TotalMinor    int64
	DiscountMinor int64
	RefundCount   int
	RefundsMinor  int64
}

// DailySales is one outlet-day. Sales count on the Lagos day they were paid,
// whatever happened to them later. Refunds count on the day they were made.
type DailySales struct {
	OrderCount    int
	TotalMinor    int64 // gross sales, VAT inclusive, refunded orders included
	VATMinor      int64
	NetMinor      int64 // net of VAT (ex-VAT revenue), before refunds
	DiscountMinor int64
	// BooksDebit is what the sale journals of these orders debited to asset and
	// liability accounts (cash, wallet, clearing, suspense, deposits held).
	BooksDebit   int64
	RefundCount  int
	RefundsMinor int64
	// BooksRefundCredit is what the refund journals made that day credited to
	// asset and liability accounts (cash, wallet, refunds payable).
	BooksRefundCredit int64
	ByMethod          []MethodTotal
	Tenders           map[string]int64 // cash, transfer, wallet received on sales of the day
	ByCashier         []CashierTotal
}

const saleStatuses = `('paid', 'partially_refunded', 'refunded')`

func (s *Store) DailySales(ctx context.Context, db DBTX, outletID uuid.UUID, day string) (DailySales, error) {
	ds := DailySales{Tenders: map[string]int64{"cash": 0, "transfer": 0, "wallet": 0}}
	err := db.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(SUM(total_minor),0), COALESCE(SUM(vat_minor),0), COALESCE(SUM(net_minor),0),
		       COALESCE(SUM(discount_minor),0)
		FROM orders
		WHERE outlet_id = $1 AND status IN `+saleStatuses+`
		  AND (paid_at AT TIME ZONE 'Africa/Lagos')::date = $2::date
	`, outletID, day).Scan(&ds.OrderCount, &ds.TotalMinor, &ds.VATMinor, &ds.NetMinor, &ds.DiscountMinor)
	if err != nil {
		return DailySales{}, err
	}
	err = db.QueryRow(ctx, `
		SELECT COALESCE(SUM(p.amount_minor),0)
		FROM orders o
		JOIN postings p ON p.journal_id = o.journal_id AND p.direction = 'debit'
		JOIN accounts a ON a.id = p.account_id AND a.type IN ('asset', 'liability')
		WHERE o.outlet_id = $1 AND o.status IN `+saleStatuses+`
		  AND (o.paid_at AT TIME ZONE 'Africa/Lagos')::date = $2::date
	`, outletID, day).Scan(&ds.BooksDebit)
	if err != nil {
		return DailySales{}, err
	}
	err = db.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(SUM(r.amount_minor),0)
		FROM order_refunds r
		WHERE r.outlet_id = $1 AND (r.created_at AT TIME ZONE 'Africa/Lagos')::date = $2::date
	`, outletID, day).Scan(&ds.RefundCount, &ds.RefundsMinor)
	if err != nil {
		return DailySales{}, err
	}
	err = db.QueryRow(ctx, `
		SELECT COALESCE(SUM(p.amount_minor),0)
		FROM order_refunds r
		JOIN postings p ON p.journal_id = r.journal_id AND p.direction = 'credit'
		JOIN accounts a ON a.id = p.account_id AND a.type IN ('asset', 'liability')
		WHERE r.outlet_id = $1 AND (r.created_at AT TIME ZONE 'Africa/Lagos')::date = $2::date
	`, outletID, day).Scan(&ds.BooksRefundCredit)
	if err != nil {
		return DailySales{}, err
	}

	rows, err := db.Query(ctx, `
		SELECT payment_method, COUNT(*), COALESCE(SUM(total_minor),0), COALESCE(SUM(vat_minor),0), COALESCE(SUM(net_minor),0)
		FROM orders
		WHERE outlet_id = $1 AND status IN `+saleStatuses+`
		  AND (paid_at AT TIME ZONE 'Africa/Lagos')::date = $2::date
		GROUP BY payment_method
		ORDER BY payment_method
	`, outletID, day)
	if err != nil {
		return DailySales{}, err
	}
	for rows.Next() {
		var m MethodTotal
		if err := rows.Scan(&m.Method, &m.OrderCount, &m.TotalMinor, &m.VATMinor, &m.NetMinor); err != nil {
			rows.Close()
			return DailySales{}, err
		}
		ds.ByMethod = append(ds.ByMethod, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return DailySales{}, err
	}

	rows, err = db.Query(ctx, `
		SELECT p.method, COALESCE(SUM(p.amount_minor),0)
		FROM payments p JOIN orders o ON o.id = p.order_id
		WHERE o.outlet_id = $1 AND o.status IN `+saleStatuses+` AND p.status = 'confirmed'
		  AND (o.paid_at AT TIME ZONE 'Africa/Lagos')::date = $2::date
		GROUP BY p.method
	`, outletID, day)
	if err != nil {
		return DailySales{}, err
	}
	for rows.Next() {
		var m string
		var n int64
		if err := rows.Scan(&m, &n); err != nil {
			rows.Close()
			return DailySales{}, err
		}
		ds.Tenders[m] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return DailySales{}, err
	}

	byStaff := map[uuid.UUID]*CashierTotal{}
	get := func(id uuid.UUID, email string) *CashierTotal {
		c, ok := byStaff[id]
		if !ok {
			c = &CashierTotal{StaffID: id, Email: email}
			byStaff[id] = c
		}
		return c
	}
	rows, err = db.Query(ctx, `
		SELECT o.staff_id, st.email, COUNT(*), COALESCE(SUM(o.total_minor),0), COALESCE(SUM(o.discount_minor),0)
		FROM orders o JOIN staff st ON st.id = o.staff_id
		WHERE o.outlet_id = $1 AND o.status IN `+saleStatuses+`
		  AND (o.paid_at AT TIME ZONE 'Africa/Lagos')::date = $2::date
		GROUP BY o.staff_id, st.email
	`, outletID, day)
	if err != nil {
		return DailySales{}, err
	}
	for rows.Next() {
		var id uuid.UUID
		var email string
		var n int
		var total, disc int64
		if err := rows.Scan(&id, &email, &n, &total, &disc); err != nil {
			rows.Close()
			return DailySales{}, err
		}
		c := get(id, email)
		c.OrderCount, c.TotalMinor, c.DiscountMinor = n, total, disc
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return DailySales{}, err
	}
	rows, err = db.Query(ctx, `
		SELECT r.staff_id, st.email, COUNT(*), COALESCE(SUM(r.amount_minor),0)
		FROM order_refunds r JOIN staff st ON st.id = r.staff_id
		WHERE r.outlet_id = $1 AND (r.created_at AT TIME ZONE 'Africa/Lagos')::date = $2::date
		GROUP BY r.staff_id, st.email
	`, outletID, day)
	if err != nil {
		return DailySales{}, err
	}
	for rows.Next() {
		var id uuid.UUID
		var email string
		var n int
		var amt int64
		if err := rows.Scan(&id, &email, &n, &amt); err != nil {
			rows.Close()
			return DailySales{}, err
		}
		c := get(id, email)
		c.RefundCount, c.RefundsMinor = n, amt
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return DailySales{}, err
	}
	for _, c := range byStaff {
		ds.ByCashier = append(ds.ByCashier, *c)
	}
	slices.SortFunc(ds.ByCashier, func(a, b CashierTotal) int { return strings.Compare(a.Email, b.Email) })
	return ds, nil
}
