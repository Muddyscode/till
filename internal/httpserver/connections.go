package httpserver

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/Muddyscode/till/internal/ledger"
	"github.com/Muddyscode/till/internal/money"
	"github.com/Muddyscode/till/internal/payments"
	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var connectionCode = regexp.MustCompile(`^[A-Z0-9]{2,8}$`)

func connectionBody(c store.Connection) map[string]any {
	b := map[string]any{
		"id": c.ID, "code": c.Code, "name": c.Name, "provider": c.Provider, "active": c.Active,
		"account_number": c.AccountNumber, "clearing_account_id": c.ClearingAccountID,
		// What the provider still holds for the merchant: received, less fees,
		// refunds paid out and settlements.
		"balance_minor": c.BalanceMinor, "webhook_path": "/v1/webhooks/" + c.ID.String(),
		"created_at": c.CreatedAt,
	}
	if c.OutletID != nil {
		b["outlet_id"] = c.OutletID
	}
	return b
}

// createConnection registers a place the merchant receives non-cash money.
// For provider "till" the secret is generated and shown once. For a real
// provider the merchant supplies that provider's webhook signing secret.
func (s *Server) createConnection(w http.ResponseWriter, r *http.Request) {
	raw, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var in struct {
		Code          string `json:"code"`
		Name          string `json:"name"`
		Provider      string `json:"provider"`
		Secret        string `json:"secret"`
		OutletID      string `json:"outlet_id"`
		AccountNumber string `json:"account_number"`
	}
	if err := decodeStrict(raw, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid JSON")
		return
	}
	bad := func(msg string) { writeError(w, http.StatusBadRequest, "invalid_connection", msg) }
	c := store.Connection{
		Code: strings.ToUpper(strings.TrimSpace(in.Code)), Name: strings.TrimSpace(in.Name),
		Provider: strings.ToLower(strings.TrimSpace(in.Provider)), Secret: strings.TrimSpace(in.Secret),
		AccountNumber: strings.TrimSpace(in.AccountNumber),
	}
	if !connectionCode.MatchString(c.Code) {
		bad("code must be 2 to 8 letters or digits, such as MONIE")
		return
	}
	if c.Name == "" || len(c.Name) > 80 || len(c.AccountNumber) > 40 {
		bad("name is required (at most 80 characters); account_number is at most 40")
		return
	}
	if _, ok := payments.For(c.Provider); !ok {
		bad("provider must be one of " + strings.Join(payments.Providers(), ", "))
		return
	}
	generated := false
	switch {
	case c.Secret == "" && c.Provider == "till":
		var b [32]byte
		if _, err := rand.Read(b[:]); err != nil {
			s.internalError(w, r, err)
			return
		}
		c.Secret, generated = "tillsec_"+hex.EncodeToString(b[:]), true
	case len(c.Secret) < 16 || len(c.Secret) > 200:
		bad("secret must be 16 to 200 characters: the provider's webhook signing secret")
		return
	}
	actor := s.staff(r)
	c.MerchantID = actor.MerchantID
	if oid := strings.TrimSpace(in.OutletID); oid != "" {
		id, err := uuid.Parse(oid)
		if err != nil {
			bad("outlet_id is invalid")
			return
		}
		if _, ok := s.authorizeOutlet(w, r, id); !ok {
			return
		}
		c.OutletID = &id
	}
	var err error
	if c.ID, err = uuid.NewV7(); err != nil {
		s.internalError(w, r, err)
		return
	}
	var created store.Connection
	err = s.withTx(r.Context(), func(tx pgx.Tx) error {
		if created, err = s.store.CreateConnection(r.Context(), tx, c, &actor.ID); err != nil {
			return err
		}
		meta, _ := json.Marshal(map[string]any{"code": c.Code, "provider": c.Provider, "name": c.Name})
		mid, sid := actor.MerchantID, actor.ID
		return s.store.InsertAudit(r.Context(), tx, store.AuditEvent{
			MerchantID: &mid, StaffID: &sid, Action: "payment_connection.created", EntityType: "payment_connection",
			EntityID: created.ID, OutletID: c.OutletID, Metadata: meta,
		})
	})
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	body := connectionBody(created)
	if generated {
		body["secret"] = created.Secret
	}
	writeJSON(w, http.StatusCreated, body)
}

