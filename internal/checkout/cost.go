package checkout

import (
	"context"
	"math"

	"github.com/Muddyscode/till/internal/ledger"
	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func mulCost(unit int64, qty int) (int64, error) {
	if unit == 0 || qty == 0 {
		return 0, nil
	}
	if unit < 0 || qty < 0 {
		return 0, ledger.Err(ledger.CodeInvalidAmount, "cost cannot be negative")
	}
	if int64(qty) > math.MaxInt64/unit {
		return 0, ledger.Err(ledger.CodeInvalidAmount, "cost overflows")
	}
	return unit * int64(qty), nil
}

func accountID(id *uuid.UUID) (uuid.UUID, error) {
	if id == nil {
		return uuid.Nil, ledger.Err(ledger.CodeInvalidOrder, "merchant books are not set up")
	}
	return *id, nil
}

func postPair(ctx context.Context, tx pgx.Tx, st *store.Store, merchantID uuid.UUID, desc string, debit, credit uuid.UUID, amount int64) error {
	_, err := ledger.Post(ctx, tx, poster(st), ledger.PostRequest{
		MerchantID:  merchantID,
		Description: desc,
		Lines: []ledger.Line{
			{AccountID: debit, Direction: ledger.Debit, Amount: amount},
			{AccountID: credit, Direction: ledger.Credit, Amount: amount},
		},
	})
	return err
}

type ReceiptInput struct {
	Merchant  store.Merchant
	Outlet    store.Outlet
	StaffID   uuid.UUID
	ProductID uuid.UUID
	Qty       int
	// UnitCost is nil when the receipt omits a cost. Explicit 0 is a real zero.
	UnitCost *int64
}

// Receive adds units and, when the booked cost is above zero, debits inventory
// and credits goods received. It does not take cash. Paying the supplier is a
// later transfer from cash to goods received.
func Receive(ctx context.Context, tx pgx.Tx, st *store.Store, in ReceiptInput) (onHand int, avg int64, err error) {
	if in.Qty <= 0 {
		return 0, 0, ledger.Err(ledger.CodeInvalidOrder, "qty must be > 0")
	}
	p, err := st.LockProduct(ctx, tx, in.Merchant.ID, in.ProductID)
	if err != nil {
		return 0, 0, err
	}
	oldOn, err := st.SumOnHand(ctx, tx, p.ID)
	if err != nil {
		return 0, 0, err
	}
	oldAvg := p.AvgCostMinor
	newAvg := oldAvg
	var booked int64
	if in.UnitCost == nil {
		booked = oldAvg
	} else if oldOn == 0 {
		newAvg = *in.UnitCost
		booked = *in.UnitCost
	} else {
		newAvg, err = store.WeightedAverage(oldOn, oldAvg, in.Qty, *in.UnitCost)
		if err != nil {
			return 0, 0, err
		}
		booked = *in.UnitCost
	}
	onHand, err = st.ReceiveStock(ctx, tx, in.Outlet.ID, p.ID, in.Qty)
	if err != nil {
		return 0, 0, err
	}
	if newAvg != oldAvg {
		p.AvgCostMinor = newAvg
		if _, err := st.UpdateProduct(ctx, tx, p); err != nil {
			return 0, 0, err
		}
	}
	amount, err := mulCost(booked, in.Qty)
	if err != nil {
		return 0, 0, err
	}
	if amount > 0 {
		inv, err := accountID(in.Merchant.InventoryAccountID)
		if err != nil {
			return 0, 0, err
		}
		goods, err := accountID(in.Merchant.GoodsReceivedAccountID)
		if err != nil {
			return 0, 0, err
		}
		if err := postPair(ctx, tx, st, in.Merchant.ID, "receipt "+p.SKU, inv, goods, amount); err != nil {
			return 0, 0, err
		}
	}
	outletID := in.Outlet.ID
	meta := map[string]any{"sku": p.SKU, "qty": in.Qty, "avg_cost_minor": newAvg}
	if in.UnitCost != nil {
		meta["unit_cost_minor"] = *in.UnitCost
	}
	if err := audit(ctx, tx, st, in.Merchant.ID, in.StaffID, &outletID, "stock.received", "stock", p.ID, meta); err != nil {
		return 0, 0, err
	}
	return onHand, newAvg, nil
}

type AdjustInput struct {
	Merchant  store.Merchant
	Outlet    store.Outlet
	StaffID   uuid.UUID
	ProductID uuid.UUID
	Delta     int
	Reason    string
}

// Adjust changes quantity. When the product has an average cost, the same
// transaction moves inventory against the adjustment account so the layer
// and the general ledger stay together.
func Adjust(ctx context.Context, tx pgx.Tx, st *store.Store, in AdjustInput) (int, error) {
	if in.Delta == 0 {
		return 0, ledger.Err(ledger.CodeInvalidOrder, "delta cannot be zero")
	}
	if in.Reason == "" {
		return 0, ledger.Err(ledger.CodeInvalidOrder, "reason is required")
	}
	p, err := st.LockProduct(ctx, tx, in.Merchant.ID, in.ProductID)
	if err != nil {
		return 0, err
	}
	onHand, err := st.AdjustStock(ctx, tx, in.Outlet.ID, p.ID, in.Delta)
	if err != nil {
		return 0, err
	}
	qty := in.Delta
	if qty < 0 {
		qty = -qty
	}
	amount, err := mulCost(p.AvgCostMinor, qty)
	if err != nil {
		return 0, err
	}
	if amount > 0 {
		inv, err := accountID(in.Merchant.InventoryAccountID)
		if err != nil {
			return 0, err
		}
		adj, err := accountID(in.Merchant.AdjustmentAccountID)
		if err != nil {
			return 0, err
		}
		debit, credit := inv, adj
		if in.Delta < 0 {
			debit, credit = adj, inv
		}
		if err := postPair(ctx, tx, st, in.Merchant.ID, "adjust "+p.SKU, debit, credit, amount); err != nil {
			return 0, err
		}
	}
	outletID := in.Outlet.ID
	if err := audit(ctx, tx, st, in.Merchant.ID, in.StaffID, &outletID, "stock.adjusted", "stock", p.ID, map[string]any{
		"sku": p.SKU, "delta": in.Delta, "reason": in.Reason, "avg_cost_minor": p.AvgCostMinor,
	}); err != nil {
		return 0, err
	}
	return onHand, nil
}

// SaveProduct writes the product and, when the average cost changes while
// units are on hand, posts the difference to inventory and the adjustment
// account. An upward revaluation credits that expense account, so the
// account is allowed to go negative. This is not a cash transfer.
func SaveProduct(ctx context.Context, tx pgx.Tx, st *store.Store, merchant store.Merchant, next store.Product, staffID uuid.UUID) (store.Product, error) {
	locked, err := st.LockProduct(ctx, tx, next.MerchantID, next.ID)
	if err != nil {
		return store.Product{}, err
	}
	updated, err := st.UpdateProduct(ctx, tx, next)
	if err != nil {
		return store.Product{}, err
	}
	if next.AvgCostMinor != locked.AvgCostMinor {
		onHand, err := st.SumOnHand(ctx, tx, next.ID)
		if err != nil {
			return store.Product{}, err
		}
		amount, increase, err := revalueAmount(locked.AvgCostMinor, next.AvgCostMinor, onHand)
		if err != nil {
			return store.Product{}, err
		}
		if amount > 0 {
			inv, err := accountID(merchant.InventoryAccountID)
			if err != nil {
				return store.Product{}, err
			}
			adj, err := accountID(merchant.AdjustmentAccountID)
			if err != nil {
				return store.Product{}, err
			}
			debit, credit := inv, adj
			if !increase {
				debit, credit = adj, inv
			}
			if err := postPair(ctx, tx, st, merchant.ID, "revalue "+next.SKU, debit, credit, amount); err != nil {
				return store.Product{}, err
			}
		}
		if err := audit(ctx, tx, st, merchant.ID, staffID, nil, "product.cost_changed", "product", next.ID, map[string]any{
			"sku": next.SKU, "from": locked.AvgCostMinor, "to": next.AvgCostMinor, "on_hand": onHand,
		}); err != nil {
			return store.Product{}, err
		}
	}
	if updated.PriceMinor != locked.PriceMinor {
		if err := audit(ctx, tx, st, merchant.ID, staffID, nil, "product.price_changed", "product", next.ID, map[string]any{
			"sku": updated.SKU, "from": locked.PriceMinor, "to": updated.PriceMinor,
		}); err != nil {
			return store.Product{}, err
		}
	}
	return updated, nil
}

func revalueAmount(oldAvg, newAvg int64, onHand int) (amount int64, increase bool, err error) {
	if onHand <= 0 || oldAvg == newAvg {
		return 0, newAvg > oldAvg, nil
	}
	if oldAvg < 0 || newAvg < 0 {
		return 0, false, ledger.Err(ledger.CodeInvalidAmount, "cost cannot be negative")
	}
	diff := newAvg - oldAvg
	increase = diff > 0
	if diff < 0 {
		diff = -diff
	}
	if int64(onHand) > math.MaxInt64/diff {
		return 0, false, ledger.Err(ledger.CodeInvalidAmount, "revaluation overflows")
	}
	return diff * int64(onHand), increase, nil
}

type TopUpInput struct {
	Merchant       store.Merchant
	Outlet         store.Outlet
	StaffID        uuid.UUID
	CustomerID     uuid.UUID
	Amount         int64
	IdempotencyKey string
}

// TopUp takes cash into the drawer and credits the customer wallet liability.
// The balance on the customer row moves in the same transaction.
func TopUp(ctx context.Context, tx pgx.Tx, st *store.Store, in TopUpInput) (store.Customer, ledger.Journal, error) {
	if in.Amount <= 0 {
		return store.Customer{}, ledger.Journal{}, ledger.Err(ledger.CodeInvalidAmount, "amount_minor must be > 0")
	}
	wallet, err := accountID(in.Merchant.WalletAccountID)
	if err != nil {
		return store.Customer{}, ledger.Journal{}, err
	}
	c, err := st.LockCustomer(ctx, tx, in.Merchant.ID, in.CustomerID)
	if err != nil {
		return store.Customer{}, ledger.Journal{}, err
	}
	bal, err := st.AddWalletBalance(ctx, tx, in.Merchant.ID, c.ID, in.Amount)
	if err != nil {
		return store.Customer{}, ledger.Journal{}, err
	}
	j, err := ledger.Post(ctx, tx, poster(st), ledger.PostRequest{
		MerchantID:     in.Merchant.ID,
		Description:    "wallet top-up " + c.Phone,
		IdempotencyKey: in.IdempotencyKey,
		Lines: []ledger.Line{
			{AccountID: in.Outlet.CashAccountID, Direction: ledger.Debit, Amount: in.Amount},
			{AccountID: wallet, Direction: ledger.Credit, Amount: in.Amount},
		},
	})
	if err != nil {
		return store.Customer{}, ledger.Journal{}, err
	}
	c.BalanceMinor = bal
	outletID := in.Outlet.ID
	if err := audit(ctx, tx, st, in.Merchant.ID, in.StaffID, &outletID, "wallet.topped_up", "customer", c.ID, map[string]any{
		"amount_minor": in.Amount, "balance_minor": bal, "phone": c.Phone,
	}); err != nil {
		return store.Customer{}, ledger.Journal{}, err
	}
	return c, j, nil
}
