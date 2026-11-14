package integration

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Phase 3A: split tender, partial refunds, discount controls, void, tax
// classes and the refund-day report. Every scenario ends with mustHold.

func num(m map[string]any, k string) int64 {
	f, _ := m[k].(float64)
	return int64(f)
}

func (c *apiClient) receiveCost(outlet, sku string, qty int, unit int64) {
	c.t.Helper()
	res := c.do(http.MethodPost, "/v1/stock/receipts", map[string]any{
		"outlet_id": outlet, "sku": sku, "qty": qty, "unit_cost_minor": unit,
	}, nil)
	if res.StatusCode != http.StatusOK {
		var out map[string]any
		decode(c.t, res, &out)
		c.t.Fatalf("receive %d %v", res.StatusCode, out)
	}
	decode(c.t, res, nil)
}

func (c *apiClient) product(sku string, price int64, class string) {
	c.t.Helper()
	res := c.do(http.MethodPost, "/v1/products", map[string]any{
		"sku": sku, "name": sku, "price_minor": price, "tax_class": class,
	}, nil)
	if res.StatusCode != http.StatusCreated {
		var out map[string]any
		decode(c.t, res, &out)
		c.t.Fatalf("product %s: %d %v", sku, res.StatusCode, out)
	}
	decode(c.t, res, nil)
}

func (c *apiClient) topup(outlet, cust string, amount int64) {
	c.t.Helper()
	r, o := c.postKeyed("/v1/customers/"+cust+"/topups", "tu-"+uuid.NewString(), map[string]any{"outlet_id": outlet, "amount_minor": amount})
	if r.StatusCode != 201 {
		c.t.Fatalf("topup %d %v", r.StatusCode, o)
	}
}

func (c *apiClient) placeOrder(body map[string]any) (*http.Response, map[string]any) {
	c.t.Helper()
	return c.postKeyed("/v1/orders", "o-"+uuid.NewString(), body)
}

func (c *apiClient) getMap(path string) map[string]any {
	c.t.Helper()
	res := c.do(http.MethodGet, path, nil, nil)
	var out map[string]any
	decode(c.t, res, &out)
	if res.StatusCode != 200 {
		c.t.Fatalf("GET %s: %d %v", path, res.StatusCode, out)
	}
	return out
}

func (sh *shop) staffClient(role, outlet string) *apiClient {
	sh.t.Helper()
	email := fmt.Sprintf("%s-%s@test.local", role, uuid.NewString()[:8])
	sh.admin.addStaff(email, role, outlet)
	return loginEmail(sh.t, email)
}

func (sh *shop) report(outlet, day string) map[string]any {
	sh.t.Helper()
	return sh.admin.getMap("/v1/reports/daily-sales?outlet_id=" + outlet + "&date=" + day)
}

func lagosDate(t *testing.T, offsetDays int) string {
	t.Helper()
	loc, err := time.LoadLocation("Africa/Lagos")
	if err != nil {
		t.Fatal(err)
	}
	return time.Now().In(loc).AddDate(0, 0, offsetDays).Format("2006-01-02")
}

func pays(order map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	list, _ := order["payments"].([]any)
	for _, p := range list {
		m := p.(map[string]any)
		out[m["method"].(string)] = m
	}
	return out
}

// ---- split tender ----

