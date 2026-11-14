package checkout

import (
	"context"
	"encoding/json"
	"math"
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
	// Role is the actor's role ("admin", "manager", "cashier"); it picks the discount limit.
	Role           string
	DiscountReason string
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

// postSaleDebits posts the sale. The debit lines (one account, or several for a
// split tender) must add up to the order total.
func postSaleDebits(ctx context.Context, tx pgx.Tx, st *store.Store, in SaleInput, order store.Order, debits []ledger.Line, key string) (ledger.Journal, error) {
	if in.Merchant.VATAccountID == nil {
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
	return ledger.Post(ctx, tx, st.Poster(), ledger.PostRequest{
		MerchantID:     in.Merchant.ID,
		Description:    "sale " + order.Reference,
		Lines:          lines,
		IdempotencyKey: key,
	})
}

type ConfirmInput struct {
	Merchant       store.Merchant
	Outlet         store.Outlet
	Order          store.Order
	Now            time.Time
	IdempotencyKey string
	// ProviderEventID is stored on the payment row (payments.provider_event_id).
	ProviderEventID string
	// DebitAccountID is where the awaited money is: the clearing account of the
	// connection it arrived on, or suspense when an unmatched payment is applied.
	DebitAccountID uuid.UUID
	// ConnectionID and Provider say which connection confirmed the payment.
	ConnectionID *uuid.UUID
	Provider     string
	// StaffID is set when a person recorded or applied the payment.
	StaffID *uuid.UUID
	// Extra is added to the order.paid audit metadata.
	Extra map[string]any
}

// ConfirmAwaited pays a pending order whose awaited tender (transfer, card,
// tap, USSD, QR, mobile money) has been confirmed: the reservation becomes a
// deduction and the sale journal is posted.
func ConfirmAwaited(ctx context.Context, tx pgx.Tx, st *store.Store, in ConfirmInput) (store.Order, error) {
	ids := make([]uuid.UUID, len(in.Order.Lines))
	for i, ln := range in.Order.Lines {
		ids[i] = ln.ProductID
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
	// (2400); the sale clears those and debits the awaited money for the rest.
	held, err := st.HeldPayments(ctx, tx, in.Order.ID)
	if err != nil {
		return store.Order{}, err
	}
	var deposits int64
	for _, h := range held {
		deposits += h.AmountMinor
	}
	debits := []ledger.Line{{AccountID: in.DebitAccountID, Direction: ledger.Debit, Amount: in.Order.TotalMinor - deposits}}
	if deposits > 0 {
		dep, err := st.EnsureDepositsAccount(ctx, tx, in.Merchant.ID)
		if err != nil {
			return store.Order{}, err
		}
		debits = append(debits, ledger.Line{AccountID: dep, Direction: ledger.Debit, Amount: deposits})
	}
	sale := SaleInput{Merchant: in.Merchant, Outlet: in.Outlet}
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
	method, err := st.ConfirmPayment(ctx, tx, in.Order.ID, j.ID, eventID, in.ConnectionID, in.Provider)
	if err != nil {
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
	meta := map[string]any{
		"reference": in.Order.Reference, "total_minor": in.Order.TotalMinor,
		"discount_minor": in.Order.DiscountMinor, "cost_minor": cost, "method": method,
	}
	if in.ConnectionID != nil {
		meta["connection_id"] = in.ConnectionID.String()
	}
	for k, v := range in.Extra {
		meta[k] = v
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return store.Order{}, err
	}
	if err := st.InsertAudit(ctx, tx, store.AuditEvent{
		MerchantID: &mid, StaffID: in.StaffID, Action: "order.paid", EntityType: "order", EntityID: in.Order.ID, OutletID: &outletID, Metadata: raw,
	}); err != nil {
		return store.Order{}, err
	}
	return in.Order, nil
}
