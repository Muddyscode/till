package httpserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/Muddyscode/till/internal/checkout"
	"github.com/Muddyscode/till/internal/ledger"
)

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	var b errorBody
	b.Error.Code = code
	b.Error.Message = message
	writeJSON(w, status, b)
}

func writeLedgerError(w http.ResponseWriter, err error) {
	if le := ledger.AsError(err); le != nil {
		writeError(w, statusFor(le.Code), le.Code, le.Message)
		return
	}
	if er, ok := w.(errorSetter); ok {
		er.SetError(err)
	}
	writeError(w, http.StatusInternalServerError, "internal", "internal error")
}

func statusFor(code string) int {
	switch code {
	case ledger.CodeUnauthorized:
		return http.StatusUnauthorized
	case ledger.CodeNotFound:
		return http.StatusNotFound
	case ledger.CodeIdempotencyConflict, ledger.CodeIdempotencyInProgress, ledger.CodeAlreadyReversed, "already_closed",
		ledger.CodeInsufficientStock, ledger.CodeInsufficientWallet, ledger.CodeRefundViaOrder, "last_admin", "email_taken", "code_taken", "sku_taken", "phone_taken",
		ledger.CodeControlAccount, ledger.CodeRefundInsufficient, checkout.CodeUnmatchedNotApplicable,
		"unmatched_not_open", "unmatched_not_refund_pending", checkout.CodeRefundExceedsOrder,
		"shift_already_open", "transfer_not_in_transit", "connection_code_taken":
		return http.StatusConflict
	case ledger.CodeForbidden, ledger.CodeOutletForbidden, checkout.CodeDiscountLimit:
		return http.StatusForbidden
	default:
		return http.StatusBadRequest
	}
}

// canonicalBody re-encodes JSON with sorted keys and numbers kept as written,
// so key order and whitespace do not matter but a large integer never loses
// precision (json.Number, not float64).
func canonicalBody(raw []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return raw
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return raw
	}
	canon, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return canon
}

// fingerprint is what an Idempotency-Key is bound to: HTTP method, the
// resolved route path (ids included) and the canonical body. The same key on a
// different order, customer or endpoint is therefore a conflict, not a replay.
func fingerprint(r *http.Request, raw []byte) []byte {
	h := sha256.New()
	h.Write([]byte(strings.ToUpper(r.Method)))
	h.Write([]byte{0})
	h.Write([]byte(strings.ToLower(r.URL.Path)))
	h.Write([]byte{0})
	h.Write(canonicalBody(raw))
	return h.Sum(nil)
}

var reservedKeyPrefixes = []string{"wh:", "sys:"}

func decodeStrict(raw []byte, dest any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing json")
		}
		return err
	}
	return nil
}

func requireIdempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		writeError(w, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key header is required")
		return "", false
	}
	lower := strings.ToLower(key)
	for _, p := range reservedKeyPrefixes {
		if strings.HasPrefix(lower, p) {
			writeError(w, http.StatusBadRequest, "idempotency_key_reserved", "Idempotency-Key prefix "+p+" is reserved")
			return "", false
		}
	}
	if len(key) > 256 {
		writeError(w, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key is too long")
		return "", false
	}
	return key, true
}
