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

// EnsureAccount returns the merchant account with this code, creating it when
// missing. Safe against two transactions creating it together.
func (s *Store) EnsureAccount(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, code, name string, typ ledger.AccountType, allowNegative bool) (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO accounts (id, merchant_id, code, name, type, currency, allow_negative, status, balance_minor)
		VALUES ($1, $2, $3, $4, $5, 'NGN', $6, 'open', 0)
		ON CONFLICT (merchant_id, code) DO NOTHING
	`, id, merchantID, code, name, typ, allowNegative); err != nil {
		return uuid.Nil, err
	}
	var out uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM accounts WHERE merchant_id = $1 AND code = $2`, merchantID, code).Scan(&out)
	return out, err
}

type Shift struct {
	ID                uuid.UUID
	MerchantID        uuid.UUID
	OutletID          uuid.UUID
	StaffID           uuid.UUID
	Status            string
	OpeningFloatMinor int64
	OpenedAt          time.Time
	ClosedAt          *time.Time
	ClosedBy          *uuid.UUID
	ExpectedMinor     *int64
	CountedMinor      *int64
	VarianceMinor     *int64
	VarianceJournalID *uuid.UUID
	Note              string
	Report            []byte
}

const shiftCols = `id, merchant_id, outlet_id, staff_id, status, opening_float_minor, opened_at, closed_at, closed_by,
	expected_cash_minor, counted_cash_minor, variance_minor, variance_journal_id, note, report`

func scanShift(sc interface{ Scan(...any) error }) (Shift, error) {
	var sh Shift
	err := sc.Scan(&sh.ID, &sh.MerchantID, &sh.OutletID, &sh.StaffID, &sh.Status, &sh.OpeningFloatMinor, &sh.OpenedAt,
		&sh.ClosedAt, &sh.ClosedBy, &sh.ExpectedMinor, &sh.CountedMinor, &sh.VarianceMinor, &sh.VarianceJournalID, &sh.Note, &sh.Report)
	return sh, err
}

// ErrShiftOpen is returned when the staff member already has an open shift at the outlet.
var ErrShiftOpen = ledger.Err("shift_already_open", "this staff member already has an open shift at this outlet")

