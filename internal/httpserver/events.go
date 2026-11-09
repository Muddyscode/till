package httpserver

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"

	"github.com/Muddyscode/till/internal/config"
	"github.com/Muddyscode/till/internal/events"
	"github.com/Muddyscode/till/internal/ledger"
	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Event feed: public events derived from audit_events (see internal/events).
// The cursor is an opaque position in the audit sequence; poll with the
// returned next_cursor. Delivery is at-least-once, events are ordered by
// sequence, and an empty page echoes the cursor you sent.

func encodeSeq(n int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte("s" + strconv.FormatInt(n, 10)))
}

func decodeSeq(s string) (int64, bool) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) < 2 || b[0] != 's' {
		return 0, false
	}
	n, err := strconv.ParseInt(string(b[1:]), 10, 64)
	return n, err == nil && n >= 0
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	st := s.staff(r)
	q := r.URL.Query()
	var after int64
	cursor := strings.TrimSpace(q.Get("cursor"))
	if cursor != "" {
		n, ok := decodeSeq(cursor)
		if !ok {
			badQuery(w, "cursor is invalid")
			return
		}
		after = n
	}
	var types []string
	if raw := strings.TrimSpace(q.Get("types")); raw != "" {
		for _, t := range strings.Split(raw, ",") {
			t = strings.TrimSpace(t)
			if !events.ValidType(t) || t == events.StockLow {
				badQuery(w, "unknown or webhook-only event type: "+t)
				return
			}
			types = append(types, t)
		}
	}
	actions := events.ActionsFor(types)
	rows, err := s.store.EventRows(r.Context(), s.store.Pool, &st.MerchantID, after, actions, nil, limitParam(r, 100, 500))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]events.Event, 0, len(rows))
	next := any(nil)
	if cursor != "" {
		next = cursor
	}
	for _, row := range rows {
		next = encodeSeq(row.Seq)
		if ev, ok := events.FromRow(row); ok {
			out = append(out, ev)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out, "next_cursor": next})
}

func endpointBody(e store.WebhookEndpoint, withSecret bool) map[string]any {
	types := e.EventTypes
	if types == nil {
		types = []string{}
	}
	b := map[string]any{"id": e.ID, "url": e.URL, "event_types": types, "created_at": e.CreatedAt}
	if withSecret {
		b["secret"] = e.Secret
	}
	return b
}

type webhookEndpointReq struct {
	URL        string   `json:"url"`
	EventTypes []string `json:"event_types"`
}

func (s *Server) createWebhookEndpoint(w http.ResponseWriter, r *http.Request) {
	raw, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var in webhookEndpointReq
	if err := decodeStrict(raw, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid JSON")
		return
	}
	if err := events.ValidateURL(in.URL, s.cfg.Env == config.EnvProduction); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_webhook_url", err.Error())
		return
	}
	seen := map[string]bool{}
	types := []string{}
	for _, t := range in.EventTypes {
		t = strings.TrimSpace(t)
		if !events.ValidType(t) {
			writeError(w, http.StatusBadRequest, "invalid_event_type", "unknown event type: "+t)
			return
		}
		if !seen[t] {
			seen[t] = true
			types = append(types, t)
		}
	}
	id, err := uuid.NewV7()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	var sec [32]byte
	if _, err := rand.Read(sec[:]); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	actor := s.staff(r)
	var created store.WebhookEndpoint
	err = s.withTx(r.Context(), func(tx pgx.Tx) error {
		existing, err := s.store.ListWebhookEndpoints(r.Context(), tx, actor.MerchantID)
		if err != nil {
			return err
		}
		if len(existing) >= 20 {
			return ledger.Err("too_many_endpoints", "at most 20 webhook endpoints")
		}
		created, err = s.store.CreateWebhookEndpoint(r.Context(), tx, store.WebhookEndpoint{
			ID: id, MerchantID: actor.MerchantID, URL: strings.TrimSpace(in.URL), EventTypes: types,
			Secret: "whsec_" + hex.EncodeToString(sec[:]),
		}, actor.ID)
		return err
	})
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, endpointBody(created, true))
}

func (s *Server) listWebhookEndpoints(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListWebhookEndpoints(r.Context(), s.store.Pool, s.staff(r).MerchantID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		out = append(out, endpointBody(e, false))
	}
	writeJSON(w, http.StatusOK, map[string]any{"webhook_endpoints": out})
}

func (s *Server) deleteWebhookEndpoint(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseID(w, r)
	if !ok {
		return
	}
	err := s.withTx(r.Context(), func(tx pgx.Tx) error {
		return s.store.DeleteWebhookEndpoint(r.Context(), tx, s.staff(r).MerchantID, id)
	})
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseID(w, r)
	if !ok {
		return
	}
	if _, err := s.store.GetWebhookEndpoint(r.Context(), s.store.Pool, s.staff(r).MerchantID, id); err != nil {
		writeLedgerError(w, err)
		return
	}
	list, err := s.store.ListDeliveries(r.Context(), s.store.Pool, id, limitParam(r, 50, 200))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, d := range list {
		out = append(out, map[string]any{
			"id": d.ID, "event_id": d.EventID, "event_type": d.EventType, "status": d.Status,
			"attempts": d.Attempts, "next_attempt_at": d.NextAttemptAt, "last_status": d.LastStatus,
			"last_error": d.LastError, "delivered_at": d.DeliveredAt, "created_at": d.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": out})
}