func TestSplitCashAndWalletPostsAtOnce(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("SERUM", "Serum", 100_000, 1)
	sh.admin.receive(sh.ikeja, "SERUM", 5)
	cust := sh.admin.customer("Ada", uniquePhone())
	sh.admin.topup(sh.ikeja, cust, 40_000)
	cashBefore := sh.bal("1000-IKJ")

	// Amounts that do not add up are refused and leave no trace.
	res, out := sh.admin.placeOrder(map[string]any{
		"outlet_id": sh.ikeja, "lines": []map[string]any{{"sku": "SERUM", "qty": 1}},
		"payments": []map[string]any{
			{"method": "cash", "amount_minor": 50_000},
			{"method": "wallet", "amount_minor": 40_000, "customer_id": cust},
		},
	})
	if res.StatusCode != 400 || errCode(out) != "tender_mismatch" {
		t.Fatalf("mismatch %d %v", res.StatusCode, out)
	}
	if sh.admin.stockAvailable(sh.ikeja, "SERUM") != 5 || sh.admin.customerBalance(cust) != 40_000 {
		t.Fatal("rejected split changed stock or wallet")
	}

	res, out = sh.admin.placeOrder(map[string]any{
		"outlet_id": sh.ikeja, "lines": []map[string]any{{"sku": "SERUM", "qty": 1}},
		"payments": []map[string]any{
			{"method": "cash", "amount_minor": 60_000},
			{"method": "wallet", "amount_minor": 40_000, "customer_id": cust},
		},
	})
	if res.StatusCode != 201 || out["status"] != "paid" || out["payment_method"] != "split" {
		t.Fatalf("split sale %d %v", res.StatusCode, out)
	}
	p := pays(out)
	if num(p["cash"], "amount_minor") != 60_000 || num(p["wallet"], "amount_minor") != 40_000 || p["cash"]["status"] != "confirmed" {
		t.Fatalf("payments %v", out["payments"])
	}
	if sh.admin.customerBalance(cust) != 0 || sh.bal("2200") != 0 {
		t.Fatal("wallet portion not taken")
	}
	if sh.bal("1000-IKJ")-cashBefore != 60_000 || sh.bal0("2400") != 0 {
		t.Fatal("cash portion not in the till")
	}
	if sh.admin.stockAvailable(sh.ikeja, "SERUM") != 4 {
		t.Fatal("stock not deducted")
	}
	rep := sh.report(sh.ikeja, lagosDate(t, 0))
	tt := rep["tender_totals"].(map[string]any)
	if num(tt, "cash_minor") != 60_000 || num(tt, "wallet_minor") != 40_000 || num(tt, "transfer_minor") != 0 || rep["matches_books"] != true {
		t.Fatalf("report %v", rep)
	}
	sh.mustHold()

	// A single tender in payments behaves like payment_method.
	res, out = sh.admin.placeOrder(map[string]any{
		"outlet_id": sh.ikeja, "lines": []map[string]any{{"sku": "SERUM", "qty": 1}},
		"payments": []map[string]any{{"method": "cash", "amount_minor": 100_000}},
	})
	if res.StatusCode != 201 || out["payment_method"] != "cash" || out["status"] != "paid" {
		t.Fatalf("single tender %d %v", res.StatusCode, out)
	}
	// payment_method and payments together are ambiguous.
	res, out = sh.admin.placeOrder(map[string]any{
		"outlet_id": sh.ikeja, "payment_method": "cash", "lines": []map[string]any{{"sku": "SERUM", "qty": 1}},
		"payments": []map[string]any{{"method": "cash", "amount_minor": 100_000}},
	})
	if res.StatusCode != 400 {
		t.Fatalf("both shapes %d %v", res.StatusCode, out)
	}
	sh.mustHold()
}

func TestSplitWithTransferHoldsDepositsUntilWebhook(t *testing.T) {
	sh := openShop(t, 750)
	sh.admin.createProduct("SERUM", "Serum", 100_000, 1)
	sh.admin.receive(sh.ikeja, "SERUM", 5)
	res, out := sh.admin.placeOrder(map[string]any{
		"outlet_id": sh.ikeja, "lines": []map[string]any{{"sku": "SERUM", "qty": 1}},
		"payments": []map[string]any{
			{"method": "cash", "amount_minor": 30_000},
			{"method": "transfer", "amount_minor": 70_000},
		},
	})
	if res.StatusCode != 201 || out["status"] != "pending_payment" || out["payment_method"] != "split" {
		t.Fatalf("split order %d %v", res.StatusCode, out)
	}
	p := pays(out)
	if p["cash"]["status"] != "held" || p["transfer"]["status"] != "pending" || num(p["transfer"], "amount_minor") != 70_000 {
		t.Fatalf("payments %v", out["payments"])
	}
	if sh.bal0("2400") != 30_000 || sh.bal("1000-IKJ") != 30_000 {
		t.Fatalf("deposit 2400=%d cash=%d", sh.bal0("2400"), sh.bal("1000-IKJ"))
	}
	if sh.admin.stockAvailable(sh.ikeja, "SERUM") != 4 {
		t.Fatal("stock not reserved")
	}
	sh.mustHold()

	// The webhook must be for the transfer portion, not the whole total.
	ref := out["reference"].(string)
	wres, wout := sh.admin.webhook(nextEvent(), ref, 100_000, "NGN")
	if wres.StatusCode != 200 || wout["status"] != "unmatched" || wout["reason"] != "amount_mismatch" {
		t.Fatalf("whole-total webhook %d %v", wres.StatusCode, wout)
	}
	wres, wout = sh.admin.webhook(nextEvent(), ref, 70_000, "NGN")
	if wres.StatusCode != 200 || wout["status"] != "paid" {
		t.Fatalf("transfer webhook %d %v", wres.StatusCode, wout)
	}
	p = pays(wout)
	if p["cash"]["status"] != "confirmed" || p["transfer"]["status"] != "confirmed" {
		t.Fatalf("confirmed payments %v", wout["payments"])
	}
	if sh.bal0("2400") != 0 {
		t.Fatalf("deposits left %d", sh.bal0("2400"))
	}
	got := sh.admin.getMap("/v1/orders/" + out["id"].(string))
	if got["status"] != "paid" || num(got, "vat_minor") != 100_000*750/10750 {
		t.Fatalf("order %v", got)
	}
	rep := sh.report(sh.ikeja, lagosDate(t, 0))
	tt := rep["tender_totals"].(map[string]any)
	if num(tt, "cash_minor") != 30_000 || num(tt, "transfer_minor") != 70_000 || num(rep, "total_minor") != 100_000 {
		t.Fatalf("report %v", rep)
	}
	if rep["matches_books"] != true {
		t.Fatalf("matches_books %v", rep)
	}
	// Only the stray 100k payment is parked in suspense; it is refundable.
	if got := len(sh.admin.listUnmatched(sh.ikeja, "open")); got != 1 {
		t.Fatalf("open unmatched %d", got)
	}
	sh.mustHold()
}

