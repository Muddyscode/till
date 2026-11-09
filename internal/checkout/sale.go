package checkout

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/Muddyscode/till/internal/ledger"
	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type SaleLine struct {
	Product store.Product
	Qty     int
}

type SaleInput struct {
	Merchant       store.Merchant
	Outlet         store.Outlet
	StaffID        uuid.UUID
	Lines          []SaleLine
	Now            time.Time
	Hold           time.Duration
	IdempotencyKey string
	DiscountMinor  int64
	CustomerID     *uuid.UUID
	// Role is the actor's role ("admin", "manager", "cashier"); it picks the discount limit.
	Role           string
	DiscountReason string
}

func poster(st *store.Store) ledger.StorePoster {
	return ledger.StorePoster{
		LockAccounts:  st.LockAccounts,
		InsertJournal: st.InsertJournal,
		InsertPosting: st.InsertPosting,
		UpdateBalance: st.UpdateBalance,
	}
}

func newID() (uuid.UUID, error) { return uuid.NewV7() }

// calc is the priced basket: shelf total, payable after discount, VAT, net,
// and the lines with their share of discount, VAT and net.
type calc struct {
	Shelf, Payable, VAT, Net int64
	Lines                    []store.OrderLine
}

// totals prices the basket. The order discount is apportioned to lines by line
// total (remainder kobo to the last line); each line's VAT is then computed on
// what the customer pays for it, by tax class. Exempt and zero-rated lines, and
// every line when the merchant is not VAT registered, carry no VAT.
func totals(lines []SaleLine, bps int, vatRegistered bool, discount int64) (calc, error) {
	if len(lines) == 0 {
		return calc{}, ledger.Err(ledger.CodeInvalidOrder, "order needs at least one line")
	}
	if discount < 0 {
		return calc{}, ledger.Err(ledger.CodeInvalidAmount, "discount_minor cannot be negative")
	}
	var shelf int64
	var built []store.OrderLine
	seen := map[uuid.UUID]struct{}{}
	for _, ln := range lines {
		if ln.Qty <= 0 {
			return calc{}, ledger.Err(ledger.CodeInvalidOrder, "qty must be > 0")
		}
		if !ln.Product.Active {
			return calc{}, ledger.Err(ledger.CodeInvalidOrder, ln.Product.SKU+" is not for sale")
		}
		if _, ok := seen[ln.Product.ID]; ok {
			return calc{}, ledger.Err(ledger.CodeInvalidOrder, "duplicate sku "+ln.Product.SKU)
		}
		seen[ln.Product.ID] = struct{}{}
		if ln.Product.PriceMinor <= 0 || int64(ln.Qty) > math.MaxInt64/ln.Product.PriceMinor {
			return calc{}, ledger.Err(ledger.CodeInvalidAmount, "line total overflows")
		}
		lineTotal := int64(ln.Qty) * ln.Product.PriceMinor
		if shelf > math.MaxInt64-lineTotal {
			return calc{}, ledger.Err(ledger.CodeInvalidAmount, "order total overflows")
		}
		shelf += lineTotal
		id, err := newID()
		if err != nil {
			return calc{}, err
		}
		class := ln.Product.TaxClass
		if class == "" {
			class = taxStandard
		}
		built = append(built, store.OrderLine{
			ID: id, ProductID: ln.Product.ID, SKU: ln.Product.SKU,
			Qty: ln.Qty, UnitPriceMinor: ln.Product.PriceMinor, LineTotalMinor: lineTotal, TaxClass: class,
		})
	}
	if discount >= shelf {
		return calc{}, ledger.Err(ledger.CodeInvalidOrder, "discount must be less than the shelf total")
	}
	weights := make([]int64, len(built))
	for i, b := range built {
		weights[i] = b.LineTotalMinor
	}
	shares := apportion(discount, weights, weights)
	c := calc{Shelf: shelf, Payable: shelf - discount, Lines: built}
	for i := range c.Lines {
		ln := &c.Lines[i]
		ln.DiscountMinor = shares[i]
		pay := ln.LineTotalMinor - ln.DiscountMinor
		if vatRegistered && ln.TaxClass == taxStandard {
			ln.VATMinor, _ = InclusiveVAT(pay, bps)
		}
		ln.NetMinor = pay - ln.VATMinor
		c.VAT += ln.VATMinor
		c.Net += ln.NetMinor
	}
	if c.Net <= 0 {
		return calc{}, ledger.Err(ledger.CodeInvalidAmount, "net sale is not positive")
	}
	return c, nil
}

