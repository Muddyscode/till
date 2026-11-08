package httpserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

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
		ledger.CodeInsufficientStock, ledger.CodeInsufficientWallet, ledger.CodeRefundViaOrder, "last_admin", "email_taken", "code_taken", "sku_taken", "phone_taken":
		return http.StatusConflict
	case ledger.CodeForbidden, ledger.CodeOutletForbidden:
		return http.StatusForbidden
	default:
		return http.StatusBadRequest
	}
}

func canonicalHash(raw []byte) []byte {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		sum := sha256.Sum256(raw)
		return sum[:]
	}
	canon, err := json.Marshal(v)
	if err != nil {
		sum := sha256.Sum256(raw)
		return sum[:]
	}
	sum := sha256.Sum256(canon)
	return sum[:]
}

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
	if len(key) > 256 {
		writeError(w, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key is too long")
		return "", false
	}
	return key, true
}