func TestVoidPendingSplitReturnsDepositsAndLateWebhookIsUnmatched(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("SERUM", "Serum", 100_000, 1)
	sh.admin.receive(sh.ikeja, "SERUM", 5)
	cust := sh.admin.customer("Ada", uniquePhone())
	sh.admin.topup(sh.ikeja, cust, 20_000)
	res, out := sh.admin.placeOrder(map[string]any{
		"outlet_id": sh.ikeja, "lines": []map[string]any{{"sku": "SERUM", "qty": 1}},
		"payments": []map[string]any{
			{"method": "cash", "amount_minor": 30_000},
			{"method": "wallet", "amount_minor": 20_000, "customer_id": cust},
			{"method": "transfer", "amount_minor": 50_000},
		},
	})
	if res.StatusCode != 201 || out["status"] != "pending_payment" {
		t.Fatalf("order %d %v", res.StatusCode, out)
	}
	id := out["id"].(string)
	if sh.bal0("2400") != 50_000 || sh.admin.customerBalance(cust) != 0 {
		t.Fatal("deposits not held")
	}
	sh.mustHold()

	other := sh.staffClient("cashier", sh.sul)
	if r, o := other.postKeyed("/v1/orders/"+id+"/void", "v-"+uuid.NewString(), map[string]any{}); r.StatusCode != 403 {
		t.Fatalf("other outlet void %d %v", r.StatusCode, o)
	}
	cashier := sh.staffClient("cashier", sh.ikeja)
	key := "v-" + uuid.NewString()
	r, v := cashier.postKeyed("/v1/orders/"+id+"/void", key, map[string]any{"reason": "customer left"})
	if r.StatusCode != 200 || v["status"] != "voided" {
		t.Fatalf("void %d %v", r.StatusCode, v)
	}
	// Same key replays; a new key finds the order no longer pending.
	if r2, _ := cashier.postKeyed("/v1/orders/"+id+"/void", key, map[string]any{"reason": "customer left"}); r2.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatal("void not replayed")
	}
	if r3, o3 := cashier.postKeyed("/v1/orders/"+id+"/void", "v-"+uuid.NewString(), map[string]any{}); r3.StatusCode != 400 {
		t.Fatalf("second void %d %v", r3.StatusCode, o3)
	}
	if sh.bal0("2400") != 0 || sh.admin.customerBalance(cust) != 20_000 || sh.bal("2200") != 20_000 {
		t.Fatalf("deposits not returned 2400=%d", sh.bal0("2400"))
	}
	if sh.bal("1000-IKJ") != 20_000 {
		t.Fatalf("cash %d", sh.bal("1000-IKJ"))
	}
	if sh.admin.stockAvailable(sh.ikeja, "SERUM") != 5 {
		t.Fatal("reservation not released")
	}
	var n int
	if err := testPool.QueryRow(context.Background(), `SELECT COUNT(*) FROM audit_events WHERE entity_id = $1 AND action = 'order.voided'`, id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("void audit %d %v", n, err)
	}
	sh.mustHold()

	// The customer pays the transfer anyway: it lands in unmatched payments.
	wres, wout := sh.admin.webhook(nextEvent(), out["reference"].(string), 50_000, "NGN")
	if wres.StatusCode != 200 || wout["status"] != "unmatched" || wout["reason"] != "order_voided" {
		t.Fatalf("late webhook %d %v", wres.StatusCode, wout)
	}
	open := sh.admin.listUnmatched(sh.ikeja, "open")
	if len(open) != 1 || open[0]["reason"] != "order_voided" || num(open[0], "amount_minor") != 50_000 {
		t.Fatalf("unmatched %v", open)
	}
	if got := sh.admin.getMap("/v1/orders/" + id); got["status"] != "voided" {
		t.Fatalf("order after webhook %v", got["status"])
	}
	if list := sh.admin.getMap("/v1/orders?status=voided"); len(list["orders"].([]any)) != 1 {
		t.Fatalf("voided filter %v", list)
	}
	sh.mustHold()
}

