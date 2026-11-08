package httpserver

import (
	"encoding/json"
	"net/http"

	"github.com/Muddyscode/till/internal/checkout"
	"github.com/Muddyscode/till/internal/money"
	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type customerReq struct {
	Name  string `json:"name"`
	Phone string `json:"phone"`
}

type topUpReq struct {
	OutletID    string          `json:"outlet_id"`
	AmountMinor json.RawMessage `json:"amount_minor"`
}

func customerBody(c store.Customer) map[string]any {
	return map[string]any{
		"id": c.ID, "name": c.Name, "phone": c.Phone,
		"balance_minor": c.BalanceMinor, "created_at": c.CreatedAt,
	}
}

func (s *Server) createCustomer(w http.ResponseWriter, r *http.Request) {
	raw, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var in customerReq
	if err := decodeStrict(raw, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid JSON")
		return
	}
	id, err := uuid.NewV7()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	actor := s.staff(r)
	c, err := s.store.CreateCustomer(r.Context(), s.store.Pool, store.Customer{
		ID: id, MerchantID: actor.MerchantID, Name: in.Name, Phone: in.Phone,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if asPg(err, &pgErr) && pgErr.Code == "23505" {
			writeError(w, http.StatusConflict, "phone_taken", "a customer with this phone already exists")
			return
		}
		writeLedgerError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, customerBody(c))
}

func (s *Server) listCustomers(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListCustomers(r.Context(), s.store.Pool, s.staff(r).MerchantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, c := range list {
		out = append(out, customerBody(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"customers": out})
}

func (s *Server) getCustomer(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseID(w, r)
	if !ok {
		return
	}
	c, err := s.store.GetCustomer(r.Context(), s.store.Pool, s.staff(r).MerchantID, id)
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, customerBody(c))
}

func (s *Server) topUpCustomer(w http.ResponseWriter, r *http.Request) {
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
	var in topUpReq
	if err := decodeStrict(raw, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid JSON")
		return
	}
	outletID, err := uuid.Parse(in.OutletID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_order", "outlet_id is invalid")
		return
	}
	outlet, ok := s.authorizeOutlet(w, r, outletID)
	if !ok {
		return
	}
	amount, err := money.ParseJSONAmount(in.AmountMinor)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_amount", "amount_minor must be a positive integer")
		return
	}
	actor := s.staff(r)
	s.finishIdempotent(w, r, actor.MerchantID, key, fingerprint(r, raw), func(tx pgx.Tx) (int, []byte, *uuid.UUID, error) {
		merchant, err := s.store.EnsureBooks(r.Context(), tx, actor.MerchantID)
		if err != nil {
			return 0, nil, nil, err
		}
		c, j, err := checkout.TopUp(r.Context(), tx, s.store, checkout.TopUpInput{
			Merchant: merchant, Outlet: outlet, StaffID: actor.ID, CustomerID: id,
			Amount: amount, IdempotencyKey: key,
		})
		if err != nil {
			return 0, nil, nil, err
		}
		body := customerBody(c)
		body["journal_id"] = j.ID
		rawBody, err := json.Marshal(body)
		if err != nil {
			return 0, nil, nil, err
		}
		return http.StatusCreated, rawBody, &j.ID, nil
	})
}
