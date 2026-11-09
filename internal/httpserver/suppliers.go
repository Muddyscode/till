package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Muddyscode/till/internal/ledger"
	"github.com/Muddyscode/till/internal/money"
	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Suppliers, supplier payments and the VAT summary (Phase 3B).

func supplierBody(sp store.Supplier) map[string]any {
	return map[string]any{"id": sp.ID, "name": sp.Name, "phone": sp.Phone, "note": sp.Note, "created_at": sp.CreatedAt}
}

type supplierReq struct {
	Name  *string `json:"name"`
	Phone *string `json:"phone"`
	Note  *string `json:"note"`
}

// cleanSupplier validates the optional fields into sp. It reports whether the request was valid.
func cleanSupplier(w http.ResponseWriter, in supplierReq, sp *store.Supplier) bool {
	if in.Name != nil {
		sp.Name = strings.TrimSpace(*in.Name)
	}
	if sp.Name == "" || len(sp.Name) > 120 {
		writeError(w, http.StatusBadRequest, "invalid_order", "name is required (max 120 characters)")
		return false
	}
	if in.Phone != nil {
		p := strings.TrimSpace(*in.Phone)
		if p != "" {
			n, err := store.NormalizePhone(p)
			if err != nil {
				writeLedgerError(w, err)
				return false
			}
			p = n
		}
		sp.Phone = p
	}
	if in.Note != nil {
		sp.Note = strings.TrimSpace(*in.Note)
		if len(sp.Note) > 500 {
			writeError(w, http.StatusBadRequest, "invalid_order", "note is too long")
			return false
		}
	}
	return true
}

func (s *Server) createSupplier(w http.ResponseWriter, r *http.Request) {
	raw, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var in supplierReq
	if err := decodeStrict(raw, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid JSON")
		return
	}
	id, err := uuid.NewV7()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	sp := store.Supplier{ID: id, MerchantID: s.staff(r).MerchantID}
	if !cleanSupplier(w, in, &sp) {
		return
	}
	created, err := s.store.CreateSupplier(r.Context(), s.store.Pool, sp)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, supplierBody(created))
}

func (s *Server) listSuppliers(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListSuppliers(r.Context(), s.store.Pool, s.staff(r).MerchantID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, sp := range list {
		out = append(out, supplierBody(sp))
	}
	writeJSON(w, http.StatusOK, map[string]any{"suppliers": out})
}

func (s *Server) getSupplier(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseID(w, r)
	if !ok {
		return
	}
	sp, err := s.store.GetSupplier(r.Context(), s.store.Pool, s.staff(r).MerchantID, id)
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, supplierBody(sp))
}

func (s *Server) patchSupplier(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseID(w, r)
	if !ok {
		return
	}
	raw, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var in supplierReq
	if err := decodeStrict(raw, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid JSON")
		return
	}
	sp, err := s.store.GetSupplier(r.Context(), s.store.Pool, s.staff(r).MerchantID, id)
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	if !cleanSupplier(w, in, &sp) {
		return
	}
	updated, err := s.store.UpdateSupplier(r.Context(), s.store.Pool, sp)
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, supplierBody(updated))
}