func TestExpiredSplitReturnsDeposits(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("SERUM", "Serum", 100_000, 1)
	sh.admin.receive(sh.ikeja, "SERUM", 5)
	res, out := sh.admin.placeOrder(map[string]any{
		"outlet_id": sh.ikeja, "lines": []map[string]any{{"sku": "SERUM", "qty": 2}},
		"payments": []map[string]any{
			{"method": "cash", "amount_minor": 50_000},
			{"method": "transfer", "amount_minor": 150_000},
		},
	})
	if res.StatusCode != 201 {
		t.Fatalf("order %d %v", res.StatusCode, out)
	}
	sh.expireOrder(out["id"].(string))
	if sh.bal0("2400") != 0 || sh.bal("1000-IKJ") != 0 {
		t.Fatalf("deposits after expiry 2400=%d cash=%d", sh.bal0("2400"), sh.bal("1000-IKJ"))
	}
	if sh.admin.stockAvailable(sh.ikeja, "SERUM") != 5 {
		t.Fatal("stock not released")
	}
	got := sh.admin.getMap("/v1/orders/" + out["id"].(string))
	if p := pays(got); p["cash"]["status"] != "returned" {
		t.Fatalf("payments %v", got["payments"])
	}
	sh.mustHold()
}

// ---- partial refunds ----

func TestPartialRefundsApportionDiscountAndVAT(t *testing.T) {
	sh := openShop(t, 750)
	sh.admin.createProduct("AAA", "A", 100_000, 0)
	sh.admin.createProduct("BBB", "B", 50_000, 0)
	sh.admin.receiveCost(sh.ikeja, "AAA", 10, 40_000)
	sh.admin.receiveCost(sh.ikeja, "BBB", 10, 20_000)
	res, out := sh.admin.placeOrder(map[string]any{
		"outlet_id": sh.ikeja, "payment_method": "cash", "discount_minor": 30_000, "discount_reason": "promo",
		"lines": []map[string]any{{"sku": "AAA", "qty": 3}, {"sku": "BBB", "qty": 2}},
	})
	if res.StatusCode != 201 || num(out, "total_minor") != 370_000 || out["discount_reason"] != "promo" {
		t.Fatalf("order %d %v", res.StatusCode, out)
	}
	id := out["id"].(string)
	type line struct{ qty, disc, vat, net, total int64 }
	lines := map[string]line{}
	var sumDisc, sumVAT, sumNet int64
	for _, l := range out["lines"].([]any) {
		m := l.(map[string]any)
		ln := line{num(m, "qty"), num(m, "discount_minor"), num(m, "vat_minor"), num(m, "net_minor"), num(m, "line_total_minor")}
		if ln.net+ln.vat+ln.disc != ln.total {
			t.Fatalf("line does not add up %v", m)
		}
		lines[m["sku"].(string)] = ln
		sumDisc += ln.disc
		sumVAT += ln.vat
		sumNet += ln.net
	}
	if sumDisc != 30_000 || sumVAT != num(out, "vat_minor") || sumNet != num(out, "net_minor") {
		t.Fatalf("lines %v do not sum to the order %v", lines, out)
	}
	if lines["AAA"].disc != 22_500 || lines["BBB"].disc != 7_500 {
		t.Fatalf("discount split %v", lines)
	}
	cum := func(v, k, q int64) int64 { return v * k / q }
	want := func(sku string, done, qty int64) int64 {
		l := lines[sku]
		return (cum(l.net, done+qty, l.qty) - cum(l.net, done, l.qty)) + (cum(l.vat, done+qty, l.qty) - cum(l.vat, done, l.qty))
	}

	refund := func(body map[string]any) (*http.Response, map[string]any) {
		return sh.admin.postKeyed("/v1/orders/"+id+"/refund", "rf-"+uuid.NewString(), body)
	}
	one := map[string]any{"lines": []map[string]any{{"sku": "AAA", "quantity": 1}}, "reason": "one damaged"}
	r1, o1 := refund(one)
	rf1 := o1["refund"].(map[string]any)
	if r1.StatusCode != 201 || o1["status"] != "partially_refunded" || num(rf1, "amount_minor") != want("AAA", 0, 1) || rf1["journal_id"] == nil {
		t.Fatalf("refund 1 %d %v", r1.StatusCode, o1)
	}
	if sh.admin.stockAvailable(sh.ikeja, "AAA") != 8 {
		t.Fatal("unit not restocked")
	}
	sh.mustHold()

	// Same key, same body: replay, not a second refund.
	k := "rf-" + uuid.NewString()
	rA, _ := sh.admin.postKeyed("/v1/orders/"+id+"/refund", k, map[string]any{"lines": []map[string]any{{"sku": "BBB", "quantity": 1}}})
	rB, _ := sh.admin.postKeyed("/v1/orders/"+id+"/refund", k, map[string]any{"lines": []map[string]any{{"sku": "BBB", "quantity": 1}}})
	if rA.StatusCode != 201 || rB.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay %d %s", rA.StatusCode, rB.Header.Get("Idempotent-Replayed"))
	}

	// Too many units, an unknown line and a duplicate line are refused.
	if r, o := refund(map[string]any{"lines": []map[string]any{{"sku": "AAA", "quantity": 3}}}); r.StatusCode != 409 || errCode(o) != "refund_exceeds_order" {
		t.Fatalf("exceeds %d %v", r.StatusCode, o)
	}
	if r, o := refund(map[string]any{"lines": []map[string]any{{"sku": "ZZZ", "quantity": 1}}}); r.StatusCode != 400 {
		t.Fatalf("unknown %d %v", r.StatusCode, o)
	}
	if r, o := refund(map[string]any{"lines": []map[string]any{{"sku": "AAA", "quantity": 1}, {"sku": "AAA", "quantity": 1}}}); r.StatusCode != 400 {
		t.Fatalf("duplicate %d %v", r.StatusCode, o)
	}
	sh.mustHold()

	// The rest in one go: empty lines = everything still unrefunded.
	r3, o3 := refund(map[string]any{})
	if r3.StatusCode != 201 || o3["status"] != "refunded" {
		t.Fatalf("final refund %d %v", r3.StatusCode, o3)
	}
	if num(o3, "refunded_minor") != 370_000 {
		t.Fatalf("refunded_minor %v", o3["refunded_minor"])
	}
	var parts, sum int64
	rows, err := testPool.Query(context.Background(), `SELECT amount_minor FROM order_refunds WHERE order_id = $1`, id)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a int64
		_ = rows.Scan(&a)
		parts++
		sum += a
	}
	rows.Close()
	if parts != 3 || sum != 370_000 {
		t.Fatalf("refund rows %d sum %d", parts, sum)
	}
	if sh.admin.stockAvailable(sh.ikeja, "AAA") != 10 || sh.admin.stockAvailable(sh.ikeja, "BBB") != 10 {
		t.Fatal("stock not fully restored")
	}
	if sh.bal("4000-IKJ") != 0 || sh.bal("2100") != 0 || sh.bal("1000-IKJ") != 0 || sh.bal("5000") != 0 {
		t.Fatalf("books not back to zero: sales=%d vat=%d cash=%d cogs=%d", sh.bal("4000-IKJ"), sh.bal("2100"), sh.bal("1000-IKJ"), sh.bal("5000"))
	}
	if r, o := refund(map[string]any{}); r.StatusCode != 400 {
		t.Fatalf("refund of refunded %d %v", r.StatusCode, o)
	}
	sh.mustHold()
}

