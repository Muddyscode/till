// Package events turns the audit log into a public event feed and delivers
// those events to merchant webhook endpoints.
package events

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
)

// actionTypes maps internal audit actions to public event types. Audit actions
// that are not listed stay internal.
var actionTypes = map[string]string{
	"order.paid":               "order.paid",
	"order.refunded":           "order.refunded",
	"order.voided":             "order.voided",
	"reservation.released":     "order.expired",
	"payment.unmatched":        "payment.unmatched",
	"stock.received":           "stock.received",
	"stock.adjusted":           "stock.adjusted",
	"stock.counted":            "stock.counted",
	"shift.opened":             "shift.opened",
	"shift.closed":             "shift.closed",
	"transfer.sent":            "transfer.sent",
	"transfer.received":        "transfer.received",
	"transfer.cancelled":       "transfer.cancelled",
	"supplier.payment_created": "supplier.payment",
}

// StockLow is delivered to webhooks only. The worker computes it after an
// order, adjustment, count or transfer leaves a product at or below its
// reorder level; it is not part of the polled feed.
const StockLow = "stock.low"

// PublicType returns the public event type for an audit action.
func PublicType(action string) (string, bool) {
	t, ok := actionTypes[action]
	return t, ok
}

// ValidType reports whether t is a type clients may subscribe to or filter by.
func ValidType(t string) bool {
	if t == StockLow {
		return true
	}
	for _, v := range actionTypes {
		if v == t {
			return true
		}
	}
	return false
}

// AllTypes lists every public type, sorted.
func AllTypes() []string {
	seen := map[string]bool{StockLow: true}
	for _, v := range actionTypes {
		seen[v] = true
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// ActionsFor returns the audit actions behind the given public types. An empty
// types list means every feed action.
func ActionsFor(types []string) []string {
	want := map[string]bool{}
	for _, t := range types {
		want[t] = true
	}
	var out []string
	for a, t := range actionTypes {
		if len(want) == 0 || want[t] {
			out = append(out, a)
		}
	}
	sort.Strings(out)
	return out
}

type Event struct {
	ID         uuid.UUID      `json:"id"`
	Type       string         `json:"type"`
	OutletID   *uuid.UUID     `json:"outlet_id"`
	OccurredAt time.Time      `json:"occurred_at"`
	Data       map[string]any `json:"data"`
}

// FromRow builds the public event for an audit row; ok is false when the
// action is internal.
func FromRow(r store.EventRow) (Event, bool) {
	t, ok := PublicType(r.Action)
	if !ok {
		return Event{}, false
	}
	var meta any
	if len(r.Metadata) > 0 {
		_ = json.Unmarshal(r.Metadata, &meta)
	}
	if meta == nil {
		meta = map[string]any{}
	}
	data := map[string]any{
		"entity_type": r.EntityType,
		"entity_id":   r.EntityID,
		"staff_id":    r.StaffID,
		"details":     meta,
	}
	return Event{ID: r.ID, Type: t, OutletID: r.OutletID, OccurredAt: r.CreatedAt.UTC(), Data: data}, true
}