func (s *Store) OpenShift(ctx context.Context, tx pgx.Tx, merchantID, outletID, staffID uuid.UUID, float int64) (Shift, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Shift{}, err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM shifts WHERE staff_id = $1 AND outlet_id = $2 AND status = 'open')`,
		staffID, outletID).Scan(&exists); err != nil {
		return Shift{}, err
	}
	if exists {
		return Shift{}, ErrShiftOpen
	}
	sh, err := scanShift(tx.QueryRow(ctx, `
		INSERT INTO shifts (id, merchant_id, outlet_id, staff_id, opening_float_minor)
		VALUES ($1,$2,$3,$4,$5) RETURNING `+shiftCols, id, merchantID, outletID, staffID, float))
	var pg interface{ SQLState() string }
	if errors.As(err, &pg) && pg.SQLState() == "23505" {
		return Shift{}, ErrShiftOpen
	}
	return sh, err
}

func (s *Store) GetShift(ctx context.Context, db DBTX, merchantID, id uuid.UUID) (Shift, error) {
	sh, err := scanShift(db.QueryRow(ctx, `SELECT `+shiftCols+` FROM shifts WHERE id = $1 AND merchant_id = $2`, id, merchantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Shift{}, ledger.Err(ledger.CodeNotFound, "shift not found")
	}
	return sh, err
}

// ListShifts returns shifts newest first. Nil filters are ignored.
func (s *Store) ListShifts(ctx context.Context, db DBTX, merchantID uuid.UUID, outletID, staffID *uuid.UUID, status string, limit int) ([]Shift, error) {
	rows, err := db.Query(ctx, `SELECT `+shiftCols+` FROM shifts
		WHERE merchant_id = $1 AND ($2::uuid IS NULL OR outlet_id = $2) AND ($3::uuid IS NULL OR staff_id = $3)
		  AND ($4 = '' OR status = $4)
		ORDER BY opened_at DESC, id DESC LIMIT $5`, merchantID, outletID, staffID, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Shift
	for rows.Next() {
		sh, err := scanShift(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}

// CashFlow is what the shift's staff moved through the outlet's cash account.
type CashFlow struct {
	CashInMinor    int64 // every debit to the cash account
	CashOutMinor   int64 // every credit to the cash account
	CashSalesMinor int64 // debits from sale journals
	RefundsMinor   int64 // credits from refund journals
	SalesCount     int
	RefundCount    int
}

// ShiftCashFlow sums postings to the outlet cash account between from and to
// for journals the staff member posted. Journals carry no actor, but every
// staff action writes an audit event in the same transaction, which gives it
// the same created_at (the transaction's now()). Sales, refunds, wallet
// top-ups, supplier payments and the like are all matched this way.
func (s *Store) ShiftCashFlow(ctx context.Context, db DBTX, merchantID, outletID, staffID, cashAccountID uuid.UUID, from, to time.Time) (CashFlow, error) {
	var f CashFlow
	err := db.QueryRow(ctx, `
		SELECT
		  COALESCE(SUM(p.amount_minor) FILTER (WHERE p.direction = 'debit'), 0),
		  COALESCE(SUM(p.amount_minor) FILTER (WHERE p.direction = 'credit'), 0),
		  COALESCE(SUM(p.amount_minor) FILTER (WHERE p.direction = 'debit' AND j.reverses_journal_id IS NULL AND j.description LIKE 'sale %'), 0),
		  COALESCE(SUM(p.amount_minor) FILTER (WHERE p.direction = 'credit' AND j.description LIKE 'refund %'), 0)
		FROM journals j
		JOIN postings p ON p.journal_id = j.id AND p.account_id = $4
		WHERE j.merchant_id = $1 AND j.created_at >= $5 AND j.created_at <= $6
		  AND EXISTS (
		    SELECT 1 FROM audit_events ae
		    WHERE ae.merchant_id = j.merchant_id AND ae.staff_id = $3 AND ae.outlet_id = $2 AND ae.created_at = j.created_at)
	`, merchantID, outletID, staffID, cashAccountID, from, to).Scan(&f.CashInMinor, &f.CashOutMinor, &f.CashSalesMinor, &f.RefundsMinor)
	if err != nil {
		return f, err
	}
	err = db.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE action = 'order.paid'), COUNT(*) FILTER (WHERE action = 'order.refunded')
		FROM audit_events
		WHERE merchant_id = $1 AND staff_id = $3 AND outlet_id = $2 AND created_at >= $4 AND created_at <= $5
		  AND action IN ('order.paid', 'order.refunded')
	`, merchantID, outletID, staffID, from, to).Scan(&f.SalesCount, &f.RefundCount)
	return f, err
}

// ZReport is the cash-up summary of a shift.
type ZReport struct {
	ShiftID           uuid.UUID  `json:"shift_id"`
	OutletID          uuid.UUID  `json:"outlet_id"`
	StaffID           uuid.UUID  `json:"staff_id"`
	OpenedAt          time.Time  `json:"opened_at"`
	ClosedAt          *time.Time `json:"closed_at"`
	OpeningFloatMinor int64      `json:"opening_float_minor"`
	SalesCount        int        `json:"sales_count"`
	RefundCount       int        `json:"refund_count"`
	CashSalesMinor    int64      `json:"cash_sales_minor"`
	CashInMinor       int64      `json:"cash_in_minor"`
	CashOutMinor      int64      `json:"cash_out_minor"`
	RefundsMinor      int64      `json:"refunds_minor"`
	ExpectedCashMinor int64      `json:"expected_cash_minor"`
	CountedCashMinor  *int64     `json:"counted_cash_minor"`
	VarianceMinor     *int64     `json:"variance_minor"`
}

func buildZ(sh Shift, f CashFlow, closedAt *time.Time) ZReport {
	return ZReport{
		ShiftID: sh.ID, OutletID: sh.OutletID, StaffID: sh.StaffID, OpenedAt: sh.OpenedAt, ClosedAt: closedAt,
		OpeningFloatMinor: sh.OpeningFloatMinor, SalesCount: f.SalesCount, RefundCount: f.RefundCount,
		CashSalesMinor: f.CashSalesMinor, CashInMinor: f.CashInMinor, CashOutMinor: f.CashOutMinor, RefundsMinor: f.RefundsMinor,
		ExpectedCashMinor: sh.OpeningFloatMinor + f.CashInMinor - f.CashOutMinor,
	}
}

