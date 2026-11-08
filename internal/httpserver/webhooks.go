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
	var eventID int64
	if err := json.Unmarshal(ev.Data.ID, &eventID); err != nil || eventID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_json", "data.id must be a positive integer")
		return
	}
	amount, err := money.ParseJSONAmount(ev.Data.Amount)
	if err != nil {
		writeError(w, http.StatusBadRequest, ledger.CodeInvalidAmount, "data.amount must be integer kobo")
		return
	}
	ref := strings.TrimSpace(ev.Data.Reference)
	order, err := s.store.GetOrderByReference(r.Context(), s.store.Pool, ref)
	if err != nil {
		meta, _ := json.Marshal(map[string]string{"reference": ref})
		_ = s.withTx(r.Context(), func(tx pgx.Tx) error {
			return s.store.InsertAudit(r.Context(), tx, store.AuditEvent{
				Action: "webhook.unknown_reference", EntityType: "webhook",
				EntityID: uuid.Nil, Metadata: meta,
			})
		})
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "unknown_reference"})
		return
	}
	currency := strings.ToUpper(strings.TrimSpace(ev.Data.Currency))
	dataStatus := strings.ToLower(strings.TrimSpace(ev.Data.Status))
	key := "wh:" + strconv.FormatInt(eventID, 10)
	s.finishIdempotent(w, r, order.MerchantID, key, canonicalHash(raw), func(tx pgx.Tx) (int, []byte, *uuid.UUID, error) {
		locked, err := s.store.LockOrder(r.Context(), tx, order.MerchantID, order.ID)
		if err != nil {
			return 0, nil, nil, err
		}
		if locked.Status != "pending_payment" {
			return s.ignoreWebhook(r, tx, locked, "webhook.order_not_pending", "order_not_pending")
		}
		if amount != locked.TotalMinor || currency != "NGN" || dataStatus != "success" {
			return s.ignoreWebhook(r, tx, locked, "webhook.amount_mismatch", "amount_mismatch")
		}
		outlet, err := s.store.GetOutlet(r.Context(), tx, locked.MerchantID, locked.OutletID)
		if err != nil {
			return 0, nil, nil, err
		}
		merchant, err := s.store.EnsureBooks(r.Context(), tx, locked.MerchantID)
		if err != nil {
			return 0, nil, nil, err
		}
		confirmed, err := checkout.ConfirmTransfer(r.Context(), tx, s.store, checkout.ConfirmInput{
			Merchant: merchant, Outlet: outlet, Order: locked, Now: time.Now(), IdempotencyKey: key,
		})
		if err != nil {
			return 0, nil, nil, err
		}
		body, err := json.Marshal(orderBody(confirmed))
		if err != nil {
			return 0, nil, nil, err
		}
		return http.StatusOK, body, confirmed.JournalID, nil
	})
}

func (s *Server) ignoreWebhook(r *http.Request, tx pgx.Tx, order store.Order, action, reason string) (int, []byte, *uuid.UUID, error) {
	meta, err := json.Marshal(map[string]any{"reference": order.Reference, "reason": reason})
	if err != nil {
		return 0, nil, nil, err
	}
	mid := order.MerchantID
	outlet := order.OutletID
	if err := s.store.InsertAudit(r.Context(), tx, store.AuditEvent{
		MerchantID: &mid, Action: action, EntityType: "order", EntityID: order.ID, OutletID: &outlet, Metadata: meta,
	}); err != nil {
		return 0, nil, nil, err
	}
	body, _ := json.Marshal(map[string]string{"status": "ignored", "reason": reason})
	return http.StatusOK, body, nil, nil
}
