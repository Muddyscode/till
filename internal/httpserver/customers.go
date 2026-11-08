package httpserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/Muddyscode/till/internal/auth"
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

// customerView hides most of the phone number unless the caller may see it (manager, admin).
func customerView(c store.Customer, fullPhone bool) map[string]any {
	phone := c.Phone
	if !fullPhone {
		phone = store.MaskPhone(phone)
	}
	return map[string]any{
		"id": c.ID, "name": c.Name, "phone": phone,
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
	phone, err := store.NormalizePhone(in.Phone)
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	actor := s.staff(r)
	c, err := s.store.CreateCustomer(r.Context(), s.store.Pool, store.Customer{
		ID: id, MerchantID: actor.MerchantID, Name: in.Name, Phone: phone,
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
	writeJSON(w, http.StatusCreated, customerView(c, canSeePhone(actor)))
}

func (s *Server) listCustomers(w http.ResponseWriter, r *http.Request) {
	st := s.staff(r)
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	full := canSeePhone(st)
	if !full && utf8.RuneCountInString(q) < 3 {
		writeError(w, http.StatusBadRequest, "search_required", "cashiers must search customers with q (at least 3 characters)")
		return
	}
	list, err := s.store.SearchCustomers(r.Context(), s.store.Pool, st.MerchantID, q, limitParam(r, 50, 200))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, c := range list {
		out = append(out, customerView(c, full))
	}
	writeJSON(w, http.StatusOK, map[string]any{"customers": out})
}

func canSeePhone(st store.Staff) bool {
	return st.Role == auth.RoleAdmin || st.Role == auth.RoleManager
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
	writeJSON(w, http.StatusOK, customerView(c, canSeePhone(s.staff(r))))
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
	s.finishIdempotent(w, r, actor.MerchantID, key, canonicalHash(raw), func(tx pgx.Tx) (int, []byte, *uuid.UUID, error) {
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
		body := customerView(c, false)
		body["journal_id"] = j.ID
		rawBody, err := json.Marshal(body)
		if err != nil {
			return 0, nil, nil, err
		}
		return http.StatusCreated, rawBody, &j.ID, nil
	})
}
