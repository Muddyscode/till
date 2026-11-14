package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/Muddyscode/till/internal/checkout"
	"github.com/Muddyscode/till/internal/ledger"
	"github.com/Muddyscode/till/internal/payments"
	"github.com/Muddyscode/till/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// paymentWebhook is where a provider (or any system speaking the till format)
// reports a payment: POST /v1/webhooks/{connection_id}. The connection picks
// the merchant, the adapter and the secret before the body is trusted.
func (s *Server) paymentWebhook(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		if isTooLarge(err) {
			s.tooLarge(w)
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_json", "could not read body")
		return
	}
	// An unknown, disabled or wrongly signed connection all answer 401, so the
	// URL cannot be used to find out which connections exist.
	deny := func() { writeError(w, http.StatusUnauthorized, ledger.CodeUnauthorized, "invalid signature") }
	id, err := uuid.Parse(chi.URLParam(r, "connection_id"))
	if err != nil {
		deny()
		return
	}
	conn, err := s.store.ConnectionByID(r.Context(), s.store.Pool, id)
	if err != nil {
		if le := ledger.AsError(err); le != nil && le.Code == ledger.CodeNotFound {
			deny()
			return
		}
		s.internalError(w, r, err)
		return
	}
	adapter, ok := payments.For(conn.Provider)
	if !ok || !conn.Active || !adapter.Verify(conn.Secret, r.Header, raw, time.Now()) {
		deny()
		return
	}
	ev, err := adapter.Parse(raw)
	if errors.Is(err, payments.ErrIgnored) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "event"})
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_payment", err.Error())
		return
	}
	s.writeIngest(w, r, conn, ev, nil)
}

// recordPayment lets a manager or admin (or an API key with that role) report
// a payment on a connection in the till format, for money Till cannot hear
// about by webhook: a transfer checked in the bank app, a card terminal that
// is not integrated. The staff member is on the audit trail.
func (s *Server) recordPayment(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseID(w, r)
	if !ok {
		return
	}
	raw, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var in payments.Standard
	if err := decodeStrict(raw, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid JSON")
		return
	}
	ev, err := in.Event()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_payment", err.Error())
		return
	}
	actor := s.staff(r)
	conn, err := s.store.GetConnection(r.Context(), s.store.Pool, actor.MerchantID, id)
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	if !conn.Active {
		writeError(w, http.StatusConflict, "connection_inactive", "payment connection is disabled")
		return
	}
	if conn.OutletID != nil {
		if _, ok := s.authorizeOutlet(w, r, *conn.OutletID); !ok {
			return
		}
	}
	s.writeIngest(w, r, conn, ev, &actor.ID)
}

func (s *Server) writeIngest(w http.ResponseWriter, r *http.Request, conn store.Connection, ev payments.Event, staffID *uuid.UUID) {
	result, err := s.ingest(r.Context(), conn, ev, staffID)
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if result.replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	w.WriteHeader(result.status)
	_, _ = w.Write(append(result.body, '\n'))
}