func (s *Server) deleteSupplier(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseID(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteSupplier(r.Context(), s.store.Pool, s.staff(r).MerchantID, id); err != nil {
		writeLedgerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type supplierPaymentReq struct {
	SupplierID  string          `json:"supplier_id"`
	OutletID    string          `json:"outlet_id"`
	AmountMinor json.RawMessage `json:"amount_minor"`
	Method      string          `json:"method"`
	Note        string          `json:"note"`
}

// createSupplierPayment pays down goods received: Dr 2150 / Cr outlet cash (cash) or Cr 1100 clearing (bank).
func (s *Server) createSupplierPayment(w http.ResponseWriter, r *http.Request) {
	key, ok := requireIdempotencyKey(w, r)
	if !ok {
		return
	}
	raw, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var in supplierPaymentReq
	if err := decodeStrict(raw, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid JSON")
		return
	}
	supplierID, err1 := uuid.Parse(in.SupplierID)
	outletID, err2 := uuid.Parse(in.OutletID)
	if err1 != nil || err2 != nil {
		writeError(w, http.StatusBadRequest, "invalid_order", "supplier_id and outlet_id are required")
		return
	}
	amount, err := money.ParseJSONAmount(in.AmountMinor)
	if err != nil {
		writeError(w, http.StatusBadRequest, ledger.CodeInvalidAmount, "amount_minor must be a positive integer in kobo")
		return
	}
	if in.Method != "cash" && in.Method != "bank" {
		writeError(w, http.StatusBadRequest, "invalid_order", "method must be cash or bank")
		return
	}
	note := strings.TrimSpace(in.Note)
	if len(note) > 500 {
		writeError(w, http.StatusBadRequest, "invalid_order", "note is too long")
		return
	}
	outlet, ok := s.authorizeOutlet(w, r, outletID)
	if !ok {
		return
	}
	actor := s.staff(r)
	if _, err := s.store.GetSupplier(r.Context(), s.store.Pool, actor.MerchantID, supplierID); err != nil {
		writeLedgerError(w, err)
		return
	}
	s.finishIdempotent(w, r, actor.MerchantID, key, fingerprint(r, raw), func(tx pgx.Tx) (int, []byte, *uuid.UUID, error) {
		merchant, err := s.store.EnsureBooks(r.Context(), tx, actor.MerchantID)
		if err != nil {
			return 0, nil, nil, err
		}
		p, err := s.store.PaySupplier(r.Context(), tx, merchant, outlet, supplierID, actor.ID, amount, in.Method, note)
		if err != nil {
			return 0, nil, nil, err
		}
		body, err := json.Marshal(map[string]any{
			"id": p.ID, "supplier_id": p.SupplierID, "outlet_id": p.OutletID, "amount_minor": p.AmountMinor,
			"method": p.Method, "note": p.Note, "journal_id": p.JournalID, "created_at": p.CreatedAt,
		})
		return http.StatusCreated, body, &p.JournalID, err
	})
}

func (s *Server) supplierStatement(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseID(w, r)
	if !ok {
		return
	}
	mid := s.staff(r).MerchantID
	sp, err := s.store.GetSupplier(r.Context(), s.store.Pool, mid, id)
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	entries, received, paid, err := s.store.SupplierStatement(r.Context(), s.store.Pool, mid, id)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]any{"kind": e.Kind, "id": e.ID, "at": e.At, "amount_minor": e.AmountMinor, "detail": e.Detail})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"supplier": supplierBody(sp), "entries": out,
		"total_received_minor": received, "total_paid_minor": paid, "balance_owed_minor": received - paid,
	})
}

// supplierReceiptCost is the unit cost a supplier-linked receipt books: the stated cost, or the product average
// before the receipt (read under the product lock, ahead of checkout.Receive).
func (s *Server) supplierReceiptCost(ctx context.Context, tx pgx.Tx, merchantID, productID uuid.UUID, unit *int64) (int64, error) {
	if unit != nil {
		return *unit, nil
	}
	p, err := s.store.LockProduct(ctx, tx, merchantID, productID)
	return p.AvgCostMinor, err
}

// ---- VAT summary ----

func (s *Server) vatReport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	now := time.Now()
	from, to := now.AddDate(-1, 0, 0), now.Add(time.Second)
	if raw := strings.TrimSpace(q.Get("from")); raw != "" {
		t, ok := parseTimeParam(raw, false)
		if !ok {
			badQuery(w, "from must be RFC 3339 or YYYY-MM-DD")
			return
		}
		from = t
	}
	if raw := strings.TrimSpace(q.Get("to")); raw != "" {
		t, ok := parseTimeParam(raw, true)
		if !ok {
			badQuery(w, "to must be RFC 3339 or YYYY-MM-DD")
			return
		}
		to = t
	}
	if !to.After(from) {
		badQuery(w, "to must be after from")
		return
	}
	months, err := s.store.VATByMonth(r.Context(), s.store.Pool, s.staff(r).MerchantID, from, to)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	var out, rev int64
	list := make([]map[string]any, 0, len(months))
	for _, m := range months {
		out += m.OutputMinor
		rev += m.ReversedMinor
		list = append(list, map[string]any{
			"month": m.Month, "output_vat_minor": m.OutputMinor, "reversed_vat_minor": m.ReversedMinor,
			"net_output_vat_minor": m.OutputMinor - m.ReversedMinor,
			"input_vat_minor":      0,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"from": from, "to": to, "months": list,
		"totals": map[string]any{
			"output_vat_minor": out, "reversed_vat_minor": rev, "net_output_vat_minor": out - rev,
			"input_vat_minor": 0, "net_vat_payable_minor": out - rev,
		},
		"notes": []string{
			"Output VAT is credits to account 2100 (VAT payable) from sale journals; reversed VAT is debits to 2100 (refunds, voids, and any remittance posted by hand). Months are Africa/Lagos.",
			"Input VAT is not tracked yet and is reported as 0.",
		},
	})
}
