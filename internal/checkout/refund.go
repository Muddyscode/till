package checkout

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Muddyscode/till/internal/ledger"
	"github.com/Muddyscode/till/internal/payments"
	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RefundLine names units to take back. Exactly one of ProductID and SKU is set.
type RefundLine struct {
	ProductID *uuid.UUID
	SKU       string
	Qty       int
}

type RefundInput struct {
	Merchant store.Merchant
	Order    store.Order // locked, with lines and payments
	StaffID  uuid.UUID
	IdemKey  string
	Reason   string
	Lines    []RefundLine // empty = everything still unrefunded
	RefundTo string       // cash, wallet or transfer; empty = see allocateRefund
}

// finishRefund reloads the order and its newest refund.
func finishRefund(ctx context.Context, tx pgx.Tx, st *store.Store, o store.Order, recordID *uuid.UUID) (store.Order, store.OrderRefund, error) {
	fresh, err := st.GetOrder(ctx, tx, o.MerchantID, o.ID)
	if err != nil {
		return store.Order{}, store.OrderRefund{}, err
	}
	fresh.RefundRecordID = recordID
	list, err := st.ListOrderRefunds(ctx, tx, o.ID)
	if err != nil || len(list) == 0 {
		return store.Order{}, store.OrderRefund{}, fmt.Errorf("refund row missing: %v", err)
	}
	return fresh, list[len(list)-1], nil
}

type pick struct {
	ln                   store.OrderLine
	qty                  int
	net, vat, cost, disc int64
}

type share struct {
	pay store.Payment
	amt int64
}

