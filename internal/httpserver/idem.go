package httpserver

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s *Server) finishIdempotent(w http.ResponseWriter, r *http.Request, merchantID uuid.UUID, key string, hash []byte, fn func(tx pgx.Tx) (int, []byte, *uuid.UUID, error)) {
	var result txResult
	err := s.withTx(r.Context(), func(tx pgx.Tx) error {
		rec, err := s.store.ClaimIdempotency(r.Context(), tx, merchantID, key, hash)
		if err != nil {
			return err
		}
		if rec.Replay {
			result = txResult{status: rec.HTTPStatus, body: rec.Body, replayed: true}
			return nil
		}
		status, body, journalID, err := fn(tx)
		if err != nil {
			return err
		}
		if err := s.store.CompleteIdempotency(r.Context(), tx, merchantID, key, status, body, journalID); err != nil {
			return err
		}
		result = txResult{status: status, body: body}
		return nil
	})
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	if result.status == 0 {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if result.replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	w.WriteHeader(result.status)
	_, _ = w.Write(append(result.body, '\n'))
}
