package httpserver

import (
	"net/http"
	"strings"

	"github.com/Muddyscode/till/internal/ledger"
	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type createAccountReq struct {
	Code          string `json:"code"`
	Name          string `json:"name"`
	Type          string `json:"type"`
	AllowNegative bool   `json:"allow_negative"`
}

func (s *Server) createAccount(w http.ResponseWriter, r *http.Request) {
	op := s.staff(r)
	var in createAccountReq
	raw, ok := s.readBody(w, r)
	if !ok {
		return
	}
	if err := decodeStrict(raw, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid JSON")
		return
	}
	in.Code = strings.TrimSpace(in.Code)
	in.Name = strings.TrimSpace(in.Name)
	if in.Code == "" || in.Name == "" {
		writeError(w, http.StatusBadRequest, "invalid_account", "code and name are required")
		return
	}
	t := ledger.AccountType(strings.ToLower(in.Type))
	if !t.Valid() {
		writeError(w, http.StatusBadRequest, "invalid_account", "type must be asset, liability, equity, income, or expense")
		return
	}
	id, err := uuid.NewV7()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	acct, err := s.store.CreateAccount(r.Context(), s.store.Pool, ledger.Account{
		ID:            id,
		MerchantID:    op.MerchantID,
		Code:          in.Code,
		Name:          in.Name,
		Type:          t,
		Currency:      "NGN",
		AllowNegative: in.AllowNegative,
		Status:        ledger.AccountOpen,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if asPg(err, &pgErr) && pgErr.Code == "23505" {
			writeError(w, http.StatusConflict, "code_taken", "account code already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, acctJSON(acct))
}

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	op := s.staff(r)
	filter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("type")))
	if filter != "" && !ledger.AccountType(filter).Valid() {
		writeError(w, http.StatusBadRequest, "invalid_account", "invalid type filter")
		return
	}
	list, err := s.store.ListAccounts(r.Context(), s.store.Pool, op.MerchantID, filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]accountJSON, 0, len(list))
	for _, a := range list {
		out = append(out, acctJSON(a))
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

func (s *Server) getAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseID(w, r)
	if !ok {
		return
	}
	acct, err := s.store.GetAccount(r.Context(), s.store.Pool, s.staff(r).MerchantID, id)
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, acctJSON(acct))
}

func (s *Server) statement(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseID(w, r)
	if !ok {
		return
	}
	limit := limitParam(r, 100, 500)
	rows, bal, err := s.store.Statement(r.Context(), s.store.Pool, s.staff(r).MerchantID, id, limit)
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	type line struct {
		JournalID    string `json:"journal_id"`
		Direction    string `json:"direction"`
		AmountMinor  int64  `json:"amount_minor"`
		Currency     string `json:"currency"`
		Description  string `json:"description"`
		CreatedAt    string `json:"created_at"`
		RunningMinor int64  `json:"running_balance_minor"`
	}
	out := make([]line, 0, len(rows))
	for _, row := range rows {
		out = append(out, line{
			JournalID:    row.Posting.JournalID.String(),
			Direction:    string(row.Posting.Direction),
			AmountMinor:  row.Posting.Amount,
			Currency:     row.Posting.Currency,
			Description:  row.Description,
			CreatedAt:    row.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			RunningMinor: row.Running,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account_id":    id,
		"balance_minor": bal,
		"entries":       out,
	})
}

func (s *Server) invariants(w http.ResponseWriter, r *http.Request) {
	idn, checks, err := s.store.Invariants(r.Context(), s.staff(r).MerchantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	all := append([]store.Check{
		{Name: "trial_balance_identity", Holds: idn.IdentityMinor == 0, Expected: 0, Actual: idn.IdentityMinor},
		{Name: "balance_cache_matches_journal", Holds: idn.CacheDrift == 0, Expected: 0, Actual: idn.CacheDrift},
	}, checks...)
	holds := true
	for _, c := range all {
		if !c.Holds {
			holds = false
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"holds":          holds,
		"identity_minor": idn.IdentityMinor,
		"cache_drift":    idn.CacheDrift,
		"accounts":       idn.Accounts,
		"journals":       idn.Journals,
		"checks":         all,
	})
}
