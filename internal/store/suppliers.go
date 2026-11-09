package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Muddyscode/till/internal/ledger"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type Supplier struct {
	ID         uuid.UUID
	MerchantID uuid.UUID
	Name       string
	Phone      string
	Note       string
	CreatedAt  time.Time
}

const supplierCols = `id, merchant_id, name, phone, note, created_at`

func scanSupplier(sc interface{ Scan(...any) error }) (Supplier, error) {
	var s Supplier
	err := sc.Scan(&s.ID, &s.MerchantID, &s.Name, &s.Phone, &s.Note, &s.CreatedAt)
	return s, err
}

func (s *Store) CreateSupplier(ctx context.Context, db DBTX, sp Supplier) (Supplier, error) {
	return scanSupplier(db.QueryRow(ctx, `INSERT INTO suppliers (id, merchant_id, name, phone, note)
		VALUES ($1,$2,$3,$4,$5) RETURNING `+supplierCols, sp.ID, sp.MerchantID, sp.Name, sp.Phone, sp.Note))
}

func (s *Store) GetSupplier(ctx context.Context, db DBTX, merchantID, id uuid.UUID) (Supplier, error) {
	sp, err := scanSupplier(db.QueryRow(ctx, `SELECT `+supplierCols+` FROM suppliers
		WHERE id = $1 AND merchant_id = $2 AND deleted_at IS NULL`, id, merchantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Supplier{}, ledger.Err(ledger.CodeNotFound, "supplier not found")
	}
	return sp, err
}

func (s *Store) ListSuppliers(ctx context.Context, db DBTX, merchantID uuid.UUID) ([]Supplier, error) {
	rows, err := db.Query(ctx, `SELECT `+supplierCols+` FROM suppliers
		WHERE merchant_id = $1 AND deleted_at IS NULL ORDER BY name, id`, merchantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Supplier
	for rows.Next() {
		sp, err := scanSupplier(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

func (s *Store) UpdateSupplier(ctx context.Context, db DBTX, sp Supplier) (Supplier, error) {
	out, err := scanSupplier(db.QueryRow(ctx, `UPDATE suppliers SET name = $3, phone = $4, note = $5
		WHERE id = $1 AND merchant_id = $2 AND deleted_at IS NULL RETURNING `+supplierCols,
		sp.ID, sp.MerchantID, sp.Name, sp.Phone, sp.Note))
	if errors.Is(err, pgx.ErrNoRows) {
		return Supplier{}, ledger.Err(ledger.CodeNotFound, "supplier not found")
	}
	return out, err
}

// DeleteSupplier hides the supplier. Its receipts and payments stay in the books.
func (s *Store) DeleteSupplier(ctx context.Context, db DBTX, merchantID, id uuid.UUID) error {
	tag, err := db.Exec(ctx, `UPDATE suppliers SET deleted_at = now() WHERE id = $1 AND merchant_id = $2 AND deleted_at IS NULL`, id, merchantID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ledger.Err(ledger.CodeNotFound, "supplier not found")
	}
	return nil
}

// RecordSupplierReceipt links a stock receipt to a supplier.
func (s *Store) RecordSupplierReceipt(ctx context.Context, tx pgx.Tx, merchantID, supplierID, outletID, productID, staffID uuid.UUID, sku string, qty int, amount int64) error {
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO supplier_receipts (id, merchant_id, supplier_id, outlet_id, product_id, sku, qty, amount_minor, staff_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id, merchantID, supplierID, outletID, productID, sku, qty, amount, staffID)
	return err
}

type SupplierPayment struct {
	ID          uuid.UUID
	SupplierID  uuid.UUID
	OutletID    uuid.UUID
	AmountMinor int64
	Method      string
	Note        string
	JournalID   uuid.UUID
	CreatedAt   time.Time
}

// PaySupplier posts Dr 2150 Goods received / Cr outlet cash (cash) or Cr 1100
// clearing (bank) and records the payment.
func (s *Store) PaySupplier(ctx context.Context, tx pgx.Tx, m Merchant, outlet Outlet, supplierID, staffID uuid.UUID, amount int64, method, note string) (SupplierPayment, error) {
	if m.GoodsReceivedAccountID == nil || m.ClearingAccountID == nil {
		return SupplierPayment{}, ledger.Err(ledger.CodeInvalidOrder, "merchant books are not set up")
	}
	credit := outlet.CashAccountID
	if method == "bank" {
		credit = *m.ClearingAccountID
	}
	j, err := ledger.Post(ctx, tx, s.Poster(), ledger.PostRequest{
		MerchantID:  m.ID,
		Description: "supplier payment " + supplierID.String(),
		Lines: []ledger.Line{
			{AccountID: *m.GoodsReceivedAccountID, Direction: ledger.Debit, Amount: amount},
			{AccountID: credit, Direction: ledger.Credit, Amount: amount},
		},
	})
	if err != nil {
		return SupplierPayment{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return SupplierPayment{}, err
	}
	p := SupplierPayment{ID: id, SupplierID: supplierID, OutletID: outlet.ID, AmountMinor: amount, Method: method, Note: note, JournalID: j.ID}
	if err := tx.QueryRow(ctx, `INSERT INTO supplier_payments (id, merchant_id, supplier_id, outlet_id, amount_minor, method, note, journal_id, staff_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING created_at`,
		id, m.ID, supplierID, outlet.ID, amount, method, note, j.ID, staffID).Scan(&p.CreatedAt); err != nil {
		return SupplierPayment{}, err
	}
	meta, _ := json.Marshal(map[string]any{"supplier_id": supplierID, "amount_minor": amount, "method": method, "payment_id": id})
	mid, sid, oid := m.ID, staffID, outlet.ID
	if err := s.InsertAudit(ctx, tx, AuditEvent{
		MerchantID: &mid, StaffID: &sid, Action: "supplier.payment_created", EntityType: "supplier", EntityID: supplierID, OutletID: &oid, Metadata: meta,
	}); err != nil {
		return SupplierPayment{}, err
	}
	return p, nil
}

type StatementEntry struct {
	Kind        string // receipt | payment
	At          time.Time
	AmountMinor int64
	Detail      string
	ID          uuid.UUID
}

// SupplierStatement lists receipts and payments oldest first with totals.
func (s *Store) SupplierStatement(ctx context.Context, db DBTX, merchantID, supplierID uuid.UUID) (entries []StatementEntry, received, paid int64, err error) {
	rows, err := db.Query(ctx, `
		SELECT 'receipt', created_at, amount_minor, sku || ' x' || qty::text, id
		FROM supplier_receipts WHERE merchant_id = $1 AND supplier_id = $2
		UNION ALL
		SELECT 'payment', created_at, amount_minor, method || CASE WHEN note = '' THEN '' ELSE ': ' || note END, id
		FROM supplier_payments WHERE merchant_id = $1 AND supplier_id = $2
		ORDER BY 2, 5
	`, merchantID, supplierID)
	if err != nil {
		return nil, 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var e StatementEntry
		if err := rows.Scan(&e.Kind, &e.At, &e.AmountMinor, &e.Detail, &e.ID); err != nil {
			return nil, 0, 0, err
		}
		if e.Kind == "receipt" {
			received += e.AmountMinor
		} else {
			paid += e.AmountMinor
		}
		entries = append(entries, e)
	}
	return entries, received, paid, rows.Err()
}

type VATMonth struct {
	Month         string
	OutputMinor   int64
	ReversedMinor int64
}

// VATByMonth sums postings to the VAT payable account (2100) per Lagos month:
// credits are output VAT collected on sales, debits are reversals (refunds and
// voids) and any remittance journals posted by hand.
func (s *Store) VATByMonth(ctx context.Context, db DBTX, merchantID uuid.UUID, from, to time.Time) ([]VATMonth, error) {
	rows, err := db.Query(ctx, `
		SELECT to_char(date_trunc('month', j.created_at AT TIME ZONE 'Africa/Lagos'), 'YYYY-MM') AS month,
		       COALESCE(SUM(p.amount_minor) FILTER (WHERE p.direction = 'credit'), 0),
		       COALESCE(SUM(p.amount_minor) FILTER (WHERE p.direction = 'debit'), 0)
		FROM postings p
		JOIN journals j ON j.id = p.journal_id
		JOIN accounts a ON a.id = p.account_id
		WHERE a.merchant_id = $1 AND a.code = '2100' AND j.created_at >= $2 AND j.created_at < $3
		GROUP BY 1 ORDER BY 1
	`, merchantID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VATMonth
	for rows.Next() {
		var m VATMonth
		if err := rows.Scan(&m.Month, &m.OutputMinor, &m.ReversedMinor); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