// ingest is the one path every confirmed payment takes, whoever reported it.
//
//  1. The event is claimed once per connection; a repeat replays the first answer.
//  2. The order is found by its reference, or else as the only pending order
//     waiting for exactly this amount (at the connection's outlet, when it has one).
//  3. A payment that fits its order pays it. Anything else (no order, two
//     candidates, late, wrong amount, wrong currency) is booked to suspense as
//     an unmatched payment for a manager. Money is never dropped.
func (s *Server) ingest(ctx context.Context, conn store.Connection, ev payments.Event, staffID *uuid.UUID) (txResult, error) {
	details, err := json.Marshal(map[string]any{
		"reference": ev.Reference, "method": ev.Method, "fee_minor": ev.FeeMinor, "paid_at": ev.PaidAt,
		"payer_name": ev.PayerName, "payer_account": ev.PayerAccount, "payer_bank": ev.PayerBank,
		"account_number": ev.AccountNumber, "session_id": ev.SessionID, "recorded_by": staffID,
	})
	if err != nil {
		return txResult{}, err
	}
	var result txResult
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		result = txResult{}
		now := time.Now()
		prev, claimed, err := s.store.ClaimEvent(ctx, tx, conn, ev.ID, details)
		if err != nil {
			return err
		}
		if !claimed {
			result = txResult{status: http.StatusOK, body: []byte(`{"status":"duplicate"}`), replayed: true}
			if prev.ResponseStatus != nil && len(prev.ResponseBody) > 0 {
				result.status, result.body = *prev.ResponseStatus, prev.ResponseBody
			}
			return nil
		}
		finish := func(orderID *uuid.UUID, outcome string, v any) error {
			body, err := json.Marshal(v)
			if err != nil {
				return err
			}
			if err := s.store.FinishEvent(ctx, tx, conn.ID, ev.ID, orderID, outcome, http.StatusOK, body); err != nil {
				return err
			}
			result = txResult{status: http.StatusOK, body: body}
			return nil
		}
		if ev.Status != payments.StatusSuccess {
			// A failed attempt moves no money. A reversal of confirmed money needs
			// a person: it is put on the audit trail and the event feed, not posted.
			if ev.Status == payments.StatusReversed {
				mid := conn.MerchantID
				if err := s.store.InsertAudit(ctx, tx, store.AuditEvent{
					MerchantID: &mid, StaffID: staffID, Action: "payment.reversed", EntityType: "payment_connection",
					EntityID: conn.ID, OutletID: conn.OutletID, Metadata: details,
				}); err != nil {
					return err
				}
			}
			return finish(nil, "ignored:"+ev.Status, map[string]string{"status": "ignored", "reason": "status_" + ev.Status})
		}
		merchant, err := s.store.EnsureBooks(ctx, tx, conn.MerchantID)
		if err != nil {
			return err
		}
		// The fee is posted last, after the order's product and stock rows are
		// locked, so this path takes locks in the same order as a sale.
		fee := func() error {
			if ev.FeeMinor == 0 {
				return nil
			}
			return checkout.PostFee(ctx, tx, s.store, conn, ev.ID, ev.FeeMinor)
		}

		var order *store.Order
		reason := "no_matching_order"
		orderID, found, err := uuid.Nil, false, error(nil)
		if ev.Reference != "" {
			if orderID, found, err = s.store.OrderByReference(ctx, tx, conn.MerchantID, ev.Reference); err != nil {
				return err
			}
		}
		if !found {
			ids, err := s.store.OrdersAwaiting(ctx, tx, conn.MerchantID, conn.OutletID, ev.AmountMinor, now)
			if err != nil {
				return err
			}
			switch len(ids) {
			case 1:
				orderID, found = ids[0], true
			case 2:
				reason = "ambiguous_amount"
			}
		}
		if found {
			locked, err := s.store.LockOrder(ctx, tx, conn.MerchantID, orderID)
			if err != nil {
				return err
			}
			order = &locked
			// A split order only expects its awaited portion from the provider.
			due := locked.TotalMinor
			if locked.Status == "pending_payment" {
				if n, ok, err := s.store.PendingAwaitedMinor(ctx, tx, locked.ID); err != nil {
					return err
				} else if ok {
					due = n
				}
			}
			switch {
			case locked.Status == "expired":
				reason = "order_expired"
			case locked.Status == "voided":
				reason = "order_voided"
			case locked.Status == "paid" || locked.Status == "partially_refunded":
				reason = "order_already_paid"
			case locked.Status == "refunded":
				reason = "order_refunded"
			case ev.Currency != "NGN":
				reason = "currency_mismatch"
			case ev.AmountMinor != due:
				reason = "amount_mismatch"
			default:
				reason = ""
			}
		} else if ev.Currency != "NGN" {
			reason = "currency_mismatch"
		}

		if reason != "" {
			u, err := checkout.RecordUnmatched(ctx, tx, s.store, checkout.UnmatchedInput{
				Merchant: merchant, Order: order, Connection: conn, EventID: ev.ID,
				Currency: ev.Currency, Reason: reason, Amount: ev.AmountMinor, Now: now,
				Details: details, StaffID: staffID,
			})
			if err != nil {
				return err
			}
			if err := fee(); err != nil {
				return err
			}
			var oid *uuid.UUID
			if order != nil {
				oid = &order.ID
			}
			return finish(oid, "unmatched:"+reason, map[string]string{
				"status": "unmatched", "reason": reason, "unmatched_payment_id": u.ID.String(),
			})
		}
		outlet, err := s.store.GetOutlet(ctx, tx, order.MerchantID, order.OutletID)
		if err != nil {
			return err
		}
		confirmed, err := checkout.ConfirmAwaited(ctx, tx, s.store, checkout.ConfirmInput{
			Merchant: merchant, Outlet: outlet, Order: *order, Now: now,
			IdempotencyKey: "wh:" + conn.ID.String() + ":" + ev.ID, ProviderEventID: ev.ID,
			DebitAccountID: conn.ClearingAccountID, ConnectionID: &conn.ID, Provider: conn.Provider,
			StaffID: staffID,
		})
		if err != nil {
			return err
		}
		if err := fee(); err != nil {
			return err
		}
		return finish(&order.ID, "paid", orderBody(confirmed))
	})
	return result, err
}