func TestRefundSplitTenderRule(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("SERUM", "Serum", 100_000, 0)
	sh.admin.receive(sh.ikeja, "SERUM", 5)
	cust := sh.admin.customer("Ada", uniquePhone())
	sh.admin.topup(sh.ikeja, cust, 80_000)
	res, out := sh.admin.placeOrder(map[string]any{
		"outlet_id": sh.ikeja, "lines": []map[string]any{{"sku": "SERUM", "qty": 2}},
		"payments": []map[string]any{
			{"method": "cash", "amount_minor": 120_000},
			{"method": "wallet", "amount_minor": 80_000, "customer_id": cust},
		},
	})
	if res.StatusCode != 201 {
		t.Fatalf("order %d %v", res.StatusCode, out)
	}
	id := out["id"].(string)
	refund := func(body map[string]any) (*http.Response, map[string]any) {
		return sh.admin.postKeyed("/v1/orders/"+id+"/refund", "rf-"+uuid.NewString(), body)
	}
	one := []map[string]any{{"sku": "SERUM", "quantity": 1}}
	// No refund_to: pro rata across the tenders (60/40).
	r1, o1 := refund(map[string]any{"lines": one})
	tenders := o1["refund"].(map[string]any)["tenders"].(map[string]any)
	if r1.StatusCode != 201 || num(tenders, "cash") != 60_000 || num(tenders, "wallet") != 40_000 {
		t.Fatalf("pro rata %d %v", r1.StatusCode, o1)
	}
	if sh.admin.customerBalance(cust) != 40_000 {
		t.Fatalf("wallet balance %d", sh.admin.customerBalance(cust))
	}
	// refund_to must name a tender with enough left.
	if r, o := refund(map[string]any{"lines": one, "refund_to": "wallet"}); r.StatusCode != 400 {
		t.Fatalf("wallet has only 40k left %d %v", r.StatusCode, o)
	}
	if r, o := refund(map[string]any{"lines": one, "refund_to": "transfer"}); r.StatusCode != 400 {
		t.Fatalf("no transfer tender %d %v", r.StatusCode, o)
	}
	sh.mustHold()
	// Pay the remainder back to cash only is not possible either (60k left), so go pro rata.
	r2, o2 := refund(map[string]any{})
	if r2.StatusCode != 201 || o2["status"] != "refunded" {
		t.Fatalf("rest %d %v", r2.StatusCode, o2)
	}
	if sh.admin.customerBalance(cust) != 80_000 || sh.bal("2200") != 80_000 || sh.bal("1000-IKJ") != 80_000 {
		t.Fatalf("after refunds wallet=%d 2200=%d cash=%d", sh.admin.customerBalance(cust), sh.bal("2200"), sh.bal("1000-IKJ"))
	}
	sh.mustHold()
}

