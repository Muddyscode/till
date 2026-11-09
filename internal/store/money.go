package store

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"time"

	"github.com/Muddyscode/till/internal/ledger"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PurgeIdempotency deletes idempotency keys older than maxAge. A client that
// retries after that window gets a fresh request, which is the documented TTL.
func (s *Store) PurgeIdempotency(ctx context.Context, maxAge time.Duration) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `
		DELETE FROM idempotency_keys
		WHERE created_at < now() - make_interval(secs => $1)
	`, maxAge.Seconds())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// LockProducts locks the products in id order and returns the locked rows.
// Every money path takes product locks before stock locks, in this order.
func (s *Store) LockProducts(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]Product, error) {
	sorted := append([]uuid.UUID(nil), ids...)
	slices.SortFunc(sorted, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	sorted = slices.Compact(sorted)
	rows, err := tx.Query(ctx, `
		SELECT `+productCols+` FROM products
		WHERE merchant_id = $1 AND id = ANY($2)
		ORDER BY id
		FOR UPDATE
	`, merchantID, sorted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]Product{}
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			return nil, err
		}
		out[p.ID] = p
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) != len(sorted) {
		return nil, ledger.Err(ledger.CodeNotFound, "product not found")
	}
	return out, nil
}

type ProviderEvent struct {
	Outcome        string
	OrderID        *uuid.UUID
	ResponseStatus *int
	ResponseBody   []byte
}

// ClaimProviderEvent records that (provider, eventID) is being handled. When
// another request already holds or finished the event, claimed is false and
// the stored result is returned. The insert waits on a concurrent claimer.
func (s *Store) ClaimProviderEvent(ctx context.Context, tx pgx.Tx, provider, eventID string) (ProviderEvent, bool, error) {
	var got string
	err := tx.QueryRow(ctx, `
		INSERT INTO provider_events (provider, event_id, outcome) VALUES ($1, $2, 'processing')
		ON CONFLICT (provider, event_id) DO NOTHING
		RETURNING provider
	`, provider, eventID).Scan(&got)
	if err == nil {
		return ProviderEvent{}, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ProviderEvent{}, false, err
	}
	var ev ProviderEvent
	err = tx.QueryRow(ctx, `
		SELECT outcome, order_id, response_status, response_body
		FROM provider_events WHERE provider = $1 AND event_id = $2
	`, provider, eventID).Scan(&ev.Outcome, &ev.OrderID, &ev.ResponseStatus, &ev.ResponseBody)
	return ev, false, err
}

func (s *Store) FinishProviderEvent(ctx context.Context, tx pgx.Tx, provider, eventID string, merchantID, orderID *uuid.UUID, outcome string, status int, body []byte) error {
	_, err := tx.Exec(ctx, `
		UPDATE provider_events
		SET merchant_id = $3, order_id = $4, outcome = $5, response_status = $6, response_body = $7
		WHERE provider = $1 AND event_id = $2
	`, provider, eventID, merchantID, orderID, outcome, status, body)
	return err
}

type Unmatched struct {
	ID                  uuid.UUID
	MerchantID          uuid.UUID
	OrderID             *uuid.UUID
	OutletID            *uuid.UUID
	Provider            string
	EventID             string
	AmountMinor         int64
	Currency            string
	Reason              string
	Status              string
	CreatedAt           time.Time
	ResolvedAt          *time.Time
	ResolvedBy          *uuid.UUID
	JournalID           *uuid.UUID
	ResolutionJournalID *uuid.UUID
	SettlementJournalID *uuid.UUID
}

const unmatchedCols = `id, merchant_id, order_id, outlet_id, provider, event_id, amount_minor, currency, reason, status,
	created_at, resolved_at, resolved_by, journal_id, resolution_journal_id, settlement_journal_id`

func scanUnmatched(sc interface{ Scan(...any) error }) (Unmatched, error) {
	var u Unmatched
	err := sc.Scan(&u.ID, &u.MerchantID, &u.OrderID, &u.OutletID, &u.Provider, &u.EventID, &u.AmountMinor, &u.Currency,
		&u.Reason, &u.Status, &u.CreatedAt, &u.ResolvedAt, &u.ResolvedBy, &u.JournalID, &u.ResolutionJournalID, &u.SettlementJournalID)
	return u, err
}

