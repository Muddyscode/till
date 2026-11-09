package httpserver

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/Muddyscode/till/internal/auth"
	"github.com/Muddyscode/till/internal/checkout"
	"github.com/Muddyscode/till/internal/ledger"
	"github.com/Muddyscode/till/internal/money"
	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var outletCodeRe = regexp.MustCompile(`^[A-Z0-9]{2,8}$`)

type outletReq struct {
	Name string `json:"name"`
	Code string `json:"code"`
}

type productReq struct {
	SKU          string          `json:"sku"`
	Name         string          `json:"name"`
	PriceMinor   json.RawMessage `json:"price_minor"`
	AvgCostMinor json.RawMessage `json:"avg_cost_minor"`
	ReorderLevel *int            `json:"reorder_level"`
	TaxClass     string          `json:"tax_class"`
}

type productPatch struct {
	Name         *string         `json:"name"`
	PriceMinor   json.RawMessage `json:"price_minor"`
	AvgCostMinor json.RawMessage `json:"avg_cost_minor"`
	ReorderLevel *int            `json:"reorder_level"`
	Active       *bool           `json:"active"`
	TaxClass     *string         `json:"tax_class"`
}

type stockReq struct {
	OutletID      string          `json:"outlet_id"`
	SKU           string          `json:"sku"`
	Qty           int             `json:"qty"`
	Delta         int             `json:"delta"`
	Reason        string          `json:"reason"`
	UnitCostMinor json.RawMessage `json:"unit_cost_minor"`
}

func outletBody(o store.Outlet) map[string]any {
	return map[string]any{
		"id": o.ID, "name": o.Name, "code": o.Code,
		"cash_account_id": o.CashAccountID, "sales_account_id": o.SalesAccountID,
		"created_at": o.CreatedAt,
	}
}

func productBody(p store.Product) map[string]any {
	return map[string]any{
		"id": p.ID, "sku": p.SKU, "name": p.Name, "price_minor": p.PriceMinor,
		"avg_cost_minor": p.AvgCostMinor,
		"reorder_level":  p.ReorderLevel, "active": p.Active, "tax_class": p.TaxClass, "created_at": p.CreatedAt,
	}
}

func (s *Server) authorizeOutlet(w http.ResponseWriter, r *http.Request, outletID uuid.UUID) (store.Outlet, bool) {
	st := s.staff(r)
	o, err := s.store.GetOutlet(r.Context(), s.store.Pool, st.MerchantID, outletID)
	if err != nil {
		writeLedgerError(w, err)
		return store.Outlet{}, false
	}
	if st.Role == auth.RoleAdmin {
		return o, true
	}
	ok, err := s.store.StaffHasOutlet(r.Context(), s.store.Pool, st.ID, outletID, st.MerchantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return store.Outlet{}, false
	}
	if !ok {
		writeError(w, http.StatusForbidden, ledger.CodeOutletForbidden, "staff is not assigned to this outlet")
		return store.Outlet{}, false
	}
	return o, true
}

