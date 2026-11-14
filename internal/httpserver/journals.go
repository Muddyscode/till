package httpserver

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/Muddyscode/till/internal/ledger"
	"github.com/Muddyscode/till/internal/money"
	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type journalLineReq struct {
	AccountID   string          `json:"account_id"`
	Direction   string          `json:"direction"`
	AmountMinor json.RawMessage `json:"amount_minor"`
}

type journalReq struct {
	Description string           `json:"description"`
	Postings    []journalLineReq `json:"postings"`
}

type txResult struct {
	status   int
	body     []byte
	replayed bool
}

func (s *Server) postJournal(w http.ResponseWriter, r *http.Request) {
	key, ok := requireIdempotencyKey(w, r)
	if !ok {
		return
	}
	raw, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var in journalReq
	if err := decodeStrict(raw, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid JSON")
		return
	}
	lines := make([]ledger.Line, 0, len(in.Postings))
	for _, p := range in.Postings {
		id, err := uuid.Parse(p.AccountID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_journal", "account_id is invalid")
			return
		}
		amt, err := money.ParseJSONAmount(p.AmountMinor)
		if err != nil {
			writeError(w, http.StatusBadRequest, ledger.CodeInvalidAmount, err.Error())
			return
		}
		lines = append(lines, ledger.Line{
			AccountID: id,
			Direction: ledger.Direction(p.Direction),
			Amount:    amt,
		})
	}
	s.postLines(w, r, key, fingerprint(r, raw), in.Description, lines, nil)
}

func (s *Server) reverseJournal(w http.ResponseWriter, r *http.Request) {
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
	op := s.staff(r)
	orig, err := s.store.GetJournal(r.Context(), s.store.Pool, op.MerchantID, id)
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	used, err := s.store.JournalUsedByOrder(r.Context(), s.store.Pool, op.MerchantID, orig.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if used {
		writeError(w, http.StatusConflict, ledger.CodeRefundViaOrder, "refund the order so stock is restored")
		return
	}
	if _, exists, err := s.store.ReversalOf(r.Context(), s.store.Pool, op.MerchantID, orig.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	} else if exists {
		writeError(w, http.StatusConflict, ledger.CodeAlreadyReversed, "journal already reversed")
		return
	}
	s.postLines(w, r, key, fingerprint(r, raw), "reversal of "+orig.ID.String(), ledger.ReverseLines(orig.Postings), &orig.ID)
}

func (s *Server) postLines(w http.ResponseWriter, r *http.Request, key string, hash []byte, desc string, lines []ledger.Line, reverses *uuid.UUID) {
	op := s.staff(r)
	var result txResult
	err := s.withTx(r.Context(), func(tx pgx.Tx) error {
		rec, err := s.store.ClaimIdempotency(r.Context(), tx, op.MerchantID, key, hash)
		if err != nil {
			return err
		}
		if rec.Replay {
			result = txResult{status: rec.HTTPStatus, body: rec.Body, replayed: true}
			return nil
		}
		if err := s.refuseControlAccounts(r.Context(), tx, op.MerchantID, lines); err != nil {
			return err
		}
		j, err := ledger.Post(r.Context(), tx, s.store.Poster(), ledger.PostRequest{
			MerchantID:        op.MerchantID,
			Description:       desc,
			Lines:             lines,
			ReversesJournalID: reverses,
			IdempotencyKey:    key,
		})
		if err != nil {
			return err
		}
		body, err := json.Marshal(jJSON(j))
		if err != nil {
			return err
		}
		if err := s.store.CompleteIdempotency(r.Context(), tx, op.MerchantID, key, http.StatusCreated, body, &j.ID); err != nil {
			return err
		}
		result = txResult{status: http.StatusCreated, body: body}
		return nil
	})
	if err != nil {
		if store.IsUnique(err, nil) && reverses != nil {
			writeError(w, http.StatusConflict, ledger.CodeAlreadyReversed, "journal already reversed")
			return
		}
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

// refuseControlAccounts blocks free-form journals and raw reversals
// from touching the merchant's system accounts. Those move only through the
// flows that keep stock, wallet balances and unmatched payments in step.
func (s *Server) refuseControlAccounts(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, lines []ledger.Line) error {
	m, err := s.store.GetMerchant(ctx, tx, merchantID)
	if err != nil {
		return err
	}
	control := map[uuid.UUID]string{}
	for code, id := range map[string]*uuid.UUID{
		"1100": m.ClearingAccountID, "1200": m.InventoryAccountID, "2100": m.VATAccountID,
		"2150": m.GoodsReceivedAccountID, "2200": m.WalletAccountID, "2300": m.SuspenseAccountID,
		"2310": m.RefundsPayableID, "5000": m.COGSAccountID,
	} {
		if id != nil {
			control[*id] = code
		}
	}
	if dep, ok, err := s.store.DepositsAccountID(ctx, tx, merchantID); err != nil {
		return err
	} else if ok {
		control[dep] = "2400"
	}
	clearing, err := s.store.ClearingAccountIDs(ctx, tx, merchantID)
	if err != nil {
		return err
	}
	for id, code := range clearing {
		control[id] = code
	}
	for _, l := range lines {
		if code, ok := control[l.AccountID]; ok {
			return ledger.Err(ledger.CodeControlAccount, "account "+code+" is a system control account and cannot be posted to directly")
		}
	}
	return nil
}

func (s *Server) getJournal(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseID(w, r)
	if !ok {
		return
	}
	j, err := s.store.GetJournal(r.Context(), s.store.Pool, s.staff(r).MerchantID, id)
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, jJSON(j))
}
