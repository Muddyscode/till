package httpserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Muddyscode/till/internal/auth"
	"github.com/Muddyscode/till/internal/checkout"
	"github.com/Muddyscode/till/internal/ledger"
	"github.com/Muddyscode/till/internal/money"
	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type orderLineReq struct {
	SKU string `json:"sku"`
	Qty int    `json:"qty"`
}

type orderReq struct {
	OutletID      string          `json:"outlet_id"`
	PaymentMethod string          `json:"payment_method"`
	Lines         []orderLineReq  `json:"lines"`
	DiscountMinor json.RawMessage `json:"discount_minor"`
	CustomerID    string          `json:"customer_id"`
	// DiscountReason is required when discount_minor > 0.
	DiscountReason string `json:"discount_reason"`
	// Payments is split tender; use it instead of payment_method.
	Payments []paymentReq `json:"payments"`
}

func orderBody(o store.Order) map[string]any {
	lines := make([]map[string]any, 0, len(o.Lines))
	for _, ln := range o.Lines {
		lines = append(lines, map[string]any{
			"product_id": ln.ProductID, "sku": ln.SKU, "qty": ln.Qty,
			"unit_price_minor": ln.UnitPriceMinor, "line_total_minor": ln.LineTotalMinor,
			"tax_class": ln.TaxClass, "discount_minor": ln.DiscountMinor, "vat_minor": ln.VATMinor,
			"net_minor": ln.NetMinor, "refunded_qty": ln.RefundedQty,
		})
	}
	payments := make([]map[string]any, 0, len(o.Payments))
	for _, p := range o.Payments {
		payments = append(payments, map[string]any{
			"method": p.Method, "status": p.Status, "amount_minor": p.AmountMinor, "refunded_minor": p.RefundedMinor,
		})
	}
	body := map[string]any{
		"id": o.ID, "outlet_id": o.OutletID, "status": o.Status,
		"payment_method": o.PaymentMethod, "reference": o.Reference,
		"total_minor": o.TotalMinor, "vat_minor": o.VATMinor, "net_minor": o.NetMinor,
		"discount_minor": o.DiscountMinor, "cost_minor": o.CostMinor,
		"currency": "NGN", "lines": lines, "payments": payments, "created_at": o.CreatedAt,
		"refunded_minor": o.RefundedMinor,
	}
	if o.DiscountReason != "" {
		body["discount_reason"] = o.DiscountReason
	}
	if o.CustomerID != nil {
		body["customer_id"] = o.CustomerID
	}
	if o.JournalID != nil {
		body["journal_id"] = o.JournalID
	}
	if o.ExpiresAt != nil {
		body["expires_at"] = o.ExpiresAt
	}
	if o.PaidAt != nil {
		body["paid_at"] = o.PaidAt
	}
	if o.RefundJournalID != nil {
		body["refund_journal_id"] = o.RefundJournalID
	}
	if o.RefundedAt != nil {
		body["refunded_at"] = o.RefundedAt
	}
	if o.RefundedBy != nil {
		body["refunded_by"] = o.RefundedBy
	}
	if o.RefundReason != "" {
		body["refund_reason"] = o.RefundReason
	}
	if o.RefundRecordID != nil {
		body["refund_record_id"] = o.RefundRecordID
	}
	return body
}

