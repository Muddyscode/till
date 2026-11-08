package httpserver

import (
	"net/http"
	"strings"
	"time"

	"github.com/Muddyscode/till/internal/auth"
	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
)

// Cursor-paginated list handlers for orders, journals/transfers and audit.
// Responses carry next_cursor (null on the last page); pass it back as ?cursor=.

var lagos = time.FixedZone("WAT", 3600) // Nigeria has no daylight saving

func badQuery(w http.ResponseWriter, msg string) {
	writeError(w, http.StatusBadRequest, "invalid_query", msg)
}

func cursorParam(w http.ResponseWriter, r *http.Request) (*store.Cursor, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("cursor"))
	if raw == "" {
		return nil, true
	}
	c, err := store.DecodeCursor(raw)
	if err != nil {
		badQuery(w, "cursor is invalid")
		return nil, false
	}
	return &c, true
}

func nextCursorValue(c *store.Cursor) any {
	if c == nil {
		return nil
	}
	return c.Encode()
}

// parseTimeParam reads RFC 3339 or a YYYY-MM-DD date (start of that day, Lagos time).
// endOfDay makes a bare date mean the end of that day, so ?to=2026-11-08 includes the 8th.
func parseTimeParam(raw string, endOfDay bool) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, true
	}
	if t, err := time.ParseInLocation("2006-01-02", raw, lagos); err == nil {
		if endOfDay {
			t = t.AddDate(0, 0, 1)
		}
		return t, true
	}
	return time.Time{}, false
}

func (s *Server) outletScope(w http.ResponseWriter, r *http.Request) (*uuid.UUID, bool) {
	st := s.staff(r)
	if raw := strings.TrimSpace(r.URL.Query().Get("outlet_id")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_order", "outlet_id is invalid")
			return nil, false
		}
		if _, ok := s.authorizeOutlet(w, r, id); !ok {
			return nil, false
		}
		return &id, true
	}
	if st.Role != auth.RoleAdmin {
		writeError(w, http.StatusBadRequest, "invalid_order", "outlet_id is required")
		return nil, false
	}
	return nil, true
}

func (s *Server) listOrdersPage(w http.ResponseWriter, r *http.Request) {
	st := s.staff(r)
	q := r.URL.Query()
	f := store.OrderFilter{}
	var ok bool
	if f.OutletID, ok = s.outletScope(w, r); !ok {
		return
	}
	if f.Status = strings.TrimSpace(q.Get("status")); f.Status != "" && !store.ValidOrderStatus(f.Status) {
		badQuery(w, "status must be pending_payment, paid, expired, or refunded")
		return
	}
	f.Reference = strings.TrimSpace(q.Get("reference"))
	if raw := strings.TrimSpace(q.Get("customer_id")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			badQuery(w, "customer_id is invalid")
			return
		}
		f.CustomerID = &id
	}
	if raw := strings.TrimSpace(q.Get("from")); raw != "" {
		t, ok := parseTimeParam(raw, false)
		if !ok {
			badQuery(w, "from must be RFC 3339 or YYYY-MM-DD")
			return
		}
		f.From = &t
	}
	if raw := strings.TrimSpace(q.Get("to")); raw != "" {
		t, ok := parseTimeParam(raw, true)
		if !ok {
			badQuery(w, "to must be RFC 3339 or YYYY-MM-DD")
			return
		}
		f.To = &t
	}
	cur, ok := cursorParam(w, r)
	if !ok {
		return
	}
	list, next, err := s.store.ListOrdersPage(r.Context(), s.store.Pool, st.MerchantID, f, cur, limitParam(r, 50, 200))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, o := range list {
		out = append(out, orderBody(o))
	}
	writeJSON(w, http.StatusOK, map[string]any{"orders": out, "next_cursor": nextCursorValue(next)})
}

func (s *Server) listJournalsPaged(twoLegged bool) http.HandlerFunc {
	key := "journals"
	if twoLegged {
		key = "transfers"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		cur, ok := cursorParam(w, r)
		if !ok {
			return
		}
		js, next, err := s.store.ListJournalsPage(r.Context(), s.store.Pool, s.staff(r).MerchantID, cur, limitParam(r, 50, 200), twoLegged)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		out := make([]journalJSON, 0, len(js))
		for _, j := range js {
			out = append(out, jJSON(j))
		}
		writeJSON(w, http.StatusOK, map[string]any{key: out, "next_cursor": nextCursorValue(next)})
	}
}

func (s *Server) listAuditPage(w http.ResponseWriter, r *http.Request) {
	st := s.staff(r)
	outletID, ok := s.outletScope(w, r)
	if !ok {
		return
	}
	cur, ok := cursorParam(w, r)
	if !ok {
		return
	}
	list, next, err := s.store.ListAuditPage(r.Context(), s.store.Pool, st.MerchantID, outletID, cur, limitParam(r, 50, 200))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, ev := range list {
		out = append(out, map[string]any{
			"id": ev.ID, "staff_id": ev.StaffID, "action": ev.Action,
			"entity_type": ev.EntityType, "entity_id": ev.EntityID,
			"outlet_id": ev.OutletID, "metadata": jsonRaw(ev.Metadata), "created_at": ev.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out, "next_cursor": nextCursorValue(next)})
}
