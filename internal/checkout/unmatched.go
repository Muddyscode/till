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
	// Order is the order the payment was matched to, if any.
	Order *store.Order
	// Connection is where the money arrived.
	Connection store.Connection
	EventID    string
	Currency   string
	Reason     string
	Amount     int64
	Now        time.Time
	// Details is who paid and how, as reported (JSON).
	Details []byte
	// StaffID is set when a person recorded the payment.
	StaffID *uuid.UUID
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

// RecordUnmatched books money a connection confirmed but that cannot be applied
// to an order: Dr the connection's clearing account / Cr Payment suspense
// (2300), plus an open unmatched_payments row for a manager to resolve.
func RecordUnmatched(ctx context.Context, tx pgx.Tx, st *store.Store, in UnmatchedInput) (store.Unmatched, error) {
	m, c := in.Merchant, in.Connection
	if m.SuspenseAccountID == nil {
		return store.Unmatched{}, ledger.Err(ledger.CodeInvalidOrder, "merchant books are not set up")
	}
	j, err := ledger.Post(ctx, tx, st.Poster(), ledger.PostRequest{
		MerchantID:     m.ID,
		Description:    "unmatched " + c.Code + " payment " + in.EventID + " (" + in.Reason + ")",
		IdempotencyKey: "wh:" + c.ID.String() + ":" + in.EventID,
		Lines: []ledger.Line{
			{AccountID: c.ClearingAccountID, Direction: ledger.Debit, Amount: in.Amount},
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
	cid := c.ID
	u := store.Unmatched{
		ID: id, MerchantID: m.ID, Provider: c.Provider, EventID: in.EventID, ConnectionID: &cid,
		AmountMinor: in.Amount, Currency: in.Currency, Reason: in.Reason, Status: "open",
		CreatedAt: in.Now, JournalID: &j.ID, Details: in.Details, OutletID: c.OutletID,
	}
	if in.Order != nil {
		oid, o := in.Order.ID, in.Order.OutletID
		u.OrderID, u.OutletID = &oid, &o
	}
	if err := st.InsertUnmatched(ctx, tx, u); err != nil {
		return store.Unmatched{}, err
	}
	extra := map[string]any{"journal_id": j.ID.String(), "connection": c.Code}
	if in.Order != nil {
		extra["reference"] = in.Order.Reference
	}
	if err := unmatchedAudit(ctx, tx, st, m.ID, in.StaffID, u.OutletID, "payment.unmatched", u, extra); err != nil {
		return store.Unmatched{}, err
	}
	return u, nil
}

// PostFee books what the provider kept from a payment: Dr 5100 Payment fees /
// Cr the connection's clearing account, so clearing shows what the provider
// really holds.
func PostFee(ctx context.Context, tx pgx.Tx, st *store.Store, c store.Connection, eventID string, fee int64) error {
	fees, err := st.EnsureAccount(ctx, tx, c.MerchantID, store.FeesAccountCode, "Payment fees", ledger.Expense, false)
	if err != nil {
		return err
	}
	_, err = ledger.Post(ctx, tx, st.Poster(), ledger.PostRequest{
		MerchantID:     c.MerchantID,
		Description:    "fee " + c.Code + " " + eventID,
		IdempotencyKey: "wh:" + c.ID.String() + ":" + eventID + ":fee",
		Lines: []ledger.Line{
			{AccountID: fees, Direction: ledger.Debit, Amount: fee},
			{AccountID: c.ClearingAccountID, Direction: ledger.Credit, Amount: fee},
		},
	})
	return err
}

// ApplyUnmatched turns an open unmatched payment into the paid sale it was
// for. The order is either still pending (the payment arrived without a usable
// reference and a manager picked the order) or expired (the money was late,
// and the stock must be available again). The amount must be exactly what the
// order was waiting for. The sale journal debits suspense instead of clearing,
// because clearing was already debited when the money arrived.
func ApplyUnmatched(ctx context.Context, tx pgx.Tx, st *store.Store, merchant store.Merchant, outlet store.Outlet, u store.Unmatched, order store.Order, staffID uuid.UUID, now time.Time) (store.Order, error) {
	if merchant.SuspenseAccountID == nil {
		return store.Order{}, ledger.Err(ledger.CodeInvalidOrder, "merchant books are not set up")
	}
	if order.Status != "expired" && order.Status != "pending_payment" {
		return store.Order{}, ledger.Err(CodeUnmatchedNotApplicable, "order is "+order.Status+"; only a pending or expired order can take this payment")
	}
	if u.Currency != "NGN" {
		return store.Order{}, ledger.Err(CodeUnmatchedNotApplicable, "payment currency is not NGN")
	}
	key := "sys:unmatched-apply:" + u.ID.String()
	extra := map[string]any{"unmatched_payment_id": u.ID.String()}

	if order.Status == "pending_payment" {
		due, ok, err := st.PendingAwaitedMinor(ctx, tx, order.ID)
		if err != nil {
			return store.Order{}, err
		}
		if !ok || u.AmountMinor != due {
			return store.Order{}, ledger.Err(CodeUnmatchedNotApplicable, "amount does not equal what the order is waiting for")
		}
		paid, err := ConfirmAwaited(ctx, tx, st, ConfirmInput{
			Merchant: merchant, Outlet: outlet, Order: order, Now: now, IdempotencyKey: key,
			ProviderEventID: u.EventID, DebitAccountID: *merchant.SuspenseAccountID,
			ConnectionID: u.ConnectionID, Provider: u.Provider, StaffID: &staffID, Extra: extra,
		})
		if err != nil {
			return store.Order{}, err
		}
		return paid, finishApply(ctx, tx, st, merchant, u, paid, staffID, now)
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
	if err := st.SetLineCosts(ctx, tx, order.Lines); err != nil {
		return store.Order{}, err
	}
	debit := []ledger.Line{{AccountID: *merchant.SuspenseAccountID, Direction: ledger.Debit, Amount: order.TotalMinor}}
	j, err := postSaleDebits(ctx, tx, st, SaleInput{Merchant: merchant, Outlet: outlet}, order, debit, key)
	if err != nil {
		return store.Order{}, err
	}
	if err := st.AttachJournal(ctx, tx, order.ID, j.ID, now); err != nil {
		return store.Order{}, err
	}
	eventID := u.EventID
	method, err := st.ConfirmPayment(ctx, tx, order.ID, j.ID, &eventID, u.ConnectionID, u.Provider)
	if err != nil {
		return store.Order{}, err
	}
	order.Status = "paid"
	order.JournalID = &j.ID
	order.PaidAt = &now
	outletID := order.OutletID
	if err := audit(ctx, tx, st, merchant.ID, staffID, &outletID, "order.paid", "order", order.ID, map[string]any{
		"reference": order.Reference, "total_minor": order.TotalMinor, "cost_minor": cost, "method": method,
		"unmatched_payment_id": u.ID.String(),
	}); err != nil {
		return store.Order{}, err
	}
	return order, finishApply(ctx, tx, st, merchant, u, order, staffID, now)
}

func finishApply(ctx context.Context, tx pgx.Tx, st *store.Store, merchant store.Merchant, u store.Unmatched, paid store.Order, staffID uuid.UUID, now time.Time) error {
	if err := st.TransitionUnmatched(ctx, tx, u.ID, "open", "applied", staffID, now, paid.JournalID, nil); err != nil {
		return err
	}
	if err := st.SetUnmatchedOrder(ctx, tx, u.ID, paid.ID, paid.OutletID); err != nil {
		return err
	}
	outletID := paid.OutletID
	return unmatchedAudit(ctx, tx, st, merchant.ID, &staffID, &outletID, "payment.unmatched_applied", u,
		map[string]any{"journal_id": paid.JournalID.String(), "reference": paid.Reference})
}

// RefundUnmatched queues the customer's money for payout: Dr Payment suspense
// (2300) / Cr Refunds payable (2310). Nothing leaves the bank yet.
func RefundUnmatched(ctx context.Context, tx pgx.Tx, st *store.Store, merchant store.Merchant, u store.Unmatched, staffID uuid.UUID, now time.Time) error {
	if merchant.SuspenseAccountID == nil || merchant.RefundsPayableID == nil {
		return ledger.Err(ledger.CodeInvalidOrder, "merchant books are not set up")
	}
	j, err := ledger.Post(ctx, tx, st.Poster(), ledger.PostRequest{
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
// (2310) / Cr the clearing account of the connection the money came in on. It
// serves both unmatched payments queued for refund and refunded orders.
func SettleRefund(ctx context.Context, tx pgx.Tx, st *store.Store, merchant store.Merchant, u store.Unmatched, staffID uuid.UUID, now time.Time) error {
	if merchant.ClearingAccountID == nil || merchant.RefundsPayableID == nil {
		return ledger.Err(ledger.CodeInvalidOrder, "merchant books are not set up")
	}
	clearing := *merchant.ClearingAccountID
	if u.ConnectionID != nil {
		c, err := st.GetConnection(ctx, tx, merchant.ID, *u.ConnectionID)
		if err != nil {
			return err
		}
		clearing = c.ClearingAccountID
	}
	j, err := ledger.Post(ctx, tx, st.Poster(), ledger.PostRequest{
		MerchantID:     merchant.ID,
		Description:    "refund paid out " + u.EventID,
		IdempotencyKey: "sys:unmatched-settle:" + u.ID.String(),
		Lines: []ledger.Line{
			{AccountID: *merchant.RefundsPayableID, Direction: ledger.Debit, Amount: u.AmountMinor},
			{AccountID: clearing, Direction: ledger.Credit, Amount: u.AmountMinor},
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