func TestRefundSplitTransferPortionGoesToRefundsPayable(t *testing.T) {
	sh := openShop(t, 750)
	sh.admin.createProduct("SERUM", "Serum", 100_000, 0)
	sh.admin.receive(sh.ikeja, "SERUM", 5)
	_, out := sh.admin.placeOrder(map[string]any{
		"outlet_id": sh.ikeja, "lines": []map[string]any{{"sku": "SERUM", "qty": 1}},
		"payments": []map[string]any{
			{"method": "cash", "amount_minor": 30_000},
			{"method": "transfer", "amount_minor": 70_000},
		},
	})
	if r, o := sh.admin.webhook(nextEvent(), out["reference"].(string), 70_000, "NGN"); r.StatusCode != 200 || o["status"] != "paid" {
		t.Fatalf("webhook %d %v", r.StatusCode, o)
	}
	id := out["id"].(string)
	// A refund straight to a transfer for more than the transfer portion is refused.
	if r, o := sh.admin.postKeyed("/v1/orders/"+id+"/refund", "rf-"+uuid.NewString(), map[string]any{"refund_to": "transfer"}); r.StatusCode != 400 {
		t.Fatalf("refund_to transfer %d %v", r.StatusCode, o)
	}
	r, o := sh.admin.postKeyed("/v1/orders/"+id+"/refund", "rf-"+uuid.NewString(), map[string]any{"reason": "returned"})
	if r.StatusCode != 201 || o["status"] != "refunded" || o["refund_record_id"] == nil {
		t.Fatalf("refund %d %v", r.StatusCode, o)
	}
	tenders := o["refund"].(map[string]any)["tenders"].(map[string]any)
	if num(tenders, "cash") != 30_000 || num(tenders, "transfer") != 70_000 {
		t.Fatalf("tenders %v", tenders)
	}
	if sh.bal("2310") != 70_000 || sh.bal("1000-IKJ") != 0 || sh.bal0("2400") != 0 {
		t.Fatalf("2310=%d cash=%d 2400=%d", sh.bal("2310"), sh.bal("1000-IKJ"), sh.bal0("2400"))
	}
	pend := sh.admin.listUnmatched(sh.ikeja, "refund_pending")
	if len(pend) != 1 || num(pend[0], "amount_minor") != 70_000 || pend[0]["id"] != o["refund_record_id"] {
		t.Fatalf("pending %v", pend)
	}
	sh.mustHold()
	if r, o := sh.admin.postKeyed("/v1/unmatched-payments/"+pend[0]["id"].(string)+"/refunded", "rd-"+uuid.NewString(), map[string]any{}); r.StatusCode != 200 {
		t.Fatalf("settle %d %v", r.StatusCode, o)
	}
	if sh.bal("2310") != 0 {
		t.Fatal("refund not settled")
	}
	sh.mustHold()
}

// ---- discount controls ----

func TestDiscountLimitsByRoleAndSettings(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("SERUM", "Serum", 100_000, 0)
	sh.admin.receive(sh.ikeja, "SERUM", 30)
	cashier := sh.staffClient("cashier", sh.ikeja)
	manager := sh.staffClient("manager", sh.ikeja)
	order := func(c *apiClient, discount int64, reason string) (*http.Response, map[string]any) {
		body := map[string]any{
			"outlet_id": sh.ikeja, "payment_method": "cash", "discount_minor": discount,
			"lines": []map[string]any{{"sku": "SERUM", "qty": 1}},
		}
		if reason != "" {
			body["discount_reason"] = reason
		}
		return c.placeOrder(body)
	}
	if r, o := order(cashier, 10_000, ""); r.StatusCode != 400 || errCode(o) != "discount_reason_required" {
		t.Fatalf("no reason %d %v", r.StatusCode, o)
	}
	if r, o := order(cashier, 10_001, "regular"); r.StatusCode != 403 || errCode(o) != "discount_limit" {
		t.Fatalf("cashier over %d %v", r.StatusCode, o)
	}
	if r, o := order(cashier, 10_000, "regular"); r.StatusCode != 201 || o["discount_reason"] != "regular" {
		t.Fatalf("cashier at limit %d %v", r.StatusCode, o)
	}
	if r, o := order(manager, 30_000, "bulk"); r.StatusCode != 201 {
		t.Fatalf("manager at limit %d %v", r.StatusCode, o)
	}
	if r, o := order(manager, 30_001, "bulk"); r.StatusCode != 403 || errCode(o) != "discount_limit" {
		t.Fatalf("manager over %d %v", r.StatusCode, o)
	}
	if r, o := order(sh.admin, 90_000, "clearance"); r.StatusCode != 201 {
		t.Fatalf("admin 90%% %d %v", r.StatusCode, o)
	}
	if r, o := order(sh.admin, 100_000, "free"); r.StatusCode != 400 {
		t.Fatalf("admin cannot discount the whole total %d %v", r.StatusCode, o)
	}
	// No discount, no reason needed.
	if r, o := order(cashier, 0, ""); r.StatusCode != 201 {
		t.Fatalf("plain %d %v", r.StatusCode, o)
	}

	// Only an admin changes the limits.
	deny := cashier.do(http.MethodPatch, "/v1/merchant/settings", map[string]any{"max_discount_bps_cashier": 9000}, nil)
	if deny.StatusCode != 403 {
		t.Fatalf("cashier settings %d", deny.StatusCode)
	}
	decode(t, deny, nil)
	bad := sh.admin.do(http.MethodPatch, "/v1/merchant/settings", map[string]any{"max_discount_bps_cashier": 10_001}, nil)
	if bad.StatusCode != 400 {
		t.Fatalf("bad bps %d", bad.StatusCode)
	}
	decode(t, bad, nil)
	res := sh.admin.do(http.MethodPatch, "/v1/merchant/settings", map[string]any{"max_discount_bps_cashier": 2000}, nil)
	var set map[string]any
	decode(t, res, &set)
	if res.StatusCode != 200 || num(set, "max_discount_bps_cashier") != 2000 || num(set, "max_discount_bps_manager") != 3000 {
		t.Fatalf("settings %d %v", res.StatusCode, set)
	}
	if r, o := order(cashier, 20_000, "raised"); r.StatusCode != 201 {
		t.Fatalf("cashier after raise %d %v", r.StatusCode, o)
	}
	sh.mustHold()
}

