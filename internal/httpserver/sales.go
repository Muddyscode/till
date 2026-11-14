package httpserver

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Muddyscode/till/internal/checkout"
	"github.com/Muddyscode/till/internal/ledger"
	"github.com/Muddyscode/till/internal/money"
	"github.com/Muddyscode/till/internal/payments"
	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// paymentReq is one tender of a split payment.
type paymentReq struct {
	Method      string          `json:"method"`
	AmountMinor json.RawMessage `json:"amount_minor"`
	CustomerID  string          `json:"customer_id"`
}

// parseTenders validates the shape of a payments array. That the amounts add
// up to the payable is checked once the basket is priced under lock.
func parseTenders(in []paymentReq) ([]checkout.Tender, string) {
	if len(in) > 3 {
		return nil, "payments can have at most one cash, one wallet and one awaited tender"
	}
	seen := map[string]bool{}
	out := make([]checkout.Tender, 0, len(in))
	for _, p := range in {
		m := strings.ToLower(strings.TrimSpace(p.Method))
		kind := m
		if payments.Awaited(m) {
			kind = "awaited"
		} else if m != "cash" && m != "wallet" {
			return nil, "payment method must be cash, wallet, " + strings.Join(payments.AwaitedMethods, ", ")
		}
		if seen[kind] {
			return nil, "payments can have at most one cash, one wallet and one awaited tender"
		}
		seen[kind] = true
		amt, err := money.ParseJSONAmount(p.AmountMinor)
		if err != nil {
			return nil, "each payment needs a positive integer amount_minor"
		}
		t := checkout.Tender{Method: m, Amount: amt}
		cid := strings.TrimSpace(p.CustomerID)
		switch {
		case m == "wallet" && cid == "":
			return nil, "wallet payment needs a customer_id"
		case m != "wallet" && cid != "":
			return nil, "customer_id is only used for wallet payments"
		case cid != "":
			id, err := uuid.Parse(cid)
			if err != nil {
				return nil, "customer_id is invalid"
			}
			t.CustomerID = &id
		}
		out = append(out, t)
	}
	return out, ""
}

func refundBody(r store.OrderRefund) map[string]any {
	body := map[string]any{
		"id": r.ID, "order_id": r.OrderID, "amount_minor": r.AmountMinor,
		"net_minor": r.NetMinor, "vat_minor": r.VATMinor, "cost_minor": r.CostMinor,
		"lines": jsonRaw(r.Lines), "tenders": jsonRaw(r.Tenders),
		"staff_id": r.StaffID, "created_at": r.CreatedAt,
	}
	if r.JournalID != nil {
		body["journal_id"] = r.JournalID
	}
	if r.Reason != "" {
		body["reason"] = r.Reason
	}
	return body
}

// voidOrder cancels a pending transfer or split order. Cashiers, managers and
// admins may do it at their own outlets.
func (s *Server) voidOrder(w http.ResponseWriter, r *http.Request) {
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
	var in struct {
		Reason string `json:"reason"`
	}
	if err := decodeStrict(raw, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid JSON")
		return
	}
	reason := strings.TrimSpace(in.Reason)
	if len(reason) > 200 {
		writeError(w, http.StatusBadRequest, ledger.CodeInvalidOrder, "reason is too long")
		return
	}
	actor := s.staff(r)
	existing, err := s.store.GetOrder(r.Context(), s.store.Pool, actor.MerchantID, id)
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	if _, ok := s.authorizeOutlet(w, r, existing.OutletID); !ok {
		return
	}
	s.finishIdempotent(w, r, actor.MerchantID, key, fingerprint(r, raw), func(tx pgx.Tx) (int, []byte, *uuid.UUID, error) {
		order, err := s.store.LockOrder(r.Context(), tx, actor.MerchantID, id)
		if err != nil {
			return 0, nil, nil, err
		}
		order, err = checkout.VoidOrder(r.Context(), tx, s.store, order, actor.ID, reason)
		if err != nil {
			return 0, nil, nil, err
		}
		fresh, err := s.store.GetOrder(r.Context(), tx, actor.MerchantID, order.ID)
		if err != nil {
			return 0, nil, nil, err
		}
		body, err := json.Marshal(orderBody(fresh))
		if err != nil {
			return 0, nil, nil, err
		}
		return http.StatusOK, body, nil, nil
	})
}

func settingsBody(m store.Merchant) map[string]any {
	return map[string]any{
		"vat_registered":           m.VATRegistered,
		"vat_inclusive_bps":        m.VATInclusiveBps,
		"max_discount_bps_cashier": m.MaxDiscountBpsCashier,
		"max_discount_bps_manager": m.MaxDiscountBpsManager,
	}
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	m, err := s.store.GetMerchant(r.Context(), s.store.Pool, s.staff(r).MerchantID)
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, settingsBody(m))
}

// patchSettings changes the discount limits and VAT settings. Admin only.
// Orders already placed keep the VAT they were sold with.
func (s *Server) patchSettings(w http.ResponseWriter, r *http.Request) {
	raw, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var in struct {
		VATRegistered         *bool `json:"vat_registered"`
		VATInclusiveBps       *int  `json:"vat_inclusive_bps"`
		MaxDiscountBpsCashier *int  `json:"max_discount_bps_cashier"`
		MaxDiscountBpsManager *int  `json:"max_discount_bps_manager"`
	}
	if err := decodeStrict(raw, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid JSON")
		return
	}
	if in.VATRegistered == nil && in.VATInclusiveBps == nil && in.MaxDiscountBpsCashier == nil && in.MaxDiscountBpsManager == nil {
		writeError(w, http.StatusBadRequest, ledger.CodeInvalidOrder, "nothing to change")
		return
	}
	inRange := func(v *int, lo, hi int) bool { return v == nil || (*v >= lo && *v <= hi) }
	if !inRange(in.VATInclusiveBps, 0, 5000) {
		writeError(w, http.StatusBadRequest, ledger.CodeInvalidOrder, "vat_inclusive_bps must be between 0 and 5000")
		return
	}
	if !inRange(in.MaxDiscountBpsCashier, 0, 10000) || !inRange(in.MaxDiscountBpsManager, 0, 10000) {
		writeError(w, http.StatusBadRequest, ledger.CodeInvalidOrder, "discount limits must be between 0 and 10000 bps")
		return
	}
	actor := s.staff(r)
	var updated store.Merchant
	err := s.withTx(r.Context(), func(tx pgx.Tx) error {
		var err error
		updated, err = s.store.UpdateMerchantSettings(r.Context(), tx, actor.MerchantID, store.SettingsPatch{
			VATRegistered: in.VATRegistered, VATInclusiveBps: in.VATInclusiveBps,
			MaxDiscountBpsCashier: in.MaxDiscountBpsCashier, MaxDiscountBpsManager: in.MaxDiscountBpsManager,
		})
		if err != nil {
			return err
		}
		meta, _ := json.Marshal(settingsBody(updated))
		mid, sid := actor.MerchantID, actor.ID
		return s.store.InsertAudit(r.Context(), tx, store.AuditEvent{
			MerchantID: &mid, StaffID: &sid, Action: "merchant.settings_updated", EntityType: "merchant",
			EntityID: actor.MerchantID, Metadata: meta,
		})
	})
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, settingsBody(updated))
}