func (s *Server) listConnections(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListConnections(r.Context(), s.store.Pool, s.staff(r).MerchantID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, c := range list {
		out = append(out, connectionBody(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"payment_connections": out})
}

// disableConnection stops a connection accepting payments. Its history and
// its clearing balance stay.
func (s *Server) disableConnection(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseID(w, r)
	if !ok {
		return
	}
	actor := s.staff(r)
	err := s.withTx(r.Context(), func(tx pgx.Tx) error {
		if err := s.store.SetConnectionActive(r.Context(), tx, actor.MerchantID, id, false); err != nil {
			return err
		}
		mid, sid := actor.MerchantID, actor.ID
		return s.store.InsertAudit(r.Context(), tx, store.AuditEvent{
			MerchantID: &mid, StaffID: &sid, Action: "payment_connection.disabled", EntityType: "payment_connection",
			EntityID: id, Metadata: []byte(`{}`),
		})
	})
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// settleConnection records a payout from the provider to the merchant's bank:
// Dr 1050 Bank / Cr the connection's clearing account.
func (s *Server) settleConnection(w http.ResponseWriter, r *http.Request) {
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
	var in struct {
		AmountMinor json.RawMessage `json:"amount_minor"`
		Reference   string          `json:"reference"`
	}
	if err := decodeStrict(raw, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid JSON")
		return
	}
	amount, err := money.ParseJSONAmount(in.AmountMinor)
	if err != nil {
		writeError(w, http.StatusBadRequest, ledger.CodeInvalidAmount, err.Error())
		return
	}
	ref := strings.TrimSpace(in.Reference)
	if len(ref) > 100 {
		writeError(w, http.StatusBadRequest, "invalid_connection", "reference is too long")
		return
	}
	actor := s.staff(r)
	s.finishIdempotent(w, r, actor.MerchantID, key, fingerprint(r, raw), func(tx pgx.Tx) (int, []byte, *uuid.UUID, error) {
		conn, err := s.store.GetConnection(r.Context(), tx, actor.MerchantID, id)
		if err != nil {
			return 0, nil, nil, err
		}
		bank, err := s.store.EnsureAccount(r.Context(), tx, actor.MerchantID, store.BankAccountCode, "Bank", ledger.Asset, false)
		if err != nil {
			return 0, nil, nil, err
		}
		j, err := ledger.Post(r.Context(), tx, s.store.Poster(), ledger.PostRequest{
			MerchantID:  actor.MerchantID,
			Description: strings.TrimSpace("settlement " + conn.Code + " " + ref),
			Lines: []ledger.Line{
				{AccountID: bank, Direction: ledger.Debit, Amount: amount},
				{AccountID: conn.ClearingAccountID, Direction: ledger.Credit, Amount: amount},
			},
		})
		if err != nil {
			return 0, nil, nil, err
		}
		sid, err := uuid.NewV7()
		if err != nil {
			return 0, nil, nil, err
		}
		st, err := s.store.InsertSettlement(r.Context(), tx, actor.MerchantID, actor.ID, store.Settlement{
			ID: sid, ConnectionID: conn.ID, AmountMinor: amount, Reference: ref, JournalID: j.ID,
		})
		if err != nil {
			return 0, nil, nil, err
		}
		meta, _ := json.Marshal(map[string]any{"code": conn.Code, "amount_minor": amount, "reference": ref, "journal_id": j.ID})
		mid, aid := actor.MerchantID, actor.ID
		if err := s.store.InsertAudit(r.Context(), tx, store.AuditEvent{
			MerchantID: &mid, StaffID: &aid, Action: "payment_connection.settled", EntityType: "payment_connection",
			EntityID: conn.ID, OutletID: conn.OutletID, Metadata: meta,
		}); err != nil {
			return 0, nil, nil, err
		}
		body, err := json.Marshal(map[string]any{
			"id": st.ID, "connection_id": conn.ID, "amount_minor": amount, "reference": ref,
			"journal_id": j.ID, "created_at": st.CreatedAt, "balance_minor": conn.BalanceMinor - amount,
		})
		return http.StatusCreated, body, &j.ID, err
	})
}