// LiveZReport computes the report of a shift as of now (open shifts) or returns
// the one frozen at close.
func (s *Store) LiveZReport(ctx context.Context, db DBTX, sh Shift, cashAccountID uuid.UUID) (ZReport, error) {
	if sh.Status == "closed" && len(sh.Report) > 0 {
		var z ZReport
		if err := json.Unmarshal(sh.Report, &z); err != nil {
			return ZReport{}, err
		}
		return z, nil
	}
	f, err := s.ShiftCashFlow(ctx, db, sh.MerchantID, sh.OutletID, sh.StaffID, cashAccountID, sh.OpenedAt, time.Now().Add(time.Second))
	if err != nil {
		return ZReport{}, err
	}
	return buildZ(sh, f, nil), nil
}

// CloseShift counts the drawer. It locks the shift, derives expected cash up to
// the transaction's now(), posts the over/short journal (Dr/Cr cash against
// 5300) when counted differs, and freezes the Z-report.
func (s *Store) CloseShift(ctx context.Context, tx pgx.Tx, merchantID, shiftID uuid.UUID, outlet Outlet, closer uuid.UUID, counted int64, note string) (Shift, ZReport, error) {
	sh, err := scanShift(tx.QueryRow(ctx, `SELECT `+shiftCols+` FROM shifts WHERE id = $1 AND merchant_id = $2 FOR UPDATE`, shiftID, merchantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Shift{}, ZReport{}, ledger.Err(ledger.CodeNotFound, "shift not found")
	}
	if err != nil {
		return Shift{}, ZReport{}, err
	}
	if sh.Status != "open" {
		return Shift{}, ZReport{}, ledger.Err("already_closed", "shift is already closed")
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		return Shift{}, ZReport{}, err
	}
	f, err := s.ShiftCashFlow(ctx, tx, merchantID, sh.OutletID, sh.StaffID, outlet.CashAccountID, sh.OpenedAt, now)
	if err != nil {
		return Shift{}, ZReport{}, err
	}
	z := buildZ(sh, f, &now)
	variance := counted - z.ExpectedCashMinor
	z.CountedCashMinor, z.VarianceMinor = &counted, &variance
	var vj *uuid.UUID
	if variance != 0 {
		acct, err := s.EnsureAccount(ctx, tx, merchantID, "5300", "Cash over/short", ledger.Expense, true)
		if err != nil {
			return Shift{}, ZReport{}, err
		}
		amount := variance
		debit, credit := outlet.CashAccountID, acct // overage: cash up, income-like credit to 5300
		if variance < 0 {
			amount = -variance
			debit, credit = acct, outlet.CashAccountID
		}
		j, err := ledger.Post(ctx, tx, s.Poster(), ledger.PostRequest{
			MerchantID:  merchantID,
			Description: "cash over/short shift " + sh.ID.String(),
			Lines: []ledger.Line{
				{AccountID: debit, Direction: ledger.Debit, Amount: amount},
				{AccountID: credit, Direction: ledger.Credit, Amount: amount},
			},
		})
		if err != nil {
			return Shift{}, ZReport{}, err
		}
		vj = &j.ID
	}
	report, err := json.Marshal(z)
	if err != nil {
		return Shift{}, ZReport{}, err
	}
	sh, err = scanShift(tx.QueryRow(ctx, `
		UPDATE shifts SET status = 'closed', closed_at = $2, closed_by = $3, expected_cash_minor = $4,
		  counted_cash_minor = $5, variance_minor = $6, variance_journal_id = $7, note = $8, report = $9
		WHERE id = $1 RETURNING `+shiftCols, shiftID, now, closer, z.ExpectedCashMinor, counted, variance, vj, note, report))
	if err != nil {
		return Shift{}, ZReport{}, err
	}
	meta, _ := json.Marshal(map[string]any{
		"shift_id": sh.ID, "shift_staff_id": sh.StaffID, "expected_cash_minor": z.ExpectedCashMinor,
		"counted_cash_minor": counted, "variance_minor": variance,
	})
	mid, cid, oid := merchantID, closer, sh.OutletID
	if err := s.InsertAudit(ctx, tx, AuditEvent{
		MerchantID: &mid, StaffID: &cid, Action: "shift.closed", EntityType: "shift", EntityID: sh.ID, OutletID: &oid, Metadata: meta,
	}); err != nil {
		return Shift{}, ZReport{}, err
	}
	return sh, z, nil
}
