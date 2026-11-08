package checkout

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Muddyscode/till/internal/ledger"
	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const CodeUnmatchedNotApplicable = "unmatched_not_applicable"

type UnmatchedInput struct {
	Merchant store.Merchant
	// Order is the order the provider reference matched, if any.
	Order    *store.Order
	Provider string
	EventID  string
	Currency string
	Reason   string
	Amount   int64
	Now      time.Time
}

func unmatchedAudit(ctx context.Context, tx pgx.Tx, st *store.Store, merchantID uuid.UUID, staffID *uuid.UUID, outletID *uuid.UUID, action string, u store.Unmatched, extra map[string]any) error {
	meta := map[string]any{
		"amount_minor": u.AmountMinor, "currency": u.Currency, "reason": u.Reason, "provider": u.Provider, "event_id": u.EventID,
	}
	for k, v := range extra {
		meta[k] = v
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return st.InsertAudit(ctx, tx, store.AuditEvent{
		MerchantID: &merchantID, StaffID: staffID, Action: action, EntityType: "unmatched_payment",
		EntityID: u.ID, OutletID: outletID, Metadata: raw,
	})
}

// RecordUnmatched books money the provider confirmed but that cannot be applied
// to its order: Dr Paystack clearing (1100) / Cr Payment suspense (2300), plus
// an open unmatched_payments row for a manager to resolve.
func RecordUnmatched(ctx context.Context, tx pgx.Tx, st *store.Store, in UnmatchedInput) (store.Unmatched, error) {
	m := in.Merchant
	if m.ClearingAccountID == nil || m.SuspenseAccountID == nil {
		return store.Unmatched{}, ledger.Err(ledger.CodeInvalidOrder, "merchant books are not set up")
	}
	j, err := ledger.Post(ctx, tx, poster(st), ledger.PostRequest{
		MerchantID:     m.ID,
		Description:    "unmatched " + in.Provider + " payment " + in.EventID + " (" + in.Reason + ")",
		IdempotencyKey: "wh:" + in.Provider + ":" + in.EventID,
		Lines: []ledger.Line{
			{AccountID: *m.ClearingAccountID, Direction: ledger.Debit, Amount: in.Amount},
			{AccountID: *m.SuspenseAccountID, Direction: ledger.Credit, Amount: in.Amount},
		},
	})
	if err != nil {
		return store.Unmatched{}, err
	}
	id, err := newID()
	if err != nil {
		return store.Unmatched{}, err
	}
	u := store.Unmatched{
		ID: id, MerchantID: m.ID, Provider: in.Provider, EventID: in.EventID,
		AmountMinor: in.Amount, Currency: in.Currency, Reason: in.Reason, Status: "open",
		CreatedAt: in.Now, JournalID: &j.ID,
	}
	var outlet *uuid.UUID
	if in.Order != nil {
		oid, o := in.Order.ID, in.Order.OutletID
		u.OrderID, u.OutletID, outlet = &oid, &o, &o
	}
	if err := st.InsertUnmatched(ctx, tx, u); err != nil {
		return store.Unmatched{}, err
	}
	extra := map[string]any{"journal_id": j.ID.String()}
	if in.Order != nil {
		extra["reference"] = in.Order.Reference
	}
	if err := unmatchedAudit(ctx, tx, st, m.ID, nil, outlet, "payment.unmatched", u, extra); err != nil {
		return store.Unmatched{}, err
	}
	return u, nil
}

// ApplyUnmatched turns an open unmatched payment into the paid sale it was
// meant for. The order must have expired (stock was released), the amount must
// equal the payable total and the stock must be available again. The sale
// journal debits suspense instead of clearing, because clearing was already
// debited when the money arrived.
func ApplyUnmatched(ctx context.Context, tx pgx.Tx, st *store.Store, merchant store.Merchant, outlet store.Outlet, u store.Unmatched, order store.Order, staffID uuid.UUID, now time.Time) (store.Order, error) {
	if merchant.SuspenseAccountID == nil {
		return store.Order{}, ledger.Err(ledger.CodeInvalidOrder, "merchant books are not set up")
	}
	if order.Status != "expired" {
		return store.Order{}, ledger.Err(CodeUnmatchedNotApplicable, "order is "+order.Status+"; only an expired order can take a late payment")
	}
	if u.Currency != "NGN" {
		return store.Order{}, ledger.Err(CodeUnmatchedNotApplicable, "payment currency is not NGN")
	}
	if u.AmountMinor != order.TotalMinor {
		return store.Order{}, ledger.Err(CodeUnmatchedNotApplicable, "amount does not equal the order total")
	}
	cost, err := st.LockSaleCost(ctx, tx, merchant.ID, order.Lines)
	if err != nil {
		return store.Order{}, err
	}
	ids := make([]uuid.UUID, len(order.Lines))
	for i, ln := range order.Lines {
		ids[i] = ln.ProductID
	}
	if err := st.LockStock(ctx, tx, order.OutletID, ids); err != nil {
		return store.Order{}, err
	}
	for _, ln := range order.Lines {
		if err := st.DeductStock(ctx, tx, order.OutletID, ln.ProductID, ln.Qty); err != nil {
			return store.Order{}, err
		}
	}
	order.CostMinor = cost
	if err := st.SetOrderCost(ctx, tx, order.ID, cost); err != nil {
		return store.Order{}, err
	}
	j, err := postSale(ctx, tx, st, SaleInput{Merchant: merchant, Outlet: outlet}, order, *merchant.SuspenseAccountID, "sys:unmatched-apply:"+u.ID.String())
	if err != nil {
		return store.Order{}, err
	}
	if err := st.AttachJournal(ctx, tx, order.ID, j.ID, now); err != nil {
		return store.Order{}, err
	}
	eventID := u.EventID
	if err := st.ConfirmPayment(ctx, tx, order.ID, j.ID, &eventID); err != nil {
		return store.Order{}, err
	}
	if err := st.TransitionUnmatched(ctx, tx, u.ID, "open", "applied", staffID, now, &j.ID, nil); err != nil {
		return store.Order{}, err
	}
	order.Status = "paid"
	order.JournalID = &j.ID
	order.PaidAt = &now
	outletID := order.OutletID
	if err := audit(ctx, tx, st, merchant.ID, staffID, &outletID, "order.paid", "order", order.ID, map[string]any{
		"reference": order.Reference, "total_minor": order.TotalMinor, "cost_minor": cost, "method": "transfer",
		"unmatched_payment_id": u.ID.String(),
	}); err != nil {
		return store.Order{}, err
	}
	if err := unmatchedAudit(ctx, tx, st, merchant.ID, &staffID, &outletID, "payment.unmatched_applied", u, map[string]any{"journal_id": j.ID.String()}); err != nil {
		return store.Order{}, err
	}
	return order, nil
}

// RefundUnmatched queues the customer's money for payout: Dr Payment suspense
// (2300) / Cr Refunds payable (2310). Nothing leaves the bank yet.
func RefundUnmatched(ctx context.Context, tx pgx.Tx, st *store.Store, merchant store.Merchant, u store.Unmatched, staffID uuid.UUID, now time.Time) error {
	if merchant.SuspenseAccountID == nil || merchant.RefundsPayableID == nil {
		return ledger.Err(ledger.CodeInvalidOrder, "merchant books are not set up")
	}
	j, err := ledger.Post(ctx, tx, poster(st), ledger.PostRequest{
		MerchantID:     merchant.ID,
		Description:    "refund unmatched payment " + u.EventID,
		IdempotencyKey: "sys:unmatched-refund:" + u.ID.String(),
		Lines: []ledger.Line{
			{AccountID: *merchant.SuspenseAccountID, Direction: ledger.Debit, Amount: u.AmountMinor},
			{AccountID: *merchant.RefundsPayableID, Direction: ledger.Credit, Amount: u.AmountMinor},
		},
	})
	if err != nil {
		return err
	}
	if err := st.TransitionUnmatched(ctx, tx, u.ID, "open", "refund_pending", staffID, now, &j.ID, nil); err != nil {
		return err
	}
	return unmatchedAudit(ctx, tx, st, merchant.ID, &staffID, u.OutletID, "payment.unmatched_refund_pending", u, map[string]any{"journal_id": j.ID.String()})
}

// SettleRefund records that the refund left the provider: Dr Refunds payable
// (2310) / Cr Paystack clearing (1100). It serves both unmatched payments
// queued for refund and refunded transfer orders.
func SettleRefund(ctx context.Context, tx pgx.Tx, st *store.Store, merchant store.Merchant, u store.Unmatched, staffID uuid.UUID, now time.Time) error {
	if merchant.ClearingAccountID == nil || merchant.RefundsPayableID == nil {
		return ledger.Err(ledger.CodeInvalidOrder, "merchant books are not set up")
	}
	j, err := ledger.Post(ctx, tx, poster(st), ledger.PostRequest{
		MerchantID:     merchant.ID,
		Description:    "refund paid out " + u.EventID,
		IdempotencyKey: "sys:unmatched-settle:" + u.ID.String(),
		Lines: []ledger.Line{
			{AccountID: *merchant.RefundsPayableID, Direction: ledger.Debit, Amount: u.AmountMinor},
			{AccountID: *merchant.ClearingAccountID, Direction: ledger.Credit, Amount: u.AmountMinor},
		},
	})
	if err != nil {
		return err
	}
	if err := st.TransitionUnmatched(ctx, tx, u.ID, "refund_pending", "refunded", staffID, now, nil, &j.ID); err != nil {
		return err
	}
	return unmatchedAudit(ctx, tx, st, merchant.ID, &staffID, u.OutletID, "payment.refund_settled", u, map[string]any{"journal_id": j.ID.String()})
}
