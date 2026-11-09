package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/Muddyscode/till/internal/ledger"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Inter-outlet transfers and stock counts. Transfers move quantity only: the
// inventory account (1200) is merchant-wide and average cost is per product,
// so nothing changes in the general ledger. While a transfer is in transit the
// units are in neither outlet; Invariants adds their value back.

type TransferLine struct {
	ProductID uuid.UUID
	SKU       string
	Quantity  int
}

type Transfer struct {
	ID           uuid.UUID
	MerchantID   uuid.UUID
	FromOutletID uuid.UUID
	ToOutletID   uuid.UUID
	Status       string
	Note         string
	CreatedBy    uuid.UUID
	CreatedAt    time.Time
	ReceivedBy   *uuid.UUID
	ReceivedAt   *time.Time
	CancelledBy  *uuid.UUID
	CancelledAt  *time.Time
	Lines        []TransferLine
}

const transferCols = `id, merchant_id, from_outlet_id, to_outlet_id, status, note, created_by, created_at,
	received_by, received_at, cancelled_by, cancelled_at`

func scanTransfer(sc interface{ Scan(...any) error }) (Transfer, error) {
	var t Transfer
	err := sc.Scan(&t.ID, &t.MerchantID, &t.FromOutletID, &t.ToOutletID, &t.Status, &t.Note, &t.CreatedBy, &t.CreatedAt,
		&t.ReceivedBy, &t.ReceivedAt, &t.CancelledBy, &t.CancelledAt)
	return t, err
}

