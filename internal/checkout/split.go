package checkout

import (
	"context"
	"strings"
	"time"

	"github.com/Muddyscode/till/internal/ledger"
	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Tender is one way the customer pays part (or all) of an order.
type Tender struct {
	Method     string // cash, wallet or transfer
	Amount     int64
	CustomerID *uuid.UUID // wallet only
}

// SaleWithTenders sells against one or more tenders that add up to the payable.
//
//   - Cash and wallet only: the sale posts at once, one journal with a debit
//     per tender (outlet cash, customer wallet).
//   - With a transfer tender: the order is held pending_payment and the stock
//     reserved. Cash and wallet tenders are taken now and parked in Deposits
//     held (2400); the Paystack webhook for the transfer amount posts the sale
//     (Dr 2400 + Dr 1100). Expiry or void returns the deposits.
//
// One tender behaves like the matching single payment_method. Two or more make
// the order's payment_method "split".
func SaleWithTenders(ctx context.Context, tx pgx.Tx, st *store.Store, in SaleInput, tenders []Tender) (store.Order, error) {
	if len(tenders) == 0 {
		return store.Order{}, ledger.Err(ledger.CodeInvalidOrder, "payments needs at least one tender")
	}
	if in.Hold == 0 {
		in.Hold = 15 * time.Minute
	}
	var cash, wallet, transfer int64
	var customer *uuid.UUID
	for _, t := range tenders {
		switch t.Method {
		case "cash":
			cash = t.Amount
		case "wallet":
			wallet = t.Amount
			customer = t.CustomerID
		case "transfer":
			transfer = t.Amount
		}
	}
	if wallet > 0 && (customer == nil || in.Merchant.WalletAccountID == nil) {
		return store.Order{}, ledger.Err(ledger.CodeInvalidOrder, "wallet payment needs a customer_id")
	}
	pending := transfer > 0
	method := "split"
	if len(tenders) == 1 {
		method = tenders[0].Method
	}

	var err error
	in.Lines, err = refreshProducts(ctx, tx, st, in.Merchant.ID, in.Lines)
	if err != nil {
		return store.Order{}, err
	}
	c, err := totals(in.Lines, in.Merchant.VATInclusiveBps, in.Merchant.VATRegistered, in.DiscountMinor)
	if err != nil {
		return store.Order{}, err
	}
	if err := enforceDiscount(in.Merchant, in.Role, c.Shelf, in.DiscountMinor, in.DiscountReason); err != nil {
		return store.Order{}, err
	}
	if cash+wallet+transfer != c.Payable {
		return store.Order{}, ledger.Err(CodeTenderMismatch, "payments must add up to the payable total")
	}
	lines := c.Lines
	var cost int64
	if !pending {
		if cost, err = st.LockSaleCost(ctx, tx, in.Merchant.ID, lines); err != nil {
			return store.Order{}, err
		}
	}
	if err := lockProducts(ctx, tx, st, in.Outlet.ID, in.Lines); err != nil {
		return store.Order{}, err
	}
	if wallet > 0 {
		if _, err := st.LockCustomer(ctx, tx, in.Merchant.ID, *customer); err != nil {
			return store.Order{}, err
		}
	}
	ref, err := st.NextReference(ctx, tx, in.Outlet.ID, in.Outlet.Code)
	if err != nil {
		return store.Order{}, err
	}
	id, err := newID()
	if err != nil {
		return store.Order{}, err
	}
	order := store.Order{
		ID: id, MerchantID: in.Merchant.ID, OutletID: in.Outlet.ID, StaffID: in.StaffID,
		Status: "paid", PaymentMethod: method, Currency: "NGN",
		TotalMinor: c.Payable, VATMinor: c.VAT, NetMinor: c.Net, DiscountMinor: in.DiscountMinor,
		DiscountReason: strings.TrimSpace(in.DiscountReason), CostMinor: cost, CustomerID: customer,
		Reference: ref, IdempotencyKey: in.IdempotencyKey, CreatedAt: in.Now,
	}
	var exp time.Time
	if pending {
		exp = in.Now.Add(in.Hold)
		order.Status = "pending_payment"
		order.ExpiresAt = &exp
	} else {
		paid := in.Now
		order.PaidAt = &paid
	}
	if err := st.InsertOrder(ctx, tx, order); err != nil {
		return store.Order{}, err
	}
	for i := range lines {
		lines[i].OrderID = id
		if err := st.InsertOrderLine(ctx, tx, lines[i]); err != nil {
			return store.Order{}, err
		}
		if !pending {
			if err := st.DeductStock(ctx, tx, in.Outlet.ID, lines[i].ProductID, lines[i].Qty); err != nil {
				return store.Order{}, err
			}
			continue
		}
		if err := st.ReserveStock(ctx, tx, in.Outlet.ID, lines[i].ProductID, lines[i].Qty); err != nil {
			return store.Order{}, err
		}
		rid, err := newID()
		if err != nil {
			return store.Order{}, err
		}
		if err := st.InsertReservation(ctx, tx, rid, in.Merchant.ID, in.Outlet.ID, id, lines[i].ProductID, lines[i].Qty, exp); err != nil {
			return store.Order{}, err
		}
	}
	if wallet > 0 {
		if _, err := st.AddWalletBalance(ctx, tx, in.Merchant.ID, *customer, -wallet); err != nil {
			return store.Order{}, err
		}
	}
	order.Lines = lines

	// tenderLines are the debits for the cash and wallet tenders.
	var tenderLines []ledger.Line
	if cash > 0 {
		tenderLines = append(tenderLines, ledger.Line{AccountID: in.Outlet.CashAccountID, Direction: ledger.Debit, Amount: cash})
	}
	if wallet > 0 {
		tenderLines = append(tenderLines, ledger.Line{AccountID: *in.Merchant.WalletAccountID, Direction: ledger.Debit, Amount: wallet})
	}
	meta := map[string]any{
		"reference": ref, "total_minor": c.Payable, "discount_minor": in.DiscountMinor, "method": method,
		"payments": map[string]int64{"cash": cash, "wallet": wallet, "transfer": transfer},
	}
	if customer != nil {
		meta["customer_id"] = customer.String()
	}
	outletID := in.Outlet.ID

	if !pending {
		j, err := postSaleDebits(ctx, tx, st, in, order, tenderLines, in.IdempotencyKey)
		if err != nil {
			return store.Order{}, err
		}
		if err := st.AttachJournal(ctx, tx, order.ID, j.ID, in.Now); err != nil {
			return store.Order{}, err
		}
		order.JournalID = &j.ID
		for _, t := range tenders {
			pid, err := newID()
			if err != nil {
				return store.Order{}, err
			}
			if err := st.InsertTender(ctx, tx, store.Payment{ID: pid, OrderID: id, Method: t.Method, Status: "confirmed", AmountMinor: t.Amount}, "", &j.ID); err != nil {
				return store.Order{}, err
			}
		}
		meta["cost_minor"] = cost
		if order.Payments, err = st.OrderPayments(ctx, tx, id); err != nil {
			return store.Order{}, err
		}
		if err := audit(ctx, tx, st, in.Merchant.ID, in.StaffID, &outletID, "order.paid", "order", order.ID, meta); err != nil {
			return store.Order{}, err
		}
		return order, nil
	}

	var depositJournal *uuid.UUID
	if len(tenderLines) > 0 {
		dep, err := st.EnsureDepositsAccount(ctx, tx, in.Merchant.ID)
		if err != nil {
			return store.Order{}, err
		}
		jl := append([]ledger.Line(nil), tenderLines...)
		jl = append(jl, ledger.Line{AccountID: dep, Direction: ledger.Credit, Amount: cash + wallet})
		dj, err := ledger.Post(ctx, tx, poster(st), ledger.PostRequest{
			MerchantID:     in.Merchant.ID,
			Description:    "deposits held " + ref,
			Lines:          jl,
			IdempotencyKey: in.IdempotencyKey + ":deposit",
		})
		if err != nil {
			return store.Order{}, err
		}
		depositJournal = &dj.ID
	}
	for _, t := range tenders {
		pid, err := newID()
		if err != nil {
			return store.Order{}, err
		}
		p := store.Payment{ID: pid, OrderID: id, Method: t.Method, Status: "held", AmountMinor: t.Amount, DepositJournalID: depositJournal}
		provider := ""
		if t.Method == "transfer" {
			p.Status, p.DepositJournalID, provider = "pending", nil, "paystack"
		}
		if err := st.InsertTender(ctx, tx, p, provider, nil); err != nil {
			return store.Order{}, err
		}
	}
	meta["expires_at"] = exp.UTC().Format(time.RFC3339)
	if order.Payments, err = st.OrderPayments(ctx, tx, id); err != nil {
		return store.Order{}, err
	}
	if err := audit(ctx, tx, st, in.Merchant.ID, in.StaffID, &outletID, "order.reserved", "order", order.ID, meta); err != nil {
		return store.Order{}, err
	}
	return order, nil
}