func (s *Server) createOrder(w http.ResponseWriter, r *http.Request) {
	key, ok := requireIdempotencyKey(w, r)
	if !ok {
		return
	}
	raw, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var in orderReq
	if err := decodeStrict(raw, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid JSON")
		return
	}
	method := strings.ToLower(strings.TrimSpace(in.PaymentMethod))
	var tenders []checkout.Tender
	split := len(in.Payments) > 0
	if split {
		if method != "" || strings.TrimSpace(in.CustomerID) != "" {
			writeError(w, http.StatusBadRequest, ledger.CodeInvalidOrder, "use either payment_method or payments, not both; give customer_id inside the wallet payment")
			return
		}
		var msg string
		if tenders, msg = parseTenders(in.Payments); msg != "" {
			writeError(w, http.StatusBadRequest, ledger.CodeInvalidOrder, msg)
			return
		}
	} else if method != "cash" && method != "transfer" && method != "wallet" {
		writeError(w, http.StatusBadRequest, ledger.CodeInvalidOrder, "payment_method must be cash, transfer, or wallet")
		return
	}
	discount, err := money.ParseOptionalKobo(in.DiscountMinor)
	if err != nil {
		writeError(w, http.StatusBadRequest, ledger.CodeInvalidAmount, "discount_minor must be a JSON integer in kobo")
		return
	}
	var customerID *uuid.UUID
	if strings.TrimSpace(in.CustomerID) != "" {
		id, err := uuid.Parse(strings.TrimSpace(in.CustomerID))
		if err != nil {
			writeError(w, http.StatusBadRequest, ledger.CodeInvalidOrder, "customer_id is invalid")
			return
		}
		customerID = &id
	}
	if !split && method == "wallet" && customerID == nil {
		writeError(w, http.StatusBadRequest, ledger.CodeInvalidOrder, "wallet payment needs a customer_id")
		return
	}
	if !split && method != "wallet" && customerID != nil {
		writeError(w, http.StatusBadRequest, ledger.CodeInvalidOrder, "customer_id is only used for wallet payments")
		return
	}
	outletID, err := uuid.Parse(in.OutletID)
	if err != nil {
		writeError(w, http.StatusBadRequest, ledger.CodeInvalidOrder, "outlet_id is invalid")
		return
	}
	outlet, ok := s.authorizeOutlet(w, r, outletID)
	if !ok {
		return
	}
	if len(in.Lines) == 0 {
		writeError(w, http.StatusBadRequest, ledger.CodeInvalidOrder, "order needs at least one line")
		return
	}
	actor := s.staff(r)
	saleLines := make([]checkout.SaleLine, 0, len(in.Lines))
	for _, ln := range in.Lines {
		p, err := s.store.GetProductBySKU(r.Context(), s.store.Pool, actor.MerchantID, normalizeSKU(ln.SKU))
		if err != nil {
			writeLedgerError(w, err)
			return
		}
		saleLines = append(saleLines, checkout.SaleLine{Product: p, Qty: ln.Qty})
	}
	hash := fingerprint(r, raw)
	s.finishIdempotent(w, r, actor.MerchantID, key, hash, func(tx pgx.Tx) (int, []byte, *uuid.UUID, error) {
		merchant, err := s.store.EnsureBooks(r.Context(), tx, actor.MerchantID)
		if err != nil {
			return 0, nil, nil, err
		}
		inSale := checkout.SaleInput{
			Merchant: merchant, Outlet: outlet, StaffID: actor.ID, Lines: saleLines,
			Now: time.Now(), Hold: time.Duration(s.cfg.HoldMinutes) * time.Minute,
			IdempotencyKey: key, DiscountMinor: discount, CustomerID: customerID,
			Role: actor.Role, DiscountReason: in.DiscountReason,
		}
		var order store.Order
		switch {
		case split:
			order, err = checkout.SaleWithTenders(r.Context(), tx, s.store, inSale, tenders)
		case method == "cash":
			order, err = checkout.CashSale(r.Context(), tx, s.store, inSale)
		case method == "wallet":
			order, err = checkout.WalletSale(r.Context(), tx, s.store, inSale)
		default:
			order, err = checkout.ReserveTransfer(r.Context(), tx, s.store, inSale)
		}
		if err != nil {
			return 0, nil, nil, err
		}
		body, err := json.Marshal(orderBody(order))
		if err != nil {
			return 0, nil, nil, err
		}
		return http.StatusCreated, body, order.JournalID, nil
	})
}

