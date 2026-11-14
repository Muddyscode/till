package store

import (
	"context"
	"errors"
	"time"

	"github.com/Muddyscode/till/internal/ledger"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// System account codes introduced with payment connections.
const (
	BankAccountCode = "1050" // money in the merchant's own bank account
	FeesAccountCode = "5100" // what providers keep from payments
)

// Connection is one place a merchant receives non-cash money. See migration 0008.
type Connection struct {
	ID                uuid.UUID
	MerchantID        uuid.UUID
	OutletID          *uuid.UUID
	Code              string
	Name              string
	Provider          string
	Secret            string
	AccountNumber     string
	ClearingAccountID uuid.UUID
	Active            bool
	CreatedAt         time.Time
	// BalanceMinor is the clearing balance: what the provider still holds.
	BalanceMinor int64
}

const connectionSelect = `SELECT c.id, c.merchant_id, c.outlet_id, c.code, c.name, c.provider, c.secret, c.account_number,
	c.clearing_account_id, c.active, c.created_at, a.balance_minor
	FROM payment_connections c JOIN accounts a ON a.id = c.clearing_account_id`

func scanConnection(sc interface{ Scan(...any) error }) (Connection, error) {
	var c Connection
	err := sc.Scan(&c.ID, &c.MerchantID, &c.OutletID, &c.Code, &c.Name, &c.Provider, &c.Secret, &c.AccountNumber,
		&c.ClearingAccountID, &c.Active, &c.CreatedAt, &c.BalanceMinor)
	if errors.Is(err, pgx.ErrNoRows) {
		return Connection{}, ledger.Err(ledger.CodeNotFound, "payment connection not found")
	}
	return c, err
}

// CreateConnection creates the connection and its clearing account 1100-CODE.
// The account may go negative: a provider nets refunds against later payouts.
func (s *Store) CreateConnection(ctx context.Context, tx pgx.Tx, c Connection, createdBy *uuid.UUID) (Connection, error) {
	acct, err := s.EnsureAccount(ctx, tx, c.MerchantID, "1100-"+c.Code, "Clearing: "+c.Name, ledger.Asset, true)
	if err != nil {
		return Connection{}, err
	}
	// ON CONFLICT waits for a concurrent creator of the same code and then
	// inserts nothing, so the loser gets a clean conflict instead of an error.
	tag, err := tx.Exec(ctx, `
		INSERT INTO payment_connections (id, merchant_id, outlet_id, code, name, provider, secret, account_number, clearing_account_id, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (merchant_id, code) DO NOTHING
	`, c.ID, c.MerchantID, c.OutletID, c.Code, c.Name, c.Provider, c.Secret, c.AccountNumber, acct, createdBy)
	if err != nil {
		return Connection{}, err
	}
	if tag.RowsAffected() != 1 {
		return Connection{}, ledger.Err("connection_code_taken", "a payment connection with this code already exists")
	}
	return s.GetConnection(ctx, tx, c.MerchantID, c.ID)
}

func (s *Store) GetConnection(ctx context.Context, db DBTX, merchantID, id uuid.UUID) (Connection, error) {
	return scanConnection(db.QueryRow(ctx, connectionSelect+` WHERE c.id = $1 AND c.merchant_id = $2`, id, merchantID))
}

// ConnectionByID finds a connection from its webhook URL, before any merchant is known.
func (s *Store) ConnectionByID(ctx context.Context, db DBTX, id uuid.UUID) (Connection, error) {
	return scanConnection(db.QueryRow(ctx, connectionSelect+` WHERE c.id = $1`, id))
}

func (s *Store) ListConnections(ctx context.Context, db DBTX, merchantID uuid.UUID) ([]Connection, error) {
	rows, err := db.Query(ctx, connectionSelect+` WHERE c.merchant_id = $1 ORDER BY c.created_at, c.id`, merchantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Connection{}
	for rows.Next() {
		c, err := scanConnection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) SetConnectionActive(ctx context.Context, tx pgx.Tx, merchantID, id uuid.UUID, active bool) error {
	tag, err := tx.Exec(ctx, `UPDATE payment_connections SET active = $3 WHERE id = $1 AND merchant_id = $2`, id, merchantID, active)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ledger.Err(ledger.CodeNotFound, "payment connection not found")
	}
	return nil
}

// ClearingAccountIDs returns every connection clearing account of the merchant.
// They are control accounts: only payment flows may post to them.
func (s *Store) ClearingAccountIDs(ctx context.Context, db DBTX, merchantID uuid.UUID) (map[uuid.UUID]string, error) {
	rows, err := db.Query(ctx, `SELECT clearing_account_id, code FROM payment_connections WHERE merchant_id = $1`, merchantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]string{}
	for rows.Next() {
		var id uuid.UUID
		var code string
		if err := rows.Scan(&id, &code); err != nil {
			return nil, err
		}
		out[id] = "1100-" + code
	}
	return out, rows.Err()
}

// ClaimEvent records that this connection's event is being handled. When it
// was already claimed, claimed is false and the stored response is returned.
// The insert waits on a concurrent claimer.
func (s *Store) ClaimEvent(ctx context.Context, tx pgx.Tx, c Connection, eventID string, details []byte) (ProviderEvent, bool, error) {
	var got string
	err := tx.QueryRow(ctx, `
		INSERT INTO provider_events (provider, event_id, connection_id, merchant_id, outcome, details)
		VALUES ($1, $2, $3, $4, 'processing', $5)
		ON CONFLICT ((COALESCE(connection_id::text, provider)), event_id) DO NOTHING
		RETURNING provider
	`, c.Provider, eventID, c.ID, c.MerchantID, details).Scan(&got)
	if err == nil {
		return ProviderEvent{}, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ProviderEvent{}, false, err
	}
	var ev ProviderEvent
	err = tx.QueryRow(ctx, `
		SELECT outcome, order_id, response_status, response_body
		FROM provider_events WHERE connection_id = $1 AND event_id = $2
	`, c.ID, eventID).Scan(&ev.Outcome, &ev.OrderID, &ev.ResponseStatus, &ev.ResponseBody)
	return ev, false, err
}

func (s *Store) FinishEvent(ctx context.Context, tx pgx.Tx, connectionID uuid.UUID, eventID string, orderID *uuid.UUID, outcome string, status int, body []byte) error {
	_, err := tx.Exec(ctx, `
		UPDATE provider_events SET order_id = $3, outcome = $4, response_status = $5, response_body = $6
		WHERE connection_id = $1 AND event_id = $2
	`, connectionID, eventID, orderID, outcome, status, body)
	return err
}

// OrderByReference finds a merchant's order by its reference.
func (s *Store) OrderByReference(ctx context.Context, db DBTX, merchantID uuid.UUID, reference string) (uuid.UUID, bool, error) {
	var id uuid.UUID
	err := db.QueryRow(ctx, `SELECT id FROM orders WHERE merchant_id = $1 AND reference = $2`, merchantID, reference).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	return id, err == nil, err
}

// OrdersAwaiting returns up to two pending orders that are waiting for exactly
// this amount, oldest first. One result is a match; two means it is ambiguous.
// With an outlet, only that outlet's orders are considered.
func (s *Store) OrdersAwaiting(ctx context.Context, db DBTX, merchantID uuid.UUID, outletID *uuid.UUID, amount int64, now time.Time) ([]uuid.UUID, error) {
	rows, err := db.Query(ctx, `
		SELECT o.id FROM orders o JOIN payments p ON p.order_id = o.id AND p.status = 'pending'
		WHERE o.merchant_id = $1 AND o.status = 'pending_payment' AND p.amount_minor = $2
		  AND ($3::uuid IS NULL OR o.outlet_id = $3) AND (o.expires_at IS NULL OR o.expires_at > $4)
		ORDER BY o.created_at, o.id LIMIT 2
	`, merchantID, amount, outletID, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// OrderConnection returns the clearing account of the connection that paid an
// order's awaited tender, and that connection's id. ok is false for orders
// paid before connections existed.
func (s *Store) OrderConnection(ctx context.Context, db DBTX, orderID uuid.UUID) (connectionID uuid.UUID, ok bool, err error) {
	err = db.QueryRow(ctx, `
		SELECT connection_id FROM payments WHERE order_id = $1 AND connection_id IS NOT NULL LIMIT 1
	`, orderID).Scan(&connectionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	return connectionID, err == nil, err
}

type Settlement struct {
	ID           uuid.UUID
	ConnectionID uuid.UUID
	AmountMinor  int64
	Reference    string
	JournalID    uuid.UUID
	CreatedAt    time.Time
}

func (s *Store) InsertSettlement(ctx context.Context, tx pgx.Tx, merchantID, staffID uuid.UUID, st Settlement) (Settlement, error) {
	err := tx.QueryRow(ctx, `
		INSERT INTO payment_settlements (id, merchant_id, connection_id, amount_minor, reference, journal_id, staff_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING created_at
	`, st.ID, merchantID, st.ConnectionID, st.AmountMinor, st.Reference, st.JournalID, staffID).Scan(&st.CreatedAt)
	return st, err
}

// SetUnmatchedOrder links an unmatched payment to the order it was applied to.
func (s *Store) SetUnmatchedOrder(ctx context.Context, tx pgx.Tx, id, orderID, outletID uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE unmatched_payments SET order_id = $2, outlet_id = $3 WHERE id = $1`, id, orderID, outletID)
	return err
}
