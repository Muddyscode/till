package store

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/Muddyscode/till/internal/ledger"
	"github.com/google/uuid"
)

// Cursor is a keyset position in a (created_at DESC, id DESC) listing.
type Cursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// Encode returns the opaque cursor string clients pass back as ?cursor=.
func (c Cursor) Encode() string {
	raw := c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

var ErrBadCursor = errors.New("invalid cursor")

func DecodeCursor(s string) (Cursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, ErrBadCursor
	}
	ts, id, ok := strings.Cut(string(b), "|")
	if !ok {
		return Cursor{}, ErrBadCursor
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return Cursor{}, ErrBadCursor
	}
	u, err := uuid.Parse(id)
	if err != nil {
		return Cursor{}, ErrBadCursor
	}
	return Cursor{CreatedAt: t, ID: u}, nil
}

func cursorArgs(c *Cursor) (*time.Time, *uuid.UUID) {
	if c == nil {
		return nil, nil
	}
	return &c.CreatedAt, &c.ID
}

type OrderFilter struct {
	OutletID   *uuid.UUID
	Status     string
	Reference  string
	CustomerID *uuid.UUID
	From, To   *time.Time // created_at >= From, created_at < To
}

var orderStatuses = map[string]bool{
	"pending_payment": true, "paid": true, "expired": true, "refunded": true, "partially_refunded": true, "voided": true,
}

func ValidOrderStatus(s string) bool { return orderStatuses[s] }

// ListOrdersPage returns up to limit orders, newest first, and the cursor of the next page (nil at the end).
func (s *Store) ListOrdersPage(ctx context.Context, db DBTX, merchantID uuid.UUID, f OrderFilter, cur *Cursor, limit int) ([]Order, *Cursor, error) {
	ct, ci := cursorArgs(cur)
	rows, err := db.Query(ctx, `
		SELECT `+orderSelect+`
		FROM orders
		WHERE merchant_id = $1
		  AND ($2::uuid IS NULL OR outlet_id = $2)
		  AND ($3 = '' OR status = $3)
		  AND ($4 = '' OR reference = $4)
		  AND ($5::uuid IS NULL OR customer_id = $5)
		  AND ($6::timestamptz IS NULL OR created_at >= $6)
		  AND ($7::timestamptz IS NULL OR created_at < $7)
		  AND ($8::timestamptz IS NULL OR (created_at, id) < ($8::timestamptz, $9::uuid))
		ORDER BY created_at DESC, id DESC
		LIMIT $10
	`, merchantID, f.OutletID, f.Status, f.Reference, f.CustomerID, f.From, f.To, ct, ci, limit+1)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	rows.Close()
	var next *Cursor
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	out, err = s.loadLines(ctx, db, out)
	return out, next, err
}

// ListJournalsPage returns up to limit journals, newest first, with keyset pagination.
func (s *Store) ListJournalsPage(ctx context.Context, db DBTX, merchantID uuid.UUID, cur *Cursor, limit int) ([]ledger.Journal, *Cursor, error) {
	ct, ci := cursorArgs(cur)
	rows, err := db.Query(ctx, `
		SELECT j.id, j.merchant_id, j.status, j.description, COALESCE(j.idempotency_key, ''), j.reverses_journal_id, j.created_at
		FROM journals j
		WHERE j.merchant_id = $1
		  AND ($2::timestamptz IS NULL OR (j.created_at, j.id) < ($2::timestamptz, $3::uuid))
		ORDER BY j.created_at DESC, j.id DESC LIMIT $4`, merchantID, ct, ci, limit+1)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var js []ledger.Journal
	for rows.Next() {
		var j ledger.Journal
		if err := rows.Scan(&j.ID, &j.MerchantID, &j.Status, &j.Description, &j.IdempotencyKey, &j.ReversesJournalID, &j.CreatedAt); err != nil {
			return nil, nil, err
		}
		js = append(js, j)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	rows.Close()
	var next *Cursor
	if len(js) > limit {
		js = js[:limit]
		last := js[len(js)-1]
		next = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	ids := make([]uuid.UUID, 0, len(js))
	for _, j := range js {
		ids = append(ids, j.ID)
	}
	posts, err := s.ListPostings(ctx, db, ids)
	if err != nil {
		return nil, nil, err
	}
	for i := range js {
		js[i].Postings = posts[js[i].ID]
	}
	return js, next, nil
}

// ListAuditPage is ListAudit with keyset pagination.
func (s *Store) ListAuditPage(ctx context.Context, db DBTX, merchantID uuid.UUID, outletID *uuid.UUID, cur *Cursor, limit int) ([]AuditEvent, *Cursor, error) {
	ct, ci := cursorArgs(cur)
	rows, err := db.Query(ctx, `
		SELECT id, merchant_id, staff_id, action, entity_type, entity_id, outlet_id, metadata, created_at
		FROM audit_events
		WHERE merchant_id = $1 AND ($2::uuid IS NULL OR outlet_id = $2)
		  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3::timestamptz, $4::uuid))
		ORDER BY created_at DESC, id DESC
		LIMIT $5
	`, merchantID, outletID, ct, ci, limit+1)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []AuditEvent
	for rows.Next() {
		var ev AuditEvent
		if err := rows.Scan(&ev.ID, &ev.MerchantID, &ev.StaffID, &ev.Action, &ev.EntityType, &ev.EntityID, &ev.OutletID, &ev.Metadata, &ev.CreatedAt); err != nil {
			return nil, nil, err
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var next *Cursor
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return out, next, nil
}