// ---- tax classes ----

func TestTaxClassesAndVATRegistration(t *testing.T) {
	sh := openShop(t, 750)
	sh.admin.product("STD", 107_500, "standard")
	sh.admin.product("EXM", 50_000, "exempt")
	sh.admin.product("ZRO", 30_000, "zero")
	for _, s := range []string{"STD", "EXM", "ZRO"} {
		sh.admin.receive(sh.ikeja, s, 10)
	}
	lines := []map[string]any{{"sku": "STD", "qty": 1}, {"sku": "EXM", "qty": 1}, {"sku": "ZRO", "qty": 1}}
	res, out := sh.admin.placeOrder(map[string]any{"outlet_id": sh.ikeja, "payment_method": "cash", "lines": lines})
	if res.StatusCode != 201 || num(out, "total_minor") != 187_500 || num(out, "vat_minor") != 7_500 || num(out, "net_minor") != 180_000 {
		t.Fatalf("mixed order %d %v", res.StatusCode, out)
	}
	byClass := map[string]int64{}
	for _, l := range out["lines"].([]any) {
		m := l.(map[string]any)
		byClass[m["tax_class"].(string)] = num(m, "vat_minor")
	}
	if byClass["standard"] != 7_500 || byClass["exempt"] != 0 || byClass["zero"] != 0 {
		t.Fatalf("line vat %v", byClass)
	}
	// Discount is apportioned before VAT is worked out per line.
	res, out = sh.admin.placeOrder(map[string]any{
		"outlet_id": sh.ikeja, "payment_method": "cash", "lines": lines,
		"discount_minor": 18_750, "discount_reason": "loyalty",
	})
	if res.StatusCode != 201 || num(out, "total_minor") != 168_750 || num(out, "vat_minor") != (107_500-10_750)*750/10750 {
		t.Fatalf("discounted mixed order %d %v", res.StatusCode, out)
	}
	sh.mustHold()

	// A manager cannot change tax class; an admin can.
	manager := sh.staffClient("manager", sh.ikeja)
	prods := sh.admin.getMap("/v1/products")["products"].([]any)
	var exmID string
	for _, p := range prods {
		if m := p.(map[string]any); m["sku"] == "EXM" {
			exmID = m["id"].(string)
			if m["tax_class"] != "exempt" {
				t.Fatalf("product %v", m)
			}
		}
	}
	if r, o := manager.postKeyedPatch("/v1/products/"+exmID, map[string]any{"tax_class": "standard"}); r.StatusCode != 403 {
		t.Fatalf("manager tax class %d %v", r.StatusCode, o)
	}
	if r, o := sh.admin.postKeyedPatch("/v1/products/"+exmID, map[string]any{"tax_class": "bogus"}); r.StatusCode != 400 {
		t.Fatalf("bad class %d %v", r.StatusCode, o)
	}
	if r, o := sh.admin.postKeyedPatch("/v1/products/"+exmID, map[string]any{"tax_class": "standard"}); r.StatusCode != 200 || o["tax_class"] != "standard" {
		t.Fatalf("admin tax class %d %v", r.StatusCode, o)
	}

	// Not VAT registered: no VAT on anything, whatever the class.
	r := sh.admin.do(http.MethodPatch, "/v1/merchant/settings", map[string]any{"vat_registered": false}, nil)
	if r.StatusCode != 200 {
		t.Fatalf("settings %d", r.StatusCode)
	}
	decode(t, r, nil)
	res, out = sh.admin.placeOrder(map[string]any{"outlet_id": sh.ikeja, "payment_method": "cash", "lines": lines})
	if res.StatusCode != 201 || num(out, "vat_minor") != 0 || num(out, "net_minor") != 187_500 {
		t.Fatalf("unregistered %d %v", res.StatusCode, out)
	}
	if sh.bal("2100") != 7_500+6_750 {
		t.Fatalf("vat payable %d", sh.bal("2100"))
	}
	sh.mustHold()
}