func (s *Store) InsertUnmatched(ctx context.Context, tx pgx.Tx, u Unmatched) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO unmatched_payments (id, merchant_id, order_id, outlet_id, provider, event_id, amount_minor, currency,
			reason, status, created_at, resolved_at, resolved_by, journal_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
	`, u.ID, u.MerchantID, u.OrderID, u.OutletID, u.Provider, u.EventID, u.AmountMinor, u.Currency,
		u.Reason, u.Status, u.CreatedAt, u.ResolvedAt, u.ResolvedBy, u.JournalID)
	return err
}

func (s *Store) GetUnmatched(ctx context.Context, db DBTX, merchantID, id uuid.UUID) (Unmatched, error) {
	u, err := scanUnmatched(db.QueryRow(ctx, `SELECT `+unmatchedCols+` FROM unmatched_payments WHERE id = $1 AND merchant_id = $2`, id, merchantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Unmatched{}, ledger.Err(ledger.CodeNotFound, "unmatched payment not found")
	}
	return u, err
}

func (s *Store) LockUnmatched(ctx context.Context, tx pgx.Tx, merchantID, id uuid.UUID) (Unmatched, error) {
	u, err := scanUnmatched(tx.QueryRow(ctx, `SELECT `+unmatchedCols+` FROM unmatched_payments WHERE id = $1 AND merchant_id = $2 FOR UPDATE`, id, merchantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Unmatched{}, ledger.Err(ledger.CodeNotFound, "unmatched payment not found")
	}
	return u, err
}

func (s *Store) ListUnmatched(ctx context.Context, db DBTX, merchantID uuid.UUID, outletID *uuid.UUID, status string, limit int) ([]Unmatched, error) {
	rows, err := db.Query(ctx, `
		SELECT `+unmatchedCols+` FROM unmatched_payments
		WHERE merchant_id = $1 AND ($2::uuid IS NULL OR outlet_id = $2) AND ($3 = '' OR status = $3)
		ORDER BY created_at DESC, id DESC
		LIMIT $4
	`, merchantID, outletID, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Unmatched{}
	for rows.Next() {
		u, err := scanUnmatched(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// TransitionUnmatched moves a row from one status to the next and records who did it.
func (s *Store) TransitionUnmatched(ctx context.Context, tx pgx.Tx, id uuid.UUID, from, to string, by uuid.UUID, at time.Time, resolutionJournal, settlementJournal *uuid.UUID) error {
	tag, err := tx.Exec(ctx, `
		UPDATE unmatched_payments
		SET status = $3, resolved_by = $4, resolved_at = $5,
		    resolution_journal_id = COALESCE($6, resolution_journal_id),
		    settlement_journal_id = COALESCE($7, settlement_journal_id)
		WHERE id = $1 AND status = $2
	`, id, from, to, by, at, resolutionJournal, settlementJournal)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ledger.Err("unmatched_not_"+from, "unmatched payment is not "+from)
	}
	return nil
}

type Check struct {
	Name     string `json:"name"`
	Holds    bool   `json:"holds"`
	Expected int64  `json:"expected"`
	Actual   int64  `json:"actual"`
	// The inventory check carries a tolerance instead of requiring equality.
	ResidueMinor   *int64 `json:"residue_minor,omitempty"`
	ToleranceMinor *int64 `json:"tolerance_minor,omitempty"`
}

// Invariants reads the accounting identity and the cross-table checks from one
// snapshot so concurrent sales cannot make a healthy ledger look broken.
func (s *Store) Invariants(ctx context.Context, merchantID uuid.UUID) (Identity, []Check, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Identity{}, nil, err
	}
	defer tx.Rollback(ctx)
	idn, err := s.Identity(ctx, tx, merchantID)
	if err != nil {
		return Identity{}, nil, err
	}
	bal := func(code string) (int64, error) {
		var n int64
		err := tx.QueryRow(ctx, `SELECT balance_minor FROM accounts WHERE merchant_id = $1 AND code = $2`, merchantID, code).Scan(&n)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return n, err
	}
	sumStatus := func(status string) (int64, error) {
		var n int64
		err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(amount_minor), 0) FROM unmatched_payments WHERE merchant_id = $1 AND status = $2`, merchantID, status).Scan(&n)
		return n, err
	}
	var checks []Check
	eq := func(name string, expected, actual int64) {
		checks = append(checks, Check{Name: name, Holds: expected == actual, Expected: expected, Actual: actual})
	}
	wallet, err := bal("2200")
	if err != nil {
		return Identity{}, nil, err
	}
	custSum, err := s.WalletBalanceSum(ctx, tx, merchantID)
	if err != nil {
		return Identity{}, nil, err
	}
	eq("wallet_matches_customer_balances", custSum, wallet)
	suspense, err := bal("2300")
	if err != nil {
		return Identity{}, nil, err
	}
	open, err := sumStatus("open")
	if err != nil {
		return Identity{}, nil, err
	}
	eq("suspense_matches_open_unmatched", open, suspense)
	refundsPayable, err := bal("2310")
	if err != nil {
		return Identity{}, nil, err
	}
	pending, err := sumStatus("refund_pending")
	if err != nil {
		return Identity{}, nil, err
	}
	eq("refunds_payable_matches_pending_refunds", pending, refundsPayable)
	deposits, err := bal("2400")
	if err != nil {
		return Identity{}, nil, err
	}
	var held int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(p.amount_minor), 0)
		FROM payments p JOIN orders o ON o.id = p.order_id
		WHERE o.merchant_id = $1 AND p.status = 'held'
	`, merchantID).Scan(&held); err != nil {
		return Identity{}, nil, err
	}
	eq("deposits_match_held_tenders", held, deposits)

	var value, units int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(s.on_hand::bigint * p.avg_cost_minor), 0), COALESCE(SUM(s.on_hand), 0)
		FROM stock_levels s JOIN products p ON p.id = s.product_id
		WHERE p.merchant_id = $1
	`, merchantID).Scan(&value, &units); err != nil {
		return Identity{}, nil, err
	}
	// Units sent between outlets and not yet received are off the shelves but still inventory.
	transitValue, err := s.InTransitValue(ctx, tx, merchantID)
	if err != nil {
		return Identity{}, nil, err
	}
	transitUnits, err := s.InTransitUnits(ctx, tx, merchantID)
	if err != nil {
		return Identity{}, nil, err
	}
	value += transitValue
	units += transitUnits
	inv, err := bal("1200")
	if err != nil {
		return Identity{}, nil, err
	}
	residue := inv - value
	if residue < 0 {
		residue = -residue
	}
	checks = append(checks, Check{
		Name: "inventory_matches_stock_value", Holds: residue <= units, Expected: value, Actual: inv,
		ResidueMinor: &residue, ToleranceMinor: &units,
	})
	return idn, checks, nil
}
