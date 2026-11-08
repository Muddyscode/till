package httpserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Muddyscode/till/internal/auth"
	"github.com/Muddyscode/till/internal/checkout"
	"github.com/Muddyscode/till/internal/ledger"
	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func unmatchedBody(u store.Unmatched) map[string]any {
	return map[string]any{
		"id": u.ID, "order_id": u.OrderID, "outlet_id": u.OutletID, "provider": u.Provider,
		"event_id": u.EventID, "amount_minor": u.AmountMinor, "currency": u.Currency,
		"reason": u.Reason, "status": u.Status, "created_at": u.CreatedAt,
		"resolved_at": u.ResolvedAt, "resolved_by": u.ResolvedBy,
		"journal_id": u.JournalID, "resolution_journal_id": u.ResolutionJournalID,
		"settlement_journal_id": u.SettlementJournalID,
	}
}

var unmatchedStatuses = map[string]bool{"open": true, "applied": true, "refund_pending": true, "refunded": true}

func (s *Server) listUnmatched(w http.ResponseWriter, r *http.Request) {
	st := s.staff(r)
	var outletID *uuid.UUID
	if raw := strings.TrimSpace(r.URL.Query().Get("outlet_id")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_order", "outlet_id is invalid")
			return
		}
		if _, ok := s.authorizeOutlet(w, r, id); !ok {
			return
		}
		outletID = &id
	} else if st.Role != auth.RoleAdmin {
		writeError(w, http.StatusBadRequest, "invalid_order", "outlet_id is required")
		return
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status != "" && !unmatchedStatuses[status] {
		writeError(w, http.StatusBadRequest, "invalid_order", "status must be open, applied, refund_pending, or refunded")
		return
	}
	list, err := s.store.ListUnmatched(r.Context(), s.store.Pool, st.MerchantID, outletID, status, limitParam(r, 50, 200))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, u := range list {
		out = append(out, unmatchedBody(u))
	}
	writeJSON(w, http.StatusOK, map[string]any{"unmatched_payments": out})
}

// unmatchedAction runs one resolution step on an unmatched payment under an
// Idempotency-Key. act runs inside the transaction with the row locked.
func (s *Server) unmatchedAction(w http.ResponseWriter, r *http.Request, act func(tx pgx.Tx, merchant store.Merchant, u store.Unmatched, actor store.Staff) (map[string]any, *uuid.UUID, error)) {
	key, ok := requireIdempotencyKey(w, r)
	if !ok {
		return
	}
	id, ok := s.parseID(w, r)
	if !ok {
		return
	}
	raw, ok := s.readBody(w, r)
	if !ok {
		return
	}
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	if err := decodeStrict(raw, &struct{}{}); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid JSON")
		return
	}
	actor := s.staff(r)
	existing, err := s.store.GetUnmatched(r.Context(), s.store.Pool, actor.MerchantID, id)
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	if existing.OutletID != nil {
		if _, ok := s.authorizeOutlet(w, r, *existing.OutletID); !ok {
			return
		}
	} else if actor.Role != auth.RoleAdmin {
		writeError(w, http.StatusForbidden, ledger.CodeForbidden, "only an admin can resolve a payment with no outlet")
		return
	}
	s.finishIdempotent(w, r, actor.MerchantID, key, fingerprint(r, raw), func(tx pgx.Tx) (int, []byte, *uuid.UUID, error) {
		merchant, err := s.store.EnsureBooks(r.Context(), tx, actor.MerchantID)
		if err != nil {
			return 0, nil, nil, err
		}
		u, err := s.store.LockUnmatched(r.Context(), tx, actor.MerchantID, id)
		if err != nil {
			return 0, nil, nil, err
		}
		body, journalID, err := act(tx, merchant, u, actor)
		if err != nil {
			return 0, nil, nil, err
		}
		out, err := json.Marshal(body)
		if err != nil {
			return 0, nil, nil, err
		}
		return http.StatusOK, out, journalID, nil
	})
}

func (s *Server) reloadUnmatched(r *http.Request, tx pgx.Tx, merchantID, id uuid.UUID) (map[string]any, error) {
	u, err := s.store.GetUnmatched(r.Context(), tx, merchantID, id)
	if err != nil {
		return nil, err
	}
	return unmatchedBody(u), nil
}

func (s *Server) applyUnmatched(w http.ResponseWriter, r *http.Request) {
	s.unmatchedAction(w, r, func(tx pgx.Tx, merchant store.Merchant, u store.Unmatched, actor store.Staff) (map[string]any, *uuid.UUID, error) {
		if u.Status != "open" {
			return nil, nil, ledger.Err("unmatched_not_open", "unmatched payment is not open")
		}
		if u.OrderID == nil {
			return nil, nil, ledger.Err(checkout.CodeUnmatchedNotApplicable, "payment has no order to apply to")
		}
		order, err := s.store.LockOrder(r.Context(), tx, merchant.ID, *u.OrderID)
		if err != nil {
			return nil, nil, err
		}
		outlet, err := s.store.GetOutlet(r.Context(), tx, merchant.ID, order.OutletID)
		if err != nil {
			return nil, nil, err
		}
		paid, err := checkout.ApplyUnmatched(r.Context(), tx, s.store, merchant, outlet, u, order, actor.ID, time.Now())
		if err != nil {
			return nil, nil, err
		}
		body, err := s.reloadUnmatched(r, tx, merchant.ID, u.ID)
		if err != nil {
			return nil, nil, err
		}
		body["order"] = orderBody(paid)
		return body, paid.JournalID, nil
	})
}

func (s *Server) refundUnmatched(w http.ResponseWriter, r *http.Request) {
	s.unmatchedAction(w, r, func(tx pgx.Tx, merchant store.Merchant, u store.Unmatched, actor store.Staff) (map[string]any, *uuid.UUID, error) {
		if u.Status != "open" {
			return nil, nil, ledger.Err("unmatched_not_open", "unmatched payment is not open")
		}
		if err := checkout.RefundUnmatched(r.Context(), tx, s.store, merchant, u, actor.ID, time.Now()); err != nil {
			return nil, nil, err
		}
		body, err := s.reloadUnmatched(r, tx, merchant.ID, u.ID)
		return body, nil, err
	})
}

func (s *Server) settleUnmatched(w http.ResponseWriter, r *http.Request) {
	s.unmatchedAction(w, r, func(tx pgx.Tx, merchant store.Merchant, u store.Unmatched, actor store.Staff) (map[string]any, *uuid.UUID, error) {
		if u.Status != "refund_pending" {
			return nil, nil, ledger.Err("unmatched_not_refund_pending", "payment is not waiting for a refund payout")
		}
		if err := checkout.SettleRefund(r.Context(), tx, s.store, merchant, u, actor.ID, time.Now()); err != nil {
			return nil, nil, err
		}
		body, err := s.reloadUnmatched(r, tx, merchant.ID, u.ID)
		return body, nil, err
	})
}