func (c *apiClient) postKeyedPatch(path string, body map[string]any) (*http.Response, map[string]any) {
	c.t.Helper()
	res := c.do(http.MethodPatch, path, body, keyHeader("p-"+uuid.NewString()))
	var out map[string]any
	decode(c.t, res, &out)
	return res, out
}

// ---- daily report ----

func TestDailyReportCountsRefundsOnTheRefundDay(t *testing.T) {
	sh := openShop(t, 750)
	sh.admin.createProduct("SERUM", "Serum", 100_000, 0)
	sh.admin.receive(sh.ikeja, "SERUM", 10)
	cashier := sh.staffClient("cashier", sh.ikeja)
	res, sale := cashier.placeOrder(map[string]any{
		"outlet_id": sh.ikeja, "payment_method": "cash", "discount_minor": 5_000, "discount_reason": "regular",
		"lines": []map[string]any{{"sku": "SERUM", "qty": 2}},
	})
	if res.StatusCode != 201 {
		t.Fatalf("sale %d %v", res.StatusCode, sale)
	}
	id := sale["id"].(string)
	if _, o := sh.admin.placeOrder(map[string]any{
		"outlet_id": sh.ikeja, "payment_method": "cash", "lines": []map[string]any{{"sku": "SERUM", "qty": 1}},
	}); o["status"] != "paid" {
		t.Fatalf("second sale %v", o)
	}
	// Move the first sale to yesterday, then refund it today.
	if _, err := testPool.Exec(context.Background(), `UPDATE orders SET paid_at = paid_at - interval '1 day' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	r, o := sh.admin.postKeyed("/v1/orders/"+id+"/refund", "rf-"+uuid.NewString(), map[string]any{
		"lines": []map[string]any{{"sku": "SERUM", "quantity": 1}},
	})
	if r.StatusCode != 201 {
		t.Fatalf("refund %d %v", r.StatusCode, o)
	}
	refunded := num(o["refund"].(map[string]any), "amount_minor")
	if r, o := sh.admin.postKeyed("/v1/orders/"+id+"/refund", "rf-"+uuid.NewString(), map[string]any{}); r.StatusCode != 201 || o["status"] != "refunded" {
		t.Fatalf("rest %d %v", r.StatusCode, o)
	}

	yday := sh.report(sh.ikeja, lagosDate(t, -1))
	if num(yday, "order_count") != 1 || num(yday, "total_minor") != 195_000 || num(yday, "refunds_minor") != 0 ||
		num(yday, "net_sales_minor") != 195_000 || num(yday, "discount_total_minor") != 5_000 || yday["matches_books"] != true {
		t.Fatalf("yesterday %v", yday)
	}
	today := sh.report(sh.ikeja, lagosDate(t, 0))
	if num(today, "order_count") != 1 || num(today, "total_minor") != 100_000 ||
		num(today, "refunds_minor") != 195_000 || num(today, "refund_count") != 2 ||
		num(today, "net_sales_minor") != 100_000-195_000 || today["matches_books"] != true {
		t.Fatalf("today %v (first refund %d)", today, refunded)
	}
	byCashier := today["by_cashier"].([]any)
	var adminRefunds, cashierSales int64
	for _, c := range byCashier {
		m := c.(map[string]any)
		adminRefunds += num(m, "refunds_minor")
		cashierSales += num(m, "total_minor")
	}
	if adminRefunds != 195_000 || cashierSales != 100_000 || len(byCashier) != 1 {
		t.Fatalf("by_cashier %v", byCashier)
	}
	sh.mustHold()
}

func TestSplitSalesCountOnSaleDayEvenWhenRefundedLater(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("SERUM", "Serum", 100_000, 0)
	sh.admin.receive(sh.ikeja, "SERUM", 4)
	_, o := sh.admin.placeOrder(map[string]any{
		"outlet_id": sh.ikeja, "payment_method": "cash", "lines": []map[string]any{{"sku": "SERUM", "qty": 1}},
	})
	if r, _ := sh.admin.postKeyed("/v1/orders/"+o["id"].(string)+"/refund", "rf-"+uuid.NewString(), map[string]any{}); r.StatusCode != 201 {
		t.Fatal("refund failed")
	}
	rep := sh.report(sh.ikeja, lagosDate(t, 0))
	if num(rep, "total_minor") != 100_000 || num(rep, "refunds_minor") != 100_000 || num(rep, "net_sales_minor") != 0 || rep["matches_books"] != true {
		t.Fatalf("report %v", rep)
	}
	sh.mustHold()
}

// bal0 is the balance of an account that may not exist yet (0 if it does not).
func (sh *shop) bal0(code string) int64 {
	sh.t.Helper()
	var n int64
	err := testPool.QueryRow(context.Background(), `SELECT balance_minor FROM accounts WHERE merchant_id = $1 AND code = $2`, sh.merchant, code).Scan(&n)
	if err != nil {
		return 0
	}
	return n
}
