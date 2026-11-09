package httpserver

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Muddyscode/till/internal/checkout"
	"github.com/Muddyscode/till/internal/ledger"
	"github.com/Muddyscode/till/internal/money"
	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const providerPaystack = "paystack"

type paystackBody struct {
	Event string `json:"event"`
	Data  struct {
		ID        json.RawMessage `json:"id"`
		Reference string          `json:"reference"`
		Amount    json.RawMessage `json:"amount"`
		Currency  string          `json:"currency"`
		Status    string          `json:"status"`
	} `json:"data"`
}

func SignPaystack(secret string, body []byte) string {
	m := hmac.New(sha512.New, []byte(secret))
	_, _ = m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func signatureOK(secret, header string, body []byte) bool {
	got, err := hex.DecodeString(strings.TrimSpace(header))
	if err != nil || len(got) == 0 {
		return false
	}
	m := hmac.New(sha512.New, []byte(secret))
	_, _ = m.Write(body)
	return hmac.Equal(m.Sum(nil), got)
}

func (s *Server) paystackWebhook(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "could not read body")
		return
	}
	if !signatureOK(s.cfg.PaystackSecret, r.Header.Get("x-paystack-signature"), raw) {
		writeError(w, http.StatusUnauthorized, ledger.CodeUnauthorized, "invalid signature")
		return
	}
	var ev paystackBody
	if err := json.Unmarshal(raw, &ev); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid JSON")
		return
	}
	if ev.Event != "charge.success" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "event"})
		return
	}
	var rawID int64
	if err := json.Unmarshal(ev.Data.ID, &rawID); err != nil || rawID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_json", "data.id must be a positive integer")
		return
	}
	eventID := strconv.FormatInt(rawID, 10)
	amount, err := money.ParseJSONAmount(ev.Data.Amount)
	if err != nil {
		writeError(w, http.StatusBadRequest, ledger.CodeInvalidAmount, "data.amount must be integer kobo")
		return
	}
	ref := strings.TrimSpace(ev.Data.Reference)
	currency := strings.ToUpper(strings.TrimSpace(ev.Data.Currency))
	dataStatus := strings.ToLower(strings.TrimSpace(ev.Data.Status))

	var result txResult
	err = s.withTx(r.Context(), func(tx pgx.Tx) error {
		result = txResult{}
		prev, claimed, err := s.store.ClaimProviderEvent(r.Context(), tx, providerPaystack, eventID)
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
		finish := func(merchantID, orderID *uuid.UUID, outcome string, status int, body []byte) error {
			if err := s.store.FinishProviderEvent(r.Context(), tx, providerPaystack, eventID, merchantID, orderID, outcome, status, body); err != nil {
				return err
			}
			result = txResult{status: status, body: body}
			return nil
		}
		order, err := s.store.GetOrderByReference(r.Context(), tx, ref)
		if err != nil {
			if le := ledger.AsError(err); le == nil || le.Code != ledger.CodeNotFound {
				return err
			}
			// No merchant to post to: keep the event and an audit trail only.
			meta, _ := json.Marshal(map[string]any{"reference": ref, "event_id": eventID, "amount": amount, "currency": currency})
			if err := s.store.InsertAudit(r.Context(), tx, store.AuditEvent{
				Action: "webhook.unknown_reference", EntityType: "webhook", EntityID: uuid.Nil, Metadata: meta,
			}); err != nil {
				return err
			}
			body, _ := json.Marshal(map[string]string{"status": "ignored", "reason": "unknown_reference"})
			return finish(nil, nil, "unknown_reference", http.StatusOK, body)
		}
		mid := order.MerchantID
		oid := order.ID
		locked, err := s.store.LockOrder(r.Context(), tx, order.MerchantID, order.ID)
		if err != nil {
			return err
		}
		if dataStatus != "success" {
			body, _ := json.Marshal(map[string]string{"status": "ignored", "reason": "status_not_success"})
			return finish(&mid, &oid, "ignored_status", http.StatusOK, body)
		}
		// A split order only expects its transfer portion from Paystack.
		due := locked.TotalMinor
		if locked.Status == "pending_payment" {
			if n, ok, err := s.store.PendingTransferMinor(r.Context(), tx, locked.ID); err != nil {
				return err
			} else if ok {
				due = n
			}
		}
		reason := ""
		switch {
		case locked.Status == "expired":
			reason = "order_expired"
		case locked.Status == "voided":
			reason = "order_voided"
		case locked.Status == "paid" || locked.Status == "partially_refunded":
			reason = "order_already_paid"
		case locked.Status == "refunded":
			reason = "order_refunded"
		case currency != "NGN":
			reason = "currency_mismatch"
		case amount != due:
			reason = "amount_mismatch"
		}
		merchant, err := s.store.EnsureBooks(r.Context(), tx, locked.MerchantID)
		if err != nil {
			return err
		}
		if reason != "" {
			u, err := checkout.RecordUnmatched(r.Context(), tx, s.store, checkout.UnmatchedInput{
				Merchant: merchant, Order: &locked, Provider: providerPaystack, EventID: eventID,
				Currency: currency, Reason: reason, Amount: amount, Now: time.Now(),
			})
			if err != nil {
				return err
			}
			body, err := json.Marshal(map[string]string{
				"status": "unmatched", "reason": reason, "unmatched_payment_id": u.ID.String(),
			})
			if err != nil {
				return err
			}
			return finish(&mid, &oid, "unmatched:"+reason, http.StatusOK, body)
		}
		outlet, err := s.store.GetOutlet(r.Context(), tx, locked.MerchantID, locked.OutletID)
		if err != nil {
			return err
		}
		confirmed, err := checkout.ConfirmTransfer(r.Context(), tx, s.store, checkout.ConfirmInput{
			Merchant: merchant, Outlet: outlet, Order: locked, Now: time.Now(),
			IdempotencyKey: "wh:" + providerPaystack + ":" + eventID, ProviderEventID: eventID,
		})
		if err != nil {
			return err
		}
		body, err := json.Marshal(orderBody(confirmed))
		if err != nil {
			return err
		}
		return finish(&mid, &oid, "paid", http.StatusOK, body)
	})
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