func (s *Server) getOrder(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseID(w, r)
	if !ok {
		return
	}
	o, err := s.store.GetOrder(r.Context(), s.store.Pool, s.staff(r).MerchantID, id)
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	if _, ok := s.authorizeOutlet(w, r, o.OutletID); !ok {
		return
	}
	writeJSON(w, http.StatusOK, orderBody(o))
}

func (s *Server) listOrders(w http.ResponseWriter, r *http.Request) {
	st := s.staff(r)
	var outletID *uuid.UUID
	if raw := strings.TrimSpace(r.URL.Query().Get("outlet_id")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_order", "outlet_id is invalid")
			return
		}
		if _, ok := s.authorizeOutlet(w, r, id); !ok {
			return
		}
		outletID = &id
	} else if st.Role != auth.RoleAdmin {
		writeError(w, http.StatusBadRequest, "invalid_order", "outlet_id is required")
		return
	}
	list, err := s.store.ListOrders(r.Context(), s.store.Pool, st.MerchantID, outletID, limitParam(r, 50, 200))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, o := range list {
		out = append(out, orderBody(o))
	}
	writeJSON(w, http.StatusOK, map[string]any{"orders": out})
}

type refundLineReq struct {
	ProductID string `json:"product_id"`
	SKU       string `json:"sku"`
	Quantity  *int   `json:"quantity"`
	Qty       *int   `json:"qty"`
}

// refundOrder takes back a whole order (no lines) or the listed lines, any
// number of times until nothing is left. See checkout.RefundOrder.
func (s *Server) refundOrder(w http.ResponseWriter, r *http.Request) {
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
		Reason   string          `json:"reason"`
		Lines    []refundLineReq `json:"lines"`
		RefundTo string          `json:"refund_to"`
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
	refundTo := strings.ToLower(strings.TrimSpace(in.RefundTo))
	if refundTo != "" && refundTo != "cash" && refundTo != "wallet" && refundTo != "transfer" {
		writeError(w, http.StatusBadRequest, ledger.CodeInvalidOrder, "refund_to must be cash, wallet, or transfer")
		return
	}
	lines := make([]checkout.RefundLine, 0, len(in.Lines))
	for _, l := range in.Lines {
		rl := checkout.RefundLine{SKU: normalizeSKU(l.SKU)}
		if (l.ProductID == "") == (rl.SKU == "") {
			writeError(w, http.StatusBadRequest, ledger.CodeInvalidOrder, "each refund line needs exactly one of product_id or sku")
			return
		}
		if l.ProductID != "" {
			pid, err := uuid.Parse(strings.TrimSpace(l.ProductID))
			if err != nil {
				writeError(w, http.StatusBadRequest, ledger.CodeInvalidOrder, "product_id is invalid")
				return
			}
			rl.ProductID = &pid
		}
		if (l.Quantity == nil) == (l.Qty == nil) {
			writeError(w, http.StatusBadRequest, ledger.CodeInvalidOrder, "each refund line needs a quantity")
			return
		}
		if l.Quantity != nil {
			rl.Qty = *l.Quantity
		} else {
			rl.Qty = *l.Qty
		}
		lines = append(lines, rl)
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
		merchant, err := s.store.EnsureBooks(r.Context(), tx, actor.MerchantID)
		if err != nil {
			return 0, nil, nil, err
		}
		order, err := s.store.LockOrder(r.Context(), tx, actor.MerchantID, id)
		if err != nil {
			return 0, nil, nil, err
		}
		order, rf, err := checkout.RefundOrder(r.Context(), tx, s.store, checkout.RefundInput{
			Merchant: merchant, Order: order, StaffID: actor.ID, IdemKey: key,
			Reason: reason, Lines: lines, RefundTo: refundTo,
		})
		if err != nil {
			return 0, nil, nil, err
		}
		out := orderBody(order)
		out["refund"] = refundBody(rf)
		body, err := json.Marshal(out)
		if err != nil {
			return 0, nil, nil, err
		}
		return http.StatusCreated, body, rf.JournalID, nil
	})
}
