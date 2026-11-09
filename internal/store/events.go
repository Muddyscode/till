package store

import (
	"context"
	"errors"
	"time"

	"github.com/Muddyscode/till/internal/ledger"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// EventRow is one audit_events row with its feed sequence number.
type EventRow struct {
	Seq        int64
	ID         uuid.UUID
	MerchantID uuid.UUID
	StaffID    *uuid.UUID
	Action     string
	EntityType string
	EntityID   uuid.UUID
	OutletID   *uuid.UUID
	Metadata   []byte
	CreatedAt  time.Time
}

// EventRows returns audit rows with seq > afterSeq in seq order. merchantID nil
// means every merchant (the delivery worker); before, when set, hides rows
// whose transaction started after it (the worker's commit-lag guard).
func (s *Store) EventRows(ctx context.Context, db DBTX, merchantID *uuid.UUID, afterSeq int64, actions []string, before *time.Time, limit int) ([]EventRow, error) {
	if actions == nil {
		actions = []string{}
	}
	rows, err := db.Query(ctx, `
		SELECT seq, id, merchant_id, staff_id, action, entity_type, entity_id, outlet_id, metadata, created_at
		FROM audit_events
		WHERE merchant_id IS NOT NULL
		  AND ($1::uuid IS NULL OR merchant_id = $1)
		  AND seq > $2
		  AND (cardinality($3::text[]) = 0 OR action = ANY($3))
		  AND ($4::timestamptz IS NULL OR created_at <= $4)
		ORDER BY seq
		LIMIT $5
	`, merchantID, afterSeq, actions, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventRow
	for rows.Next() {
		var e EventRow
		if err := rows.Scan(&e.Seq, &e.ID, &e.MerchantID, &e.StaffID, &e.Action, &e.EntityType, &e.EntityID, &e.OutletID, &e.Metadata, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

type WebhookEndpoint struct {
	ID         uuid.UUID
	MerchantID uuid.UUID
	URL        string
	EventTypes []string
	Secret     string
	CreatedAt  time.Time
}

const endpointCols = `id, merchant_id, url, event_types, secret, created_at`

func scanEndpoint(sc interface{ Scan(...any) error }) (WebhookEndpoint, error) {
	var e WebhookEndpoint
	err := sc.Scan(&e.ID, &e.MerchantID, &e.URL, &e.EventTypes, &e.Secret, &e.CreatedAt)
	return e, err
}

func (s *Store) CreateWebhookEndpoint(ctx context.Context, tx pgx.Tx, e WebhookEndpoint, createdBy uuid.UUID) (WebhookEndpoint, error) {
	if e.EventTypes == nil {
		e.EventTypes = []string{}
	}
	return scanEndpoint(tx.QueryRow(ctx, `
		INSERT INTO webhook_endpoints (id, merchant_id, url, event_types, secret, created_by)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING `+endpointCols,
		e.ID, e.MerchantID, e.URL, e.EventTypes, e.Secret, createdBy))
}

func (s *Store) ListWebhookEndpoints(ctx context.Context, db DBTX, merchantID uuid.UUID) ([]WebhookEndpoint, error) {
	rows, err := db.Query(ctx, `SELECT `+endpointCols+` FROM webhook_endpoints
		WHERE merchant_id = $1 AND deleted_at IS NULL ORDER BY created_at DESC`, merchantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WebhookEndpoint
	for rows.Next() {
		e, err := scanEndpoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) GetWebhookEndpoint(ctx context.Context, db DBTX, merchantID, id uuid.UUID) (WebhookEndpoint, error) {
	e, err := scanEndpoint(db.QueryRow(ctx, `SELECT `+endpointCols+` FROM webhook_endpoints
		WHERE id = $1 AND merchant_id = $2 AND deleted_at IS NULL`, id, merchantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return WebhookEndpoint{}, ledger.Err(ledger.CodeNotFound, "webhook endpoint not found")
	}
	return e, err
}

// DeleteWebhookEndpoint soft-deletes the endpoint and stops its pending deliveries.
func (s *Store) DeleteWebhookEndpoint(ctx context.Context, tx pgx.Tx, merchantID, id uuid.UUID) error {
	tag, err := tx.Exec(ctx, `UPDATE webhook_endpoints SET deleted_at = now()
		WHERE id = $1 AND merchant_id = $2 AND deleted_at IS NULL`, id, merchantID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ledger.Err(ledger.CodeNotFound, "webhook endpoint not found")
	}
	_, err = tx.Exec(ctx, `UPDATE webhook_deliveries SET status = 'failed', last_error = 'endpoint deleted'
		WHERE endpoint_id = $1 AND status = 'pending'`, id)
	return err
}

type Delivery struct {
	ID            uuid.UUID
	EndpointID    uuid.UUID
	EventID       uuid.UUID
	EventType     string
	Payload       []byte
	Status        string
	Attempts      int
	NextAttemptAt time.Time
	LastStatus    *int
	LastError     *string
	DeliveredAt   *time.Time
	CreatedAt     time.Time
}

const deliveryCols = `id, endpoint_id, event_id, event_type, payload, status, attempts, next_attempt_at, last_status, last_error, delivered_at, created_at`

func scanDelivery(sc interface{ Scan(...any) error }) (Delivery, error) {
	var d Delivery
	err := sc.Scan(&d.ID, &d.EndpointID, &d.EventID, &d.EventType, &d.Payload, &d.Status, &d.Attempts,
		&d.NextAttemptAt, &d.LastStatus, &d.LastError, &d.DeliveredAt, &d.CreatedAt)
	return d, err
}

func (s *Store) ListDeliveries(ctx context.Context, db DBTX, endpointID uuid.UUID, limit int) ([]Delivery, error) {
	rows, err := db.Query(ctx, `SELECT `+deliveryCols+` FROM webhook_deliveries
		WHERE endpoint_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2`, endpointID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// LockCursor locks and returns the named cursor so two instances fan out one batch at a time.
func (s *Store) LockCursor(ctx context.Context, tx pgx.Tx, name string) (int64, error) {
	var seq int64
	err := tx.QueryRow(ctx, `SELECT seq FROM event_cursors WHERE name = $1 FOR UPDATE`, name).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := tx.Exec(ctx, `INSERT INTO event_cursors (name, seq) VALUES ($1, 0) ON CONFLICT DO NOTHING`, name); err != nil {
			return 0, err
		}
		err = tx.QueryRow(ctx, `SELECT seq FROM event_cursors WHERE name = $1 FOR UPDATE`, name).Scan(&seq)
	}
	return seq, err
}

func (s *Store) SetCursor(ctx context.Context, tx pgx.Tx, name string, seq int64) error {
	_, err := tx.Exec(ctx, `UPDATE event_cursors SET seq = $2 WHERE name = $1`, name, seq)
	return err
}

func (s *Store) InsertDelivery(ctx context.Context, tx pgx.Tx, d Delivery) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO webhook_deliveries (id, endpoint_id, event_id, event_type, payload)
		VALUES ($1,$2,$3,$4,$5) ON CONFLICT (endpoint_id, event_id) DO NOTHING
	`, d.ID, d.EndpointID, d.EventID, d.EventType, d.Payload)
	return err
}

type DueDelivery struct {
	Delivery
	URL    string
	Secret string
}

// ClaimDueDeliveries leases up to limit due deliveries: attempts is bumped and
// next_attempt_at pushed out by lease, so a crashed worker's rows come back.
// SKIP LOCKED lets several instances claim disjoint rows.
func (s *Store) ClaimDueDeliveries(ctx context.Context, limit int, lease time.Duration) ([]DueDelivery, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH c AS (
			SELECT d.id FROM webhook_deliveries d
			WHERE d.status = 'pending' AND d.next_attempt_at <= now()
			  AND EXISTS (SELECT 1 FROM webhook_endpoints e WHERE e.id = d.endpoint_id AND e.deleted_at IS NULL)
			ORDER BY d.next_attempt_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		), u AS (
			UPDATE webhook_deliveries d
			SET attempts = d.attempts + 1, next_attempt_at = now() + make_interval(secs => $2)
			FROM c WHERE d.id = c.id
			RETURNING d.id, d.endpoint_id, d.event_id, d.event_type, d.payload, d.status, d.attempts,
			          d.next_attempt_at, d.last_status, d.last_error, d.delivered_at, d.created_at
		)
		SELECT u.id, u.endpoint_id, u.event_id, u.event_type, u.payload, u.status, u.attempts,
		       u.next_attempt_at, u.last_status, u.last_error, u.delivered_at, u.created_at, e.url, e.secret
		FROM u JOIN webhook_endpoints e ON e.id = u.endpoint_id
	`, limit, lease.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DueDelivery
	for rows.Next() {
		var d DueDelivery
		if err := rows.Scan(&d.ID, &d.EndpointID, &d.EventID, &d.EventType, &d.Payload, &d.Status, &d.Attempts,
			&d.NextAttemptAt, &d.LastStatus, &d.LastError, &d.DeliveredAt, &d.CreatedAt, &d.URL, &d.Secret); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// FinishDelivery records an attempt. ok marks it delivered; otherwise the row
// is failed for good when final, or retried at next.
func (s *Store) FinishDelivery(ctx context.Context, id uuid.UUID, ok, final bool, next time.Time, status int, errMsg string) error {
	var st *int
	if status != 0 {
		st = &status
	}
	var msg *string
	if errMsg != "" {
		msg = &errMsg
	}
	switch {
	case ok:
		_, err := s.Pool.Exec(ctx, `UPDATE webhook_deliveries SET status = 'delivered', delivered_at = now(),
			last_status = $2, last_error = NULL WHERE id = $1`, id, st)
		return err
	case final:
		_, err := s.Pool.Exec(ctx, `UPDATE webhook_deliveries SET status = 'failed', last_status = $2, last_error = $3 WHERE id = $1`, id, st, msg)
		return err
	default:
		_, err := s.Pool.Exec(ctx, `UPDATE webhook_deliveries SET next_attempt_at = $2, last_status = $3, last_error = $4 WHERE id = $1`, id, next, st, msg)
		return err
	}
}

// OrderSKUs lists the SKUs on an order.
func (s *Store) OrderSKUs(ctx context.Context, db DBTX, orderID uuid.UUID) ([]string, error) {
	rows, err := db.Query(ctx, `SELECT sku FROM order_lines WHERE order_id = $1`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sku string
		if err := rows.Scan(&sku); err != nil {
			return nil, err
		}
		out = append(out, sku)
	}
	return out, rows.Err()
}

type LowStockRow struct {
	SKU          string
	Available    int
	OnHand       int
	ReorderLevel int
}

// LowStockAmong returns the given SKUs at an outlet whose available quantity is
// at or below a positive reorder level.
func (s *Store) LowStockAmong(ctx context.Context, db DBTX, merchantID, outletID uuid.UUID, skus []string) ([]LowStockRow, error) {
	rows, err := db.Query(ctx, `
		SELECT p.sku, sl.on_hand - sl.reserved, sl.on_hand, p.reorder_level
		FROM stock_levels sl JOIN products p ON p.id = sl.product_id
		WHERE sl.outlet_id = $1 AND p.merchant_id = $2 AND p.sku = ANY($3)
		  AND p.reorder_level > 0 AND sl.on_hand - sl.reserved <= p.reorder_level
		ORDER BY p.sku
	`, outletID, merchantID, skus)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LowStockRow
	for rows.Next() {
		var r LowStockRow
		if err := rows.Scan(&r.SKU, &r.Available, &r.OnHand, &r.ReorderLevel); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