// refreshProducts locks the sale's products in id order and swaps the pre-lock
// copies for the locked rows, so price and active are read under the lock.
func refreshProducts(ctx context.Context, tx pgx.Tx, st *store.Store, merchantID uuid.UUID, lines []SaleLine) ([]SaleLine, error) {
	ids := make([]uuid.UUID, len(lines))
	for i, ln := range lines {
		ids[i] = ln.Product.ID
	}
	locked, err := st.LockProducts(ctx, tx, merchantID, ids)
	if err != nil {
		return nil, err
	}
	out := make([]SaleLine, len(lines))
	for i, ln := range lines {
		out[i] = SaleLine{Product: locked[ln.Product.ID], Qty: ln.Qty}
	}
	return out, nil
}

func lockProducts(ctx context.Context, tx pgx.Tx, st *store.Store, outlet uuid.UUID, lines []SaleLine) error {
	ids := make([]uuid.UUID, len(lines))
	for i, ln := range lines {
		ids[i] = ln.Product.ID
	}
	return st.LockStock(ctx, tx, outlet, ids)
}

func audit(ctx context.Context, tx pgx.Tx, st *store.Store, merchantID, staffID uuid.UUID, outletID *uuid.UUID, action, entity string, entityID uuid.UUID, meta any) error {
	raw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	mid, sid := merchantID, staffID
	return st.InsertAudit(ctx, tx, store.AuditEvent{
		MerchantID: &mid,
		StaffID:    &sid,
		Action:     action,
		EntityType: entity,
		EntityID:   entityID,
		OutletID:   outletID,
		Metadata:   raw,
	})
}

func postSale(ctx context.Context, tx pgx.Tx, st *store.Store, in SaleInput, order store.Order, debit uuid.UUID, key string) (ledger.Journal, error) {
	return postSaleDebits(ctx, tx, st, in, order, []ledger.Line{{AccountID: debit, Direction: ledger.Debit, Amount: order.TotalMinor}}, key)
}

// postSaleDebits posts the sale. The debit lines (one account, or several for a
// split tender) must add up to the order total.
func postSaleDebits(ctx context.Context, tx pgx.Tx, st *store.Store, in SaleInput, order store.Order, debits []ledger.Line, key string) (ledger.Journal, error) {
	if in.Merchant.VATAccountID == nil || in.Merchant.ClearingAccountID == nil {
		return ledger.Journal{}, ledger.Err(ledger.CodeInvalidOrder, "merchant books are not set up")
	}
	lines := append([]ledger.Line(nil), debits...)
	lines = append(lines, ledger.Line{AccountID: in.Outlet.SalesAccountID, Direction: ledger.Credit, Amount: order.NetMinor})
	if order.VATMinor > 0 {
		lines = append(lines, ledger.Line{
			AccountID: *in.Merchant.VATAccountID, Direction: ledger.Credit, Amount: order.VATMinor,
		})
	}
	if order.CostMinor > 0 {
		if in.Merchant.COGSAccountID == nil || in.Merchant.InventoryAccountID == nil {
			return ledger.Journal{}, ledger.Err(ledger.CodeInvalidOrder, "merchant books are not set up")
		}
		lines = append(lines,
			ledger.Line{AccountID: *in.Merchant.COGSAccountID, Direction: ledger.Debit, Amount: order.CostMinor},
			ledger.Line{AccountID: *in.Merchant.InventoryAccountID, Direction: ledger.Credit, Amount: order.CostMinor},
		)
	}
	return ledger.Post(ctx, tx, poster(st), ledger.PostRequest{
		MerchantID:     in.Merchant.ID,
		Description:    "sale " + order.Reference,
		Lines:          lines,
		IdempotencyKey: key,
	})
}

func CashSale(ctx context.Context, tx pgx.Tx, st *store.Store, in SaleInput) (store.Order, error) {
	return payNow(ctx, tx, st, in, "cash", in.Outlet.CashAccountID)
}