func (s *Server) createOutlet(w http.ResponseWriter, r *http.Request) {
	raw, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var in outletReq
	if err := decodeStrict(raw, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid JSON")
		return
	}
	code := strings.ToUpper(strings.TrimSpace(in.Code))
	if strings.TrimSpace(in.Name) == "" || !outletCodeRe.MatchString(code) {
		writeError(w, http.StatusBadRequest, "invalid_order", "outlet name is required and code must be 2-8 letters or digits")
		return
	}
	id, err := uuid.NewV7()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	actor := s.staff(r)
	var created store.Outlet
	err = s.withTx(r.Context(), func(tx pgx.Tx) error {
		var err error
		created, err = s.store.CreateOutlet(r.Context(), tx, id, actor.MerchantID, in.Name, code)
		if err != nil {
			return err
		}
		meta, _ := json.Marshal(map[string]string{"code": created.Code, "name": created.Name})
		mid, sid := actor.MerchantID, actor.ID
		oid := created.ID
		return s.store.InsertAudit(r.Context(), tx, store.AuditEvent{
			MerchantID: &mid, StaffID: &sid, Action: "outlet.created", EntityType: "outlet",
			EntityID: created.ID, OutletID: &oid, Metadata: meta,
		})
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if asPg(err, &pgErr) && pgErr.Code == "23505" {
			writeError(w, http.StatusConflict, "code_taken", "outlet code already exists")
			return
		}
		writeLedgerError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, outletBody(created))
}

func (s *Server) listOutlets(w http.ResponseWriter, r *http.Request) {
	st := s.staff(r)
	list, err := s.store.ListOutlets(r.Context(), s.store.Pool, st.MerchantID, st.ID, st.Role)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, o := range list {
		out = append(out, outletBody(o))
	}
	writeJSON(w, http.StatusOK, map[string]any{"outlets": out})
}

// parseTaxClass accepts "" (standard) or one of standard, exempt, zero.
func parseTaxClass(raw string) (string, bool) {
	c := strings.ToLower(strings.TrimSpace(raw))
	switch c {
	case "", "standard", "exempt", "zero":
		return c, true
	}
	return "", false
}

func normalizeSKU(sku string) string {
	return strings.ToUpper(strings.TrimSpace(sku))
}

func (s *Server) createProduct(w http.ResponseWriter, r *http.Request) {
	raw, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var in productReq
	if err := decodeStrict(raw, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid JSON")
		return
	}
	sku := normalizeSKU(in.SKU)
	name := strings.TrimSpace(in.Name)
	price, err := money.ParseJSONAmount(in.PriceMinor)
	if sku == "" || name == "" || err != nil || price <= 0 {
		writeError(w, http.StatusBadRequest, ledger.CodeInvalidAmount, "sku, name, and a positive integer price_minor are required")
		return
	}
	avg, err := money.ParseOptionalKobo(in.AvgCostMinor)
	if err != nil {
		writeError(w, http.StatusBadRequest, ledger.CodeInvalidAmount, "avg_cost_minor must be a JSON integer in kobo")
		return
	}
	taxClass, ok := parseTaxClass(in.TaxClass)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_order", "tax_class must be standard, exempt, or zero")
		return
	}
	reorder := 0
	if in.ReorderLevel != nil {
		if *in.ReorderLevel < 0 {
			writeError(w, http.StatusBadRequest, "invalid_order", "reorder_level cannot be negative")
			return
		}
		reorder = *in.ReorderLevel
	}
	id, err := uuid.NewV7()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	actor := s.staff(r)
	p, err := s.store.CreateProduct(r.Context(), s.store.Pool, store.Product{
		ID: id, MerchantID: actor.MerchantID, SKU: sku, Name: name, PriceMinor: price, AvgCostMinor: avg, ReorderLevel: reorder, Active: true, TaxClass: taxClass,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if asPg(err, &pgErr) && pgErr.Code == "23505" {
			writeError(w, http.StatusConflict, "sku_taken", "sku already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, productBody(p))
}

func (s *Server) listProducts(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListProducts(r.Context(), s.store.Pool, s.staff(r).MerchantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, p := range list {
		out = append(out, productBody(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"products": out})
}

func (s *Server) patchProduct(w http.ResponseWriter, r *http.Request) {
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
	var in productPatch
	if err := decodeStrict(raw, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid JSON")
		return
	}
	// Everything that does not need the product row is validated first.
	var name *string
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if n == "" {
			writeError(w, http.StatusBadRequest, "invalid_order", "name cannot be empty")
			return
		}
		name = &n
	}
	var price *int64
	if len(in.PriceMinor) > 0 {
		v, err := money.ParseJSONAmount(in.PriceMinor)
		if err != nil || v <= 0 {
			writeError(w, http.StatusBadRequest, ledger.CodeInvalidAmount, "price_minor must be a positive integer")
			return
		}
		price = &v
	}
	if in.ReorderLevel != nil && *in.ReorderLevel < 0 {
		writeError(w, http.StatusBadRequest, "invalid_order", "reorder_level cannot be negative")
		return
	}
	var taxClass *string
	if in.TaxClass != nil {
		c, ok := parseTaxClass(*in.TaxClass)
		if !ok || c == "" {
			writeError(w, http.StatusBadRequest, "invalid_order", "tax_class must be standard, exempt, or zero")
			return
		}
		taxClass = &c
	}
	var avg *int64
	if c := strings.TrimSpace(string(in.AvgCostMinor)); c != "" && c != "null" {
		v, err := money.ParseOptionalKobo(in.AvgCostMinor)
		if err != nil {
			writeError(w, http.StatusBadRequest, ledger.CodeInvalidAmount, "avg_cost_minor must be a JSON integer in kobo")
			return
		}
		avg = &v
	}
	actor := s.staff(r)
	s.finishIdempotent(w, r, actor.MerchantID, key, fingerprint(r, raw), func(tx pgx.Tx) (int, []byte, *uuid.UUID, error) {
		merchant, err := s.store.EnsureBooks(r.Context(), tx, actor.MerchantID)
		if err != nil {
			return 0, nil, nil, err
		}
		// The patch is applied to the row as locked here, never to a copy read
		// before the transaction, so it cannot revert a concurrent receipt's cost.
		p, err := s.store.LockProduct(r.Context(), tx, actor.MerchantID, id)
		if err != nil {
			return 0, nil, nil, err
		}
		if name != nil {
			p.Name = *name
		}
		if price != nil {
			p.PriceMinor = *price
		}
		if in.ReorderLevel != nil {
			p.ReorderLevel = *in.ReorderLevel
		}
		if in.Active != nil {
			p.Active = *in.Active
		}
		if taxClass != nil {
			p.TaxClass = *taxClass
		}
		if avg != nil {
			p.AvgCostMinor = *avg
		}
		updated, err := checkout.SaveProduct(r.Context(), tx, s.store, merchant, p, actor.ID)
		if err != nil {
			return 0, nil, nil, err
		}
		body, err := json.Marshal(productBody(updated))
		if err != nil {
			return 0, nil, nil, err
		}
		return http.StatusOK, body, nil, nil
	})
}

func (s *Server) receiveStock(w http.ResponseWriter, r *http.Request) {
	s.stockMove(w, r, false)
}

func (s *Server) adjustStock(w http.ResponseWriter, r *http.Request) {
	s.stockMove(w, r, true)
}

func (s *Server) stockMove(w http.ResponseWriter, r *http.Request, adjust bool) {
	key, ok := requireIdempotencyKey(w, r)
	if !ok {
		return
	}
	raw, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var in stockReq
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
	actor := s.staff(r)
	product, err := s.store.GetProductBySKU(r.Context(), s.store.Pool, actor.MerchantID, normalizeSKU(in.SKU))
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	if adjust && strings.TrimSpace(in.Reason) == "" {
		writeError(w, http.StatusBadRequest, "invalid_order", "reason is required")
		return
	}
	if !adjust && in.Qty <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_order", "qty must be > 0")
		return
	}
	if adjust && in.Delta == 0 {
		writeError(w, http.StatusBadRequest, "invalid_order", "delta cannot be zero")
		return
	}
	unitRaw := strings.TrimSpace(string(in.UnitCostMinor))
	if adjust && unitRaw != "" && unitRaw != "null" {
		writeError(w, http.StatusBadRequest, "invalid_order", "unit_cost_minor is only for receipts")
		return
	}
	var unit *int64
	if !adjust && unitRaw != "" && unitRaw != "null" {
		n, err := money.ParseOptionalKobo(in.UnitCostMinor)
		if err != nil {
			writeError(w, http.StatusBadRequest, ledger.CodeInvalidAmount, "unit_cost_minor must be a JSON integer in kobo")
			return
		}
		unit = &n
	}
	s.finishIdempotent(w, r, actor.MerchantID, key, fingerprint(r, raw), func(tx pgx.Tx) (int, []byte, *uuid.UUID, error) {
		merchant, err := s.store.EnsureBooks(r.Context(), tx, actor.MerchantID)
		if err != nil {
			return 0, nil, nil, err
		}
		body := map[string]any{"sku": product.SKU, "outlet_id": outlet.ID}
		if adjust {
			onHand, err := checkout.Adjust(r.Context(), tx, s.store, checkout.AdjustInput{
				Merchant: merchant, Outlet: outlet, StaffID: actor.ID, ProductID: product.ID,
				Delta: in.Delta, Reason: strings.TrimSpace(in.Reason),
			})
			if err != nil {
				return 0, nil, nil, err
			}
			body["on_hand"] = onHand
		} else {
			onHand, avg, err := checkout.Receive(r.Context(), tx, s.store, checkout.ReceiptInput{
				Merchant: merchant, Outlet: outlet, StaffID: actor.ID, ProductID: product.ID,
				Qty: in.Qty, UnitCost: unit,
			})
			if err != nil {
				return 0, nil, nil, err
			}
			body["on_hand"] = onHand
			body["avg_cost_minor"] = avg
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, nil, err
		}
		return http.StatusOK, raw, nil, nil
	})
}