func (s *Store) transferLines(ctx context.Context, db DBTX, id uuid.UUID) ([]TransferLine, error) {
	rows, err := db.Query(ctx, `SELECT product_id, sku, quantity FROM stock_transfer_lines WHERE transfer_id = $1 ORDER BY product_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TransferLine
	for rows.Next() {
		var l TransferLine
		if err := rows.Scan(&l.ProductID, &l.SKU, &l.Quantity); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) GetTransfer(ctx context.Context, db DBTX, merchantID, id uuid.UUID) (Transfer, error) {
	t, err := scanTransfer(db.QueryRow(ctx, `SELECT `+transferCols+` FROM stock_transfers WHERE id = $1 AND merchant_id = $2`, id, merchantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Transfer{}, ledger.Err(ledger.CodeNotFound, "transfer not found")
	}
	if err != nil {
		return Transfer{}, err
	}
	t.Lines, err = s.transferLines(ctx, db, id)
	return t, err
}

// ListTransfers lists newest first. A non-nil staffID limits the list to
// transfers touching an outlet that staff member is assigned to.
func (s *Store) ListTransfers(ctx context.Context, db DBTX, merchantID uuid.UUID, staffID *uuid.UUID, status string, limit int) ([]Transfer, error) {
	rows, err := db.Query(ctx, `SELECT `+transferCols+` FROM stock_transfers
		WHERE merchant_id = $1 AND ($2 = '' OR status = $2)
		  AND ($3::uuid IS NULL
		       OR from_outlet_id IN (SELECT outlet_id FROM staff_outlets WHERE staff_id = $3)
		       OR to_outlet_id IN (SELECT outlet_id FROM staff_outlets WHERE staff_id = $3))
		ORDER BY created_at DESC, id DESC LIMIT $4`, merchantID, status, staffID, limit)
	if err != nil {
		return nil, err
	}
	var out []Transfer
	for rows.Next() {
		t, err := scanTransfer(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Lines, err = s.transferLines(ctx, db, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func lineIDs(lines []TransferLine) []uuid.UUID {
	ids := make([]uuid.UUID, len(lines))
	for i, l := range lines {
		ids[i] = l.ProductID
	}
	return ids
}

func (s *Store) transferAudit(ctx context.Context, tx pgx.Tx, t Transfer, action string, staff uuid.UUID, outlet uuid.UUID) error {
	skus := make([]string, len(t.Lines))
	lines := make([]map[string]any, len(t.Lines))
	for i, l := range t.Lines {
		skus[i] = l.SKU
		lines[i] = map[string]any{"sku": l.SKU, "quantity": l.Quantity}
	}
	meta, _ := json.Marshal(map[string]any{
		"transfer_id": t.ID, "from_outlet_id": t.FromOutletID, "to_outlet_id": t.ToOutletID, "skus": skus, "lines": lines,
	})
	mid, sid, oid := t.MerchantID, staff, outlet
	return s.InsertAudit(ctx, tx, AuditEvent{
		MerchantID: &mid, StaffID: &sid, Action: action, EntityType: "transfer", EntityID: t.ID, OutletID: &oid, Metadata: meta,
	})
}

// CreateTransfer deducts the lines from the source outlet (never below reserved)
// and records the transfer as in_transit. Lines must have distinct products.
func (s *Store) CreateTransfer(ctx context.Context, tx pgx.Tx, t Transfer) (Transfer, error) {
	ids := lineIDs(t.Lines)
	if _, err := s.LockProducts(ctx, tx, t.MerchantID, ids); err != nil {
		return Transfer{}, err
	}
	if err := s.LockStock(ctx, tx, t.FromOutletID, ids); err != nil {
		return Transfer{}, err
	}
	for _, l := range sortedLines(t.Lines) {
		if err := s.DeductStock(ctx, tx, t.FromOutletID, l.ProductID, l.Quantity); err != nil {
			return Transfer{}, err
		}
	}
	t.Status = "in_transit"
	row := tx.QueryRow(ctx, `INSERT INTO stock_transfers (id, merchant_id, from_outlet_id, to_outlet_id, status, note, created_by)
		VALUES ($1,$2,$3,$4,'in_transit',$5,$6) RETURNING `+transferCols,
		t.ID, t.MerchantID, t.FromOutletID, t.ToOutletID, t.Note, t.CreatedBy)
	created, err := scanTransfer(row)
	if err != nil {
		return Transfer{}, err
	}
	for _, l := range t.Lines {
		if _, err := tx.Exec(ctx, `INSERT INTO stock_transfer_lines (transfer_id, product_id, sku, quantity) VALUES ($1,$2,$3,$4)`,
			t.ID, l.ProductID, l.SKU, l.Quantity); err != nil {
			return Transfer{}, err
		}
	}
	created.Lines = t.Lines
	if err := s.transferAudit(ctx, tx, created, "transfer.sent", t.CreatedBy, t.FromOutletID); err != nil {
		return Transfer{}, err
	}
	return created, nil
}

func sortedLines(in []TransferLine) []TransferLine {
	out := slices.Clone(in)
	slices.SortFunc(out, func(a, b TransferLine) int { return bytes.Compare(a.ProductID[:], b.ProductID[:]) })
	return out
}

// SettleTransfer finishes an in-transit transfer: receive adds the units to the
// destination, cancel returns them to the source. A transfer that is not in
// transit is a transfer_not_in_transit conflict.
func (s *Store) SettleTransfer(ctx context.Context, tx pgx.Tx, merchantID, id, staff uuid.UUID, receive bool) (Transfer, error) {
	t, err := scanTransfer(tx.QueryRow(ctx, `SELECT `+transferCols+` FROM stock_transfers WHERE id = $1 AND merchant_id = $2 FOR UPDATE`, id, merchantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Transfer{}, ledger.Err(ledger.CodeNotFound, "transfer not found")
	}
	if err != nil {
		return Transfer{}, err
	}
	if t.Status != "in_transit" {
		return Transfer{}, ledger.Err("transfer_not_in_transit", "transfer is "+t.Status+", not in transit")
	}
	if t.Lines, err = s.transferLines(ctx, tx, id); err != nil {
		return Transfer{}, err
	}
	if _, err := s.LockProducts(ctx, tx, merchantID, lineIDs(t.Lines)); err != nil {
		return Transfer{}, err
	}
	target, action, outlet := t.FromOutletID, "transfer.cancelled", t.FromOutletID
	set := `status = 'cancelled', cancelled_by = $2, cancelled_at = now()`
	if receive {
		target, action, outlet = t.ToOutletID, "transfer.received", t.ToOutletID
		set = `status = 'received', received_by = $2, received_at = now()`
	}
	if err := s.LockStock(ctx, tx, target, lineIDs(t.Lines)); err != nil {
		return Transfer{}, err
	}
	for _, l := range sortedLines(t.Lines) {
		if _, err := s.ReceiveStock(ctx, tx, target, l.ProductID, l.Quantity); err != nil {
			return Transfer{}, err
		}
	}
	updated, err := scanTransfer(tx.QueryRow(ctx, `UPDATE stock_transfers SET `+set+` WHERE id = $1 RETURNING `+transferCols, id, staff))
	if err != nil {
		return Transfer{}, err
	}
	updated.Lines = t.Lines
	if err := s.transferAudit(ctx, tx, updated, action, staff, outlet); err != nil {
		return Transfer{}, err
	}
	return updated, nil
}

// InTransitValue is the cost of units sent but not yet received or cancelled.
func (s *Store) InTransitValue(ctx context.Context, db DBTX, merchantID uuid.UUID) (int64, error) {
	var v int64
	err := db.QueryRow(ctx, `
		SELECT COALESCE(SUM(l.quantity::bigint * p.avg_cost_minor), 0)
		FROM stock_transfers t
		JOIN stock_transfer_lines l ON l.transfer_id = t.id
		JOIN products p ON p.id = l.product_id
		WHERE t.merchant_id = $1 AND t.status = 'in_transit'
	`, merchantID).Scan(&v)
	return v, err
}

// InTransitUnits counts the units currently in transit (the inventory check's tolerance).
func (s *Store) InTransitUnits(ctx context.Context, db DBTX, merchantID uuid.UUID) (int64, error) {
	var v int64
	err := db.QueryRow(ctx, `
		SELECT COALESCE(SUM(l.quantity), 0)
		FROM stock_transfers t JOIN stock_transfer_lines l ON l.transfer_id = t.id
		WHERE t.merchant_id = $1 AND t.status = 'in_transit'
	`, merchantID).Scan(&v)
	return v, err
}

// ---- stock counts ----

type CountLine struct {
	ProductID          uuid.UUID
	SKU                string
	ExpectedOnHand     int
	Counted            int
	Variance           int
	UnitCostMinor      int64
	VarianceValueMinor int64
}

type StockCount struct {
	ID         uuid.UUID
	MerchantID uuid.UUID
	OutletID   uuid.UUID
	Note       string
	CreatedBy  uuid.UUID
	CreatedAt  time.Time
	Lines      []CountLine
}

// EnsureStockRow makes sure a stock_levels row exists (0 on hand) so a count
// can adjust it.
func (s *Store) EnsureStockRow(ctx context.Context, tx pgx.Tx, outletID, productID uuid.UUID) error {
	_, err := tx.Exec(ctx, `INSERT INTO stock_levels (outlet_id, product_id, on_hand, reserved) VALUES ($1,$2,0,0)
		ON CONFLICT (outlet_id, product_id) DO NOTHING`, outletID, productID)
	return err
}

// StockLevel reads on_hand and reserved; call after LockStock.
func (s *Store) StockLevel(ctx context.Context, tx pgx.Tx, outletID, productID uuid.UUID) (onHand, reserved int, err error) {
	err = tx.QueryRow(ctx, `SELECT on_hand, reserved FROM stock_levels WHERE outlet_id = $1 AND product_id = $2`, outletID, productID).Scan(&onHand, &reserved)
	return
}

func (s *Store) InsertCount(ctx context.Context, tx pgx.Tx, c StockCount) (StockCount, error) {
	err := tx.QueryRow(ctx, `INSERT INTO stock_counts (id, merchant_id, outlet_id, note, created_by)
		VALUES ($1,$2,$3,$4,$5) RETURNING created_at`, c.ID, c.MerchantID, c.OutletID, c.Note, c.CreatedBy).Scan(&c.CreatedAt)
	if err != nil {
		return c, err
	}
	for _, l := range c.Lines {
		if _, err := tx.Exec(ctx, `INSERT INTO stock_count_lines
			(count_id, product_id, sku, expected_on_hand, counted, variance, unit_cost_minor, variance_value_minor)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			c.ID, l.ProductID, l.SKU, l.ExpectedOnHand, l.Counted, l.Variance, l.UnitCostMinor, l.VarianceValueMinor); err != nil {
			return c, err
		}
	}
	return c, nil
}

func (s *Store) GetCount(ctx context.Context, db DBTX, merchantID, id uuid.UUID) (StockCount, error) {
	var c StockCount
	err := db.QueryRow(ctx, `SELECT id, merchant_id, outlet_id, note, created_by, created_at FROM stock_counts
		WHERE id = $1 AND merchant_id = $2`, id, merchantID).Scan(&c.ID, &c.MerchantID, &c.OutletID, &c.Note, &c.CreatedBy, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ledger.Err(ledger.CodeNotFound, "stock count not found")
	}
	if err != nil {
		return c, err
	}
	rows, err := db.Query(ctx, `SELECT product_id, sku, expected_on_hand, counted, variance, unit_cost_minor, variance_value_minor
		FROM stock_count_lines WHERE count_id = $1 ORDER BY sku`, id)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var l CountLine
		if err := rows.Scan(&l.ProductID, &l.SKU, &l.ExpectedOnHand, &l.Counted, &l.Variance, &l.UnitCostMinor, &l.VarianceValueMinor); err != nil {
			return c, err
		}
		c.Lines = append(c.Lines, l)
	}
	return c, rows.Err()
}
