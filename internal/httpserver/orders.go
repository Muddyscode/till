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
}

func orderBody(o store.Order) map[string]any {
	lines := make([]map[string]any, 0, len(o.Lines))
	for _, ln := range o.Lines {
		lines = append(lines, map[string]any{
			"sku": ln.SKU, "qty": ln.Qty,
			"unit_price_minor": ln.UnitPriceMinor, "line_total_minor": ln.LineTotalMinor,
		})
	}
	body := map[string]any{
		"id": o.ID, "outlet_id": o.OutletID, "status": o.Status,
		"payment_method": o.PaymentMethod, "reference": o.Reference,
		"total_minor": o.TotalMinor, "vat_minor": o.VATMinor, "net_minor": o.NetMinor,
		"discount_minor": o.DiscountMinor, "cost_minor": o.CostMinor,
		"currency": "NGN", "lines": lines, "created_at": o.CreatedAt,
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
	if method != "cash" && method != "transfer" && method != "wallet" {
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
	if method == "wallet" && customerID == nil {
		writeError(w, http.StatusBadRequest, ledger.CodeInvalidOrder, "wallet payment needs a customer_id")
		return
	}
	if method != "wallet" && customerID != nil {
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
		}
		var order store.Order
		switch method {
		case "cash":
			order, err = checkout.CashSale(r.Context(), tx, s.store, inSale)
		case "wallet":
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
		merchant, err := s.store.EnsureBooks(r.Context(), tx, actor.MerchantID)
		if err != nil {
			return 0, nil, nil, err
		}
		order, err := s.store.LockOrder(r.Context(), tx, actor.MerchantID, id)
		if err != nil {
			return 0, nil, nil, err
		}
		order, err = checkout.Refund(r.Context(), tx, s.store, merchant, order, actor.ID, key, reason)
		if err != nil {
			return 0, nil, nil, err
		}
		body, err := json.Marshal(orderBody(order))
		if err != nil {
			return 0, nil, nil, err
		}
		return http.StatusCreated, body, order.RefundJournalID, nil
	})
}