func WalletSale(ctx context.Context, tx pgx.Tx, st *store.Store, in SaleInput) (store.Order, error) {
	if in.Merchant.WalletAccountID == nil {
		return store.Order{}, ledger.Err(ledger.CodeInvalidOrder, "merchant books are not set up")
	}
	return payNow(ctx, tx, st, in, "wallet", *in.Merchant.WalletAccountID)
}

func payNow(ctx context.Context, tx pgx.Tx, st *store.Store, in SaleInput, method string, debit uuid.UUID) (store.Order, error) {
	if method == "wallet" {
		if in.CustomerID == nil {
			return store.Order{}, ledger.Err(ledger.CodeInvalidOrder, "wallet payment needs a customer_id")
		}
	} else if in.CustomerID != nil {
		return store.Order{}, ledger.Err(ledger.CodeInvalidOrder, "customer_id is only used for wallet payments")
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
	payable, vat, net, lines := c.Payable, c.VAT, c.Net, c.Lines
	cost, err := st.LockSaleCost(ctx, tx, in.Merchant.ID, lines)
	if err != nil {
		return store.Order{}, err
	}
	if err := lockProducts(ctx, tx, st, in.Outlet.ID, in.Lines); err != nil {
		return store.Order{}, err
	}
	if method == "wallet" {
		if _, err := st.LockCustomer(ctx, tx, in.Merchant.ID, *in.CustomerID); err != nil {
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
	paid := in.Now
	order := store.Order{
		ID: id, MerchantID: in.Merchant.ID, OutletID: in.Outlet.ID, StaffID: in.StaffID,
		Status: "paid", PaymentMethod: method, Currency: "NGN",
		TotalMinor: payable, VATMinor: vat, NetMinor: net,
		DiscountMinor: in.DiscountMinor, DiscountReason: strings.TrimSpace(in.DiscountReason), CostMinor: cost, CustomerID: in.CustomerID,
		Reference: ref, IdempotencyKey: in.IdempotencyKey, PaidAt: &paid, CreatedAt: in.Now,
	}
	if err := st.InsertOrder(ctx, tx, order); err != nil {
		return store.Order{}, err
	}
	for i := range lines {
		lines[i].OrderID = id
		if err := st.InsertOrderLine(ctx, tx, lines[i]); err != nil {
			return store.Order{}, err
		}
		if err := st.DeductStock(ctx, tx, in.Outlet.ID, lines[i].ProductID, lines[i].Qty); err != nil {
			return store.Order{}, err
		}
	}
	if method == "wallet" {
		if _, err := st.AddWalletBalance(ctx, tx, in.Merchant.ID, *in.CustomerID, -payable); err != nil {
			return store.Order{}, err
		}
	}
	order.Lines = lines
	j, err := postSale(ctx, tx, st, in, order, debit, in.IdempotencyKey)
	if err != nil {
		return store.Order{}, err
	}
	if err := st.AttachJournal(ctx, tx, order.ID, j.ID, paid); err != nil {
		return store.Order{}, err
	}
	order.JournalID = &j.ID
	payID, err := newID()
	if err != nil {
		return store.Order{}, err
	}
	if err := st.InsertPayment(ctx, tx, payID, order.ID, method, "confirmed", payable, "", nil, &j.ID); err != nil {
		return store.Order{}, err
	}
	outletID := in.Outlet.ID
	meta := map[string]any{
		"reference": ref, "total_minor": payable, "discount_minor": in.DiscountMinor,
		"cost_minor": cost, "method": method,
	}
	if in.CustomerID != nil {
		meta["customer_id"] = in.CustomerID.String()
	}
	if err := audit(ctx, tx, st, in.Merchant.ID, in.StaffID, &outletID, "order.paid", "order", order.ID, meta); err != nil {
		return store.Order{}, err
	}
	return order, nil
}

func ReserveTransfer(ctx context.Context, tx pgx.Tx, st *store.Store, in SaleInput) (store.Order, error) {
	if in.Hold == 0 {
		in.Hold = 15 * time.Minute
	}
	if in.CustomerID != nil {
		return store.Order{}, ledger.Err(ledger.CodeInvalidOrder, "customer_id is only used for wallet payments")
	}
	lockedLines, err := refreshProducts(ctx, tx, st, in.Merchant.ID, in.Lines)
	if err != nil {
		return store.Order{}, err
	}
	in.Lines = lockedLines
	if err := lockProducts(ctx, tx, st, in.Outlet.ID, in.Lines); err != nil {
		return store.Order{}, err
	}
	c, err := totals(in.Lines, in.Merchant.VATInclusiveBps, in.Merchant.VATRegistered, in.DiscountMinor)
	if err != nil {
		return store.Order{}, err
	}
	if err := enforceDiscount(in.Merchant, in.Role, c.Shelf, in.DiscountMinor, in.DiscountReason); err != nil {
		return store.Order{}, err
	}
	payable, vat, net, lines := c.Payable, c.VAT, c.Net, c.Lines
	ref, err := st.NextReference(ctx, tx, in.Outlet.ID, in.Outlet.Code)
	if err != nil {
		return store.Order{}, err
	}
	id, err := newID()
	if err != nil {
		return store.Order{}, err
	}
	exp := in.Now.Add(in.Hold)
	order := store.Order{
		ID: id, MerchantID: in.Merchant.ID, OutletID: in.Outlet.ID, StaffID: in.StaffID,
		Status: "pending_payment", PaymentMethod: "transfer", Currency: "NGN",
		TotalMinor: payable, VATMinor: vat, NetMinor: net, DiscountMinor: in.DiscountMinor,
		DiscountReason: strings.TrimSpace(in.DiscountReason),
		Reference:      ref, IdempotencyKey: in.IdempotencyKey, ExpiresAt: &exp, CreatedAt: in.Now,
	}
	if err := st.InsertOrder(ctx, tx, order); err != nil {
		return store.Order{}, err
	}
	for i := range lines {
		lines[i].OrderID = id
		if err := st.InsertOrderLine(ctx, tx, lines[i]); err != nil {
			return store.Order{}, err
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
	order.Lines = lines
	payID, err := newID()
	if err != nil {
		return store.Order{}, err
	}
	if err := st.InsertPayment(ctx, tx, payID, order.ID, "transfer", "pending", payable, "paystack", nil, nil); err != nil {
		return store.Order{}, err
	}
	outletID := in.Outlet.ID
	if err := audit(ctx, tx, st, in.Merchant.ID, in.StaffID, &outletID, "order.reserved", "order", order.ID, map[string]any{
		"reference": ref, "total_minor": payable, "discount_minor": in.DiscountMinor, "expires_at": exp.UTC().Format(time.RFC3339),
	}); err != nil {
		return store.Order{}, err
	}
	return order, nil
}

type ConfirmInput struct {
	Merchant       store.Merchant
	Outlet         store.Outlet
	Order          store.Order
	Now            time.Time
	IdempotencyKey string
	// ProviderEventID is stored on the payment row (payments.provider_event_id).
	ProviderEventID string
}

func ConfirmTransfer(ctx context.Context, tx pgx.Tx, st *store.Store, in ConfirmInput) (store.Order, error) {
	if in.Merchant.ClearingAccountID == nil {
		return store.Order{}, ledger.Err(ledger.CodeInvalidOrder, "merchant books are not set up")
	}
	ids := make([]uuid.UUID, len(in.Order.Lines))
	lines := make([]SaleLine, len(in.Order.Lines))
	for i, ln := range in.Order.Lines {
		ids[i] = ln.ProductID
		lines[i] = SaleLine{Qty: ln.Qty, Product: store.Product{ID: ln.ProductID}}
	}
	cost, err := st.LockSaleCost(ctx, tx, in.Merchant.ID, in.Order.Lines)
	if err != nil {
		return store.Order{}, err
	}
	if err := st.LockStock(ctx, tx, in.Outlet.ID, ids); err != nil {
		return store.Order{}, err
	}
	for _, ln := range in.Order.Lines {
		if err := st.CommitReserved(ctx, tx, in.Outlet.ID, ln.ProductID, ln.Qty); err != nil {
			return store.Order{}, err
		}
	}
	if err := st.SetReservationsStatus(ctx, tx, in.Order.ID, "active", "committed"); err != nil {
		return store.Order{}, err
	}
	in.Order.CostMinor = cost
	if err := st.SetOrderCost(ctx, tx, in.Order.ID, cost); err != nil {
		return store.Order{}, err
	}
	if err := st.SetLineCosts(ctx, tx, in.Order.Lines); err != nil {
		return store.Order{}, err
	}
	// A split order already holds its cash and wallet tenders in Deposits held
	// (2400); the sale clears those and debits Paystack clearing for the rest.
	held, err := st.HeldPayments(ctx, tx, in.Order.ID)
	if err != nil {
		return store.Order{}, err
	}
	var deposits int64
	for _, h := range held {
		deposits += h.AmountMinor
	}
	debits := []ledger.Line{{AccountID: *in.Merchant.ClearingAccountID, Direction: ledger.Debit, Amount: in.Order.TotalMinor - deposits}}
	if deposits > 0 {
		dep, err := st.EnsureDepositsAccount(ctx, tx, in.Merchant.ID)
		if err != nil {
			return store.Order{}, err
		}
		debits = append(debits, ledger.Line{AccountID: dep, Direction: ledger.Debit, Amount: deposits})
	}
	sale := SaleInput{Merchant: in.Merchant, Outlet: in.Outlet, Lines: lines}
	j, err := postSaleDebits(ctx, tx, st, sale, in.Order, debits, in.IdempotencyKey)
	if err != nil {
		return store.Order{}, err
	}
	if err := st.ConfirmHeld(ctx, tx, in.Order.ID, j.ID); err != nil {
		return store.Order{}, err
	}
	if err := st.AttachJournal(ctx, tx, in.Order.ID, j.ID, in.Now); err != nil {
		return store.Order{}, err
	}
	var eventID *string
	if in.ProviderEventID != "" {
		eventID = &in.ProviderEventID
	}
	if err := st.ConfirmPayment(ctx, tx, in.Order.ID, j.ID, eventID); err != nil {
		return store.Order{}, err
	}
	if in.Order.Payments, err = st.OrderPayments(ctx, tx, in.Order.ID); err != nil {
		return store.Order{}, err
	}
	in.Order.Status = "paid"
	in.Order.JournalID = &j.ID
	in.Order.PaidAt = &in.Now
	outletID := in.Outlet.ID
	mid := in.Merchant.ID
	raw, err := json.Marshal(map[string]any{
		"reference": in.Order.Reference, "total_minor": in.Order.TotalMinor,
		"discount_minor": in.Order.DiscountMinor, "cost_minor": cost, "method": "transfer",
	})
	if err != nil {
		return store.Order{}, err
	}
	if err := st.InsertAudit(ctx, tx, store.AuditEvent{
		MerchantID: &mid, Action: "order.paid", EntityType: "order", EntityID: in.Order.ID, OutletID: &outletID, Metadata: raw,
	}); err != nil {
		return store.Order{}, err
	}
	return in.Order, nil
}

// Refund reverses a paid order. Cash and wallet sales reverse exactly. A
// transfer (Paystack) sale credits Refunds payable (2310) instead of clearing,
// because the money has to be paid back out through the provider; the
// matching refund_pending record is settled later through
// POST /v1/unmatched-payments/{id}/refunded.
func Refund(ctx context.Context, tx pgx.Tx, st *store.Store, merchant store.Merchant, order store.Order, staffID uuid.UUID, idemKey, reason string) (store.Order, error) {
	if order.Status != "paid" || order.JournalID == nil {
		return store.Order{}, ledger.Err(ledger.CodeInvalidOrder, "only a paid order can be refunded")
	}
	ids := make([]uuid.UUID, len(order.Lines))
	for i, ln := range order.Lines {
		ids[i] = ln.ProductID
	}
	// Products first (id order), then stock (product id order): the same order
	// every sale and receipt uses, so they cannot deadlock with a refund.
	if _, err := st.LockProducts(ctx, tx, merchant.ID, ids); err != nil {
		return store.Order{}, err
	}
	if err := st.LockStock(ctx, tx, order.OutletID, ids); err != nil {
		return store.Order{}, err
	}
	if order.PaymentMethod == "wallet" {
		if order.CustomerID == nil {
			return store.Order{}, ledger.Err(ledger.CodeInvalidOrder, "wallet refund needs a customer")
		}
		if _, err := st.LockCustomer(ctx, tx, merchant.ID, *order.CustomerID); err != nil {
			return store.Order{}, err
		}
	}
	orig, err := st.GetJournal(ctx, tx, merchant.ID, *order.JournalID)
	if err != nil {
		return store.Order{}, err
	}
	if _, exists, err := st.ReversalOf(ctx, tx, merchant.ID, orig.ID); err != nil {
		return store.Order{}, err
	} else if exists {
		return store.Order{}, ledger.Err(ledger.CodeAlreadyReversed, "order already refunded")
	}
	for _, ln := range order.Lines {
		if err := st.Restock(ctx, tx, order.OutletID, ln.ProductID, ln.Qty); err != nil {
			return store.Order{}, err
		}
	}
	lines := ledger.ReverseLines(orig.Postings)
	transfer := order.PaymentMethod == "transfer"
	if transfer {
		if merchant.RefundsPayableID == nil || merchant.ClearingAccountID == nil || merchant.SuspenseAccountID == nil {
			return store.Order{}, ledger.Err(ledger.CodeInvalidOrder, "merchant books are not set up")
		}
		swapped := false
		for i := range lines {
			a := lines[i].AccountID
			if lines[i].Direction == ledger.Credit && (a == *merchant.ClearingAccountID || a == *merchant.SuspenseAccountID) {
				lines[i].AccountID = *merchant.RefundsPayableID
				swapped = true
			}
		}
		if !swapped {
			return store.Order{}, ledger.Err(ledger.CodeInvalidOrder, "transfer sale has no clearing line to refund")
		}
	}
	j, err := ledger.Post(ctx, tx, poster(st), ledger.PostRequest{
		MerchantID:        merchant.ID,
		Description:       "refund " + order.Reference,
		Lines:             lines,
		ReversesJournalID: &orig.ID,
		IdempotencyKey:    idemKey,
	})
	if err != nil {
		if le := ledger.AsError(err); le != nil && le.Code == ledger.CodeInsufficientFunds {
			return store.Order{}, ledger.Err(ledger.CodeRefundInsufficient, "refund cannot be posted: "+le.Message)
		}
		return store.Order{}, err
	}
	now := time.Now()
	if err := st.MarkRefunded(ctx, tx, order.ID, j.ID, staffID, reason, now); err != nil {
		return store.Order{}, err
	}
	if err := recordFullRefund(ctx, tx, st, order, j.ID, staffID, reason, now); err != nil {
		return store.Order{}, err
	}
	if order.PaymentMethod == "wallet" {
		if _, err := st.AddWalletBalance(ctx, tx, merchant.ID, *order.CustomerID, order.TotalMinor); err != nil {
			return store.Order{}, err
		}
	}
	if transfer {
		rid, err := newID()
		if err != nil {
			return store.Order{}, err
		}
		oid, outlet := order.ID, order.OutletID
		if err := st.InsertUnmatched(ctx, tx, store.Unmatched{
			ID: rid, MerchantID: merchant.ID, OrderID: &oid, OutletID: &outlet,
			Provider: "paystack", EventID: "refund:" + order.ID.String(),
			AmountMinor: order.TotalMinor, Currency: "NGN", Reason: "order_refund",
			Status: "refund_pending", CreatedAt: now, ResolvedAt: &now, ResolvedBy: &staffID, JournalID: &j.ID,
		}); err != nil {
			return store.Order{}, err
		}
		order.RefundRecordID = &rid
	}
	order.Status = "refunded"
	order.RefundJournalID = &j.ID
	order.RefundedAt = &now
	order.RefundedBy = &staffID
	order.RefundReason = reason
	outletID := order.OutletID
	if err := audit(ctx, tx, st, merchant.ID, staffID, &outletID, "order.refunded", "order", order.ID, map[string]any{
		"reference": order.Reference, "reversal_journal_id": j.ID.String(), "reason": reason,
	}); err != nil {
		return store.Order{}, err
	}
	return order, nil
}