// RefundOrder refunds a paid or partly refunded order, in whole or by line.
// Every refund is a journal of its own built from the line amounts stored at
// sale time:
//
//	Dr sales (net) / Dr VAT payable / Cr tender accounts, plus Dr inventory / Cr COGS
//
// A tender that came through a payment connection is credited to Refunds payable (2310) and a
// refund_pending record is opened, settled later through
// POST /v1/unmatched-payments/{id}/refunded. Units are restocked and cost
// returns at the cost recorded on the line. Each refund adds an order_refunds
// row. The order is partially_refunded until every unit is back, then refunded.
func RefundOrder(ctx context.Context, tx pgx.Tx, st *store.Store, in RefundInput) (store.Order, store.OrderRefund, error) {
	o, m := in.Order, in.Merchant
	if (o.Status != "paid" && o.Status != "partially_refunded") || o.JournalID == nil {
		return store.Order{}, store.OrderRefund{}, ledger.Err(ledger.CodeInvalidOrder, "only a paid order can be refunded")
	}
	picks, err := pickLines(o, in.Lines)
	if err != nil {
		return store.Order{}, store.OrderRefund{}, err
	}
	var net, vat, cost int64
	for _, p := range picks {
		net += p.net
		vat += p.vat
		cost += p.cost
	}
	amount := net + vat
	if amount == 0 && cost == 0 {
		return store.Order{}, store.OrderRefund{}, ledger.Err(ledger.CodeInvalidOrder, "nothing to refund on these lines")
	}
	shares, err := allocateRefund(o.Payments, amount, in.RefundTo)
	if err != nil {
		return store.Order{}, store.OrderRefund{}, err
	}
	if m.VATAccountID == nil || m.InventoryAccountID == nil || m.COGSAccountID == nil ||
		m.WalletAccountID == nil || m.RefundsPayableID == nil {
		return store.Order{}, store.OrderRefund{}, ledger.Err(ledger.CodeInvalidOrder, "merchant books are not set up")
	}

	// Products first (id order), then stock, then the customer: the same order
	// every sale and receipt uses.
	ids := make([]uuid.UUID, len(picks))
	for i, p := range picks {
		ids[i] = p.ln.ProductID
	}
	if _, err := st.LockProducts(ctx, tx, m.ID, ids); err != nil {
		return store.Order{}, store.OrderRefund{}, err
	}
	if err := st.LockStock(ctx, tx, o.OutletID, ids); err != nil {
		return store.Order{}, store.OrderRefund{}, err
	}
	var walletShare, transferShare int64
	for _, s := range shares {
		switch {
		case s.pay.Method == "wallet":
			walletShare = s.amt
		case payments.Awaited(s.pay.Method):
			transferShare = s.amt
		}
	}
	if walletShare > 0 {
		if o.CustomerID == nil {
			return store.Order{}, store.OrderRefund{}, ledger.Err(ledger.CodeInvalidOrder, "wallet refund needs a customer")
		}
		if _, err := st.LockCustomer(ctx, tx, m.ID, *o.CustomerID); err != nil {
			return store.Order{}, store.OrderRefund{}, err
		}
	}
	outlet, err := st.GetOutlet(ctx, tx, m.ID, o.OutletID)
	if err != nil {
		return store.Order{}, store.OrderRefund{}, err
	}

	var jl []ledger.Line
	if net > 0 {
		jl = append(jl, ledger.Line{AccountID: outlet.SalesAccountID, Direction: ledger.Debit, Amount: net})
	}
	if vat > 0 {
		jl = append(jl, ledger.Line{AccountID: *m.VATAccountID, Direction: ledger.Debit, Amount: vat})
	}
	tenders := map[string]int64{}
	for _, s := range shares {
		if s.amt == 0 {
			continue
		}
		tenders[s.pay.Method] = s.amt
		acct := outlet.CashAccountID
		switch {
		case s.pay.Method == "wallet":
			acct = *m.WalletAccountID
		case payments.Awaited(s.pay.Method):
			// The money came through a provider, so it has to go back that way.
			acct = *m.RefundsPayableID
		}
		jl = append(jl, ledger.Line{AccountID: acct, Direction: ledger.Credit, Amount: s.amt})
	}
	if cost > 0 {
		jl = append(jl,
			ledger.Line{AccountID: *m.InventoryAccountID, Direction: ledger.Debit, Amount: cost},
			ledger.Line{AccountID: *m.COGSAccountID, Direction: ledger.Credit, Amount: cost},
		)
	}
	j, err := ledger.Post(ctx, tx, st.Poster(), ledger.PostRequest{
		MerchantID:     m.ID,
		Description:    "refund " + o.Reference,
		Lines:          jl,
		IdempotencyKey: in.IdemKey,
	})
	if err != nil {
		if le := ledger.AsError(err); le != nil && le.Code == ledger.CodeInsufficientFunds {
			return store.Order{}, store.OrderRefund{}, ledger.Err(ledger.CodeRefundInsufficient, "refund cannot be posted: "+le.Message)
		}
		return store.Order{}, store.OrderRefund{}, err
	}

	type lineOut struct {
		ProductID uuid.UUID `json:"product_id"`
		SKU       string    `json:"sku"`
		Quantity  int       `json:"quantity"`
		Net       int64     `json:"net_minor"`
		VAT       int64     `json:"vat_minor"`
		Cost      int64     `json:"cost_minor"`
	}
	out := make([]lineOut, 0, len(picks))
	left := 0
	for _, ln := range o.Lines {
		left += ln.Qty - ln.RefundedQty
	}
	for _, p := range picks {
		if err := st.Restock(ctx, tx, o.OutletID, p.ln.ProductID, p.qty); err != nil {
			return store.Order{}, store.OrderRefund{}, err
		}
		if err := st.AddLineRefundedQty(ctx, tx, p.ln.ID, p.qty); err != nil {
			return store.Order{}, store.OrderRefund{}, err
		}
		left -= p.qty
		out = append(out, lineOut{p.ln.ProductID, p.ln.SKU, p.qty, p.net, p.vat, p.cost})
	}
	for _, s := range shares {
		if s.amt > 0 {
			if err := st.AddPaymentRefunded(ctx, tx, s.pay.ID, s.amt); err != nil {
				return store.Order{}, store.OrderRefund{}, err
			}
		}
	}
	if walletShare > 0 {
		if _, err := st.AddWalletBalance(ctx, tx, m.ID, *o.CustomerID, walletShare); err != nil {
			return store.Order{}, store.OrderRefund{}, err
		}
	}
	now := time.Now()
	refundID, err := newID()
	if err != nil {
		return store.Order{}, store.OrderRefund{}, err
	}
	var recordID *uuid.UUID
	if transferShare > 0 {
		rid, err := newID()
		if err != nil {
			return store.Order{}, store.OrderRefund{}, err
		}
		oid, outletID := o.ID, o.OutletID
		var connID *uuid.UUID
		if cid, ok, err := st.OrderConnection(ctx, tx, o.ID); err != nil {
			return store.Order{}, store.OrderRefund{}, err
		} else if ok {
			connID = &cid
		}
		if err := st.InsertUnmatched(ctx, tx, store.Unmatched{
			ID: rid, MerchantID: m.ID, OrderID: &oid, OutletID: &outletID, ConnectionID: connID,
			Provider: "refund", EventID: "refund:" + refundID.String(),
			AmountMinor: transferShare, Currency: "NGN", Reason: "order_refund",
			Status: "refund_pending", CreatedAt: now, ResolvedAt: &now, ResolvedBy: &in.StaffID, JournalID: &j.ID,
		}); err != nil {
			return store.Order{}, store.OrderRefund{}, err
		}
		recordID = &rid
	}
	rawLines, err := json.Marshal(out)
	if err != nil {
		return store.Order{}, store.OrderRefund{}, err
	}
	rawTenders, err := json.Marshal(tenders)
	if err != nil {
		return store.Order{}, store.OrderRefund{}, err
	}
	if err := st.InsertOrderRefund(ctx, tx, store.OrderRefund{
		ID: refundID, MerchantID: m.ID, OrderID: o.ID, OutletID: o.OutletID, JournalID: &j.ID,
		AmountMinor: amount, NetMinor: net, VATMinor: vat, CostMinor: cost,
		Lines: rawLines, Tenders: rawTenders, Reason: in.Reason, StaffID: in.StaffID, CreatedAt: now,
	}); err != nil {
		return store.Order{}, store.OrderRefund{}, err
	}
	status := "partially_refunded"
	if left == 0 {
		status = "refunded"
	}
	if err := st.ApplyRefund(ctx, tx, o.ID, j.ID, in.StaffID, in.Reason, now, amount, status); err != nil {
		return store.Order{}, store.OrderRefund{}, err
	}
	outletID := o.OutletID
	if err := audit(ctx, tx, st, m.ID, in.StaffID, &outletID, "order.refunded", "order", o.ID, map[string]any{
		"reference": o.Reference, "refund_id": refundID.String(), "reversal_journal_id": j.ID.String(),
		"amount_minor": amount, "partial": status == "partially_refunded", "reason": in.Reason,
	}); err != nil {
		return store.Order{}, store.OrderRefund{}, err
	}
	return finishRefund(ctx, tx, st, o, recordID)
}

