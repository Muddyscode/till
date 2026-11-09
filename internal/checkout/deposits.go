package checkout

import (
	"context"

	"github.com/Muddyscode/till/internal/ledger"
	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// returnDeposits gives back the cash and wallet tenders a pending split order
// is holding: Dr Deposits held (2400) / Cr outlet cash and Cr customer wallet
// (2200), and the wallet balance is restored. It does nothing when the order
// holds no deposits. The caller has locked the order and its stock.
func returnDeposits(ctx context.Context, tx pgx.Tx, st *store.Store, o store.Order) error {
	held, err := st.HeldPayments(ctx, tx, o.ID)
	if err != nil || len(held) == 0 {
		return err
	}
	merchant, err := st.EnsureBooks(ctx, tx, o.MerchantID)
	if err != nil {
		return err
	}
	outlet, err := st.GetOutlet(ctx, tx, o.MerchantID, o.OutletID)
	if err != nil {
		return err
	}
	dep, err := st.EnsureDepositsAccount(ctx, tx, o.MerchantID)
	if err != nil {
		return err
	}
	var total int64
	var credits []ledger.Line
	for _, h := range held {
		total += h.AmountMinor
		switch h.Method {
		case "cash":
			credits = append(credits, ledger.Line{AccountID: outlet.CashAccountID, Direction: ledger.Credit, Amount: h.AmountMinor})
		case "wallet":
			if o.CustomerID == nil || merchant.WalletAccountID == nil {
				return ledger.Err(ledger.CodeInvalidOrder, "wallet deposit has no customer")
			}
			if _, err := st.LockCustomer(ctx, tx, o.MerchantID, *o.CustomerID); err != nil {
				return err
			}
			if _, err := st.AddWalletBalance(ctx, tx, o.MerchantID, *o.CustomerID, h.AmountMinor); err != nil {
				return err
			}
			credits = append(credits, ledger.Line{AccountID: *merchant.WalletAccountID, Direction: ledger.Credit, Amount: h.AmountMinor})
		default:
			return ledger.Err(ledger.CodeInvalidOrder, "unexpected held tender "+h.Method)
		}
	}
	lines := append([]ledger.Line{{AccountID: dep, Direction: ledger.Debit, Amount: total}}, credits...)
	j, err := ledger.Post(ctx, tx, poster(st), ledger.PostRequest{
		MerchantID:     o.MerchantID,
		Description:    "deposits returned " + o.Reference,
		Lines:          lines,
		IdempotencyKey: "sys:deposit-return:" + o.ID.String(),
	})
	if err != nil {
		if le := ledger.AsError(err); le != nil && le.Code == ledger.CodeInsufficientFunds {
			return ledger.Err(ledger.CodeRefundInsufficient, "deposits cannot be returned: "+le.Message)
		}
		return err
	}
	return st.ReturnHeld(ctx, tx, o.ID, j.ID)
}

// VoidOrder cancels a pending (transfer or split) order before it is paid: the
// stock reservation is released, held cash and wallet tenders are returned and
// the order becomes voided. A transfer that arrives later lands in unmatched
// payments (order_voided). The caller has locked the order.
func VoidOrder(ctx context.Context, tx pgx.Tx, st *store.Store, o store.Order, staffID uuid.UUID, reason string) (store.Order, error) {
	if o.Status != "pending_payment" {
		return store.Order{}, ledger.Err(ledger.CodeInvalidOrder, "only a pending order can be voided")
	}
	res, err := st.ReservationsByOrder(ctx, tx, o.ID)
	if err != nil {
		return store.Order{}, err
	}
	ids := make([]uuid.UUID, 0, len(res))
	for _, r := range res {
		ids = append(ids, r.ProductID)
	}
	if len(ids) > 0 {
		if err := st.LockStock(ctx, tx, o.OutletID, ids); err != nil {
			return store.Order{}, err
		}
	}
	for _, r := range res {
		if r.Status != "active" {
			continue
		}
		if err := st.ReleaseReserved(ctx, tx, o.OutletID, r.ProductID, r.Qty); err != nil {
			return store.Order{}, err
		}
	}
	if err := st.SetReservationsStatus(ctx, tx, o.ID, "active", "released"); err != nil {
		return store.Order{}, err
	}
	if err := returnDeposits(ctx, tx, st, o); err != nil {
		return store.Order{}, err
	}
	if err := st.FailPendingPayments(ctx, tx, o.ID); err != nil {
		return store.Order{}, err
	}
	if err := st.MarkVoided(ctx, tx, o.ID); err != nil {
		return store.Order{}, err
	}
	o.Status = "voided"
	outletID := o.OutletID
	if err := audit(ctx, tx, st, o.MerchantID, staffID, &outletID, "order.voided", "order", o.ID, map[string]any{
		"reference": o.Reference, "total_minor": o.TotalMinor, "reason": reason,
	}); err != nil {
		return store.Order{}, err
	}
	return o, nil
}