// pickLines turns the request into units per order line, with each line's
// refund amounts. A line's net, VAT, discount and cost are spread over its
// units as cumulative floors, so the last units carry the remainder kobo and a
// line refunded in pieces always adds up to exactly what was sold.
func pickLines(o store.Order, req []RefundLine) ([]pick, error) {
	var picks []pick
	add := func(ln store.OrderLine, qty int) {
		part := func(v int64) int64 {
			return mulDiv(v, int64(ln.RefundedQty+qty), int64(ln.Qty)) - mulDiv(v, int64(ln.RefundedQty), int64(ln.Qty))
		}
		picks = append(picks, pick{ln: ln, qty: qty, net: part(ln.NetMinor), vat: part(ln.VATMinor), cost: part(ln.CostMinor), disc: part(ln.DiscountMinor)})
	}
	if len(req) == 0 {
		for _, ln := range o.Lines {
			if rem := ln.Qty - ln.RefundedQty; rem > 0 {
				add(ln, rem)
			}
		}
	} else {
		seen := map[uuid.UUID]bool{}
		for _, rl := range req {
			var found *store.OrderLine
			for i := range o.Lines {
				ln := &o.Lines[i]
				if (rl.ProductID != nil && ln.ProductID == *rl.ProductID) || (rl.ProductID == nil && ln.SKU == rl.SKU) {
					found = ln
					break
				}
			}
			if found == nil {
				return nil, ledger.Err(ledger.CodeInvalidOrder, "refund line is not on this order")
			}
			if seen[found.ID] {
				return nil, ledger.Err(ledger.CodeInvalidOrder, "duplicate refund line "+found.SKU)
			}
			seen[found.ID] = true
			if rl.Qty <= 0 {
				return nil, ledger.Err(ledger.CodeInvalidOrder, "quantity must be > 0")
			}
			if rl.Qty > found.Qty-found.RefundedQty {
				return nil, ledger.Err(CodeRefundExceedsOrder, fmt.Sprintf("only %d of %s left to refund", found.Qty-found.RefundedQty, found.SKU))
			}
			add(*found, rl.Qty)
		}
	}
	if len(picks) == 0 {
		return nil, ledger.Err(ledger.CodeInvalidOrder, "nothing left to refund")
	}
	return picks, nil
}

// allocateRefund decides which tenders the money goes back to. With refund_to
// the whole amount goes to that tender (it must still have that much left).
// Without it, the refund is spread over the order's tenders in proportion to
// what each still has left (so a single-tender order refunds to its own tender
// and a split order refunds pro rata); rounding kobo go to the first tenders
// with room, in cash, wallet, transfer order.
func allocateRefund(pays []store.Payment, amount int64, to string) ([]share, error) {
	var cand []store.Payment
	for _, p := range pays {
		if p.Status == "confirmed" && p.AmountMinor-p.RefundedMinor > 0 && (to == "" || p.Method == to) {
			cand = append(cand, p)
		}
	}
	if amount == 0 {
		return nil, nil
	}
	var room int64
	for _, p := range cand {
		room += p.AmountMinor - p.RefundedMinor
	}
	if to != "" && room < amount {
		return nil, ledger.Err(ledger.CodeInvalidOrder, fmt.Sprintf("refund_to %s has only %d left to refund", to, room))
	}
	if room < amount {
		return nil, ledger.Err(ledger.CodeInvalidOrder, "refund is more than was paid")
	}
	out := make([]share, len(cand))
	var given int64
	for i, p := range cand {
		out[i] = share{pay: p, amt: mulDiv(amount, p.AmountMinor-p.RefundedMinor, room)}
		given += out[i].amt
	}
	for i := 0; given < amount; i = (i + 1) % len(out) {
		if out[i].amt < out[i].pay.AmountMinor-out[i].pay.RefundedMinor {
			out[i].amt++
			given++
		}
	}
	return out, nil
}
