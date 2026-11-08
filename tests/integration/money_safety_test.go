package integration

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Muddyscode/till/internal/checkout"
	"github.com/Muddyscode/till/internal/httpserver"
	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func keyHeader(key string) http.Header {
	h := http.Header{}
	h.Set("Idempotency-Key", key)
	return h
}

func (c *apiClient) postKeyed(path, key string, body any) (*http.Response, map[string]any) {
	c.t.Helper()
	res := c.do(http.MethodPost, path, body, keyHeader(key))
	var out map[string]any
	decode(c.t, res, &out)
	return res, out
}

var eventSeq = time.Now().UnixNano() % 1_000_000_000

func nextEvent() int64 {
	eventSeq++
	return eventSeq
}

func (c *apiClient) webhook(id int64, ref string, amount int64, currency string) (*http.Response, map[string]any) {
	c.t.Helper()
	body := fmt.Sprintf(`{"event":"charge.success","data":{"id":%d,"reference":%q,"amount":%d,"currency":%q,"status":"success"}}`, id, ref, amount, currency)
	h := http.Header{}
	h.Set("x-paystack-signature", httpserver.SignPaystack("test-paystack-secret", []byte(body)))
	res := c.doRaw(http.MethodPost, "/v1/webhooks/paystack", body, h)
	var out map[string]any
	decode(c.t, res, &out)
	return res, out
}

func (sh *shop) bal(code string) int64 {
	sh.t.Helper()
	_, b := sh.admin.accountByCode(code)
	return b
}

func (sh *shop) mustHold() {
	sh.t.Helper()
	inv := sh.admin.identity()
	if inv["holds"] != true {
		sh.t.Fatalf("invariants broken: %v", inv)
	}
}

func (sh *shop) expireOrder(id string) {
	sh.t.Helper()
	if _, err := testPool.Exec(context.Background(), `UPDATE orders SET expires_at = now() - interval '1 minute' WHERE id = $1`, id); err != nil {
		sh.t.Fatal(err)
	}
	st := store.New(testPool)
	failed := map[uuid.UUID]struct{}{}
	checkout.SweepExpired(context.Background(), st, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), time.Now(), failed)
	var status string
	if err := testPool.QueryRow(context.Background(), `SELECT status FROM orders WHERE id = $1`, id).Scan(&status); err != nil {
		sh.t.Fatal(err)
	}
	if status != "expired" {
		sh.t.Fatalf("order status %s, want expired", status)
	}
}

// transferShop opens a shop with a 1000 kobo-priced product and a pending transfer order for qty units.
func transferShop(t *testing.T, stock, qty int) (*shop, map[string]any) {
	t.Helper()
	sh := openShop(t, 750)
	sh.admin.createProduct("SERUM", "Serum", 100_000, 1)
	sh.admin.receive(sh.ikeja, "SERUM", stock)
	res, out := sh.admin.order("t-"+uuid.NewString(), sh.ikeja, "transfer", "SERUM", qty)
	if res.StatusCode != 201 {
		t.Fatalf("transfer order %d %v", res.StatusCode, out)
	}
	return sh, out
}

func (c *apiClient) listUnmatched(outlet, status string) []map[string]any {
	c.t.Helper()
	res := c.do(http.MethodGet, "/v1/unmatched-payments?outlet_id="+outlet+"&status="+status, nil, nil)
	var out struct {
		Items []map[string]any `json:"unmatched_payments"`
	}
	decode(c.t, res, &out)
	if res.StatusCode != 200 {
		c.t.Fatalf("list unmatched %d", res.StatusCode)
	}
	return out.Items
}

// ---- 1. idempotency scoping ----

func TestIdempotencyKeyScopedToRouteAndReplayHeader(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("SERUM", "Serum", 100_000, 1)
	sh.admin.receive(sh.ikeja, "SERUM", 5)
	_, a := sh.admin.order("a-"+uuid.NewString(), sh.ikeja, "cash", "SERUM", 1)
	_, b := sh.admin.order("b-"+uuid.NewString(), sh.ikeja, "cash", "SERUM", 1)
	key := "refund-" + uuid.NewString()
	res, out := sh.admin.postKeyed("/v1/orders/"+a["id"].(string)+"/refund", key, map[string]any{})
	if res.StatusCode != 201 {
		t.Fatalf("refund a %d %v", res.StatusCode, out)
	}
	if res.Header.Get("Idempotent-Replayed") != "" {
		t.Fatal("first response flagged as replay")
	}
	res, out = sh.admin.postKeyed("/v1/orders/"+b["id"].(string)+"/refund", key, map[string]any{})
	if res.StatusCode != 409 || errCode(out) != "idempotency_conflict" {
		t.Fatalf("same key, other order: %d %v", res.StatusCode, out)
	}
	got := sh.admin.do(http.MethodGet, "/v1/orders/"+b["id"].(string), nil, nil)
	var ob map[string]any
	decode(t, got, &ob)
	if ob["status"] != "paid" {
		t.Fatalf("order b must stay paid, got %v", ob["status"])
	}
	res, out = sh.admin.postKeyed("/v1/orders/"+a["id"].(string)+"/refund", key, map[string]any{})
	if res.StatusCode != 201 || res.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay %d replayed=%q %v", res.StatusCode, res.Header.Get("Idempotent-Replayed"), out)
	}

	// Wallet top-ups to two customers with one key: the second must not look like a replay.
	c1 := sh.admin.customer("One", "0801"+uuid.NewString()[:7])
	c2 := sh.admin.customer("Two", "0802"+uuid.NewString()[:7])
	k2 := "topup-" + uuid.NewString()
	body := map[string]any{"outlet_id": sh.ikeja, "amount_minor": 50_000}
	res, out = sh.admin.postKeyed("/v1/customers/"+c1+"/topups", k2, body)
	if res.StatusCode != 201 {
		t.Fatalf("topup 1 %d %v", res.StatusCode, out)
	}
	res, out = sh.admin.postKeyed("/v1/customers/"+c2+"/topups", k2, body)
	if res.StatusCode != 409 || errCode(out) != "idempotency_conflict" {
		t.Fatalf("topup 2 %d %v", res.StatusCode, out)
	}
	if sh.admin.customerBalance(c2) != 0 {
		t.Fatal("second customer was credited by a stale replay")
	}
}

func TestReservedIdempotencyPrefixesRejected(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("SERUM", "Serum", 100_000, 1)
	sh.admin.receive(sh.ikeja, "SERUM", 5)
	for _, k := range []string{"wh:9100", "WH:1", "sys:x", "Sys:unmatched"} {
		res, out := sh.admin.order(k, sh.ikeja, "cash", "SERUM", 1)
		if res.StatusCode != 400 || errCode(out) != "idempotency_key_reserved" {
			t.Fatalf("key %q: %d %v", k, res.StatusCode, out)
		}
	}
}

func TestPurgeOldIdempotencyKeys(t *testing.T) {
	sh := openShop(t, 0)
	ctx := context.Background()
	old, fresh := "old-"+uuid.NewString(), "fresh-"+uuid.NewString()
	for _, k := range []struct {
		key string
		age string
	}{{old, "31 days"}, {fresh, "1 day"}} {
		if _, err := testPool.Exec(ctx, `
			INSERT INTO idempotency_keys (merchant_id, key, request_hash, status, created_at)
			VALUES ($1, $2, '\x00', 'completed', now() - $3::interval)`, sh.merchant, k.key, k.age); err != nil {
			t.Fatal(err)
		}
	}
	n, err := store.New(testPool).PurgeIdempotency(ctx, 30*24*time.Hour)
	if err != nil || n < 1 {
		t.Fatalf("purge %d %v", n, err)
	}
	var left int
	if err := testPool.QueryRow(ctx, `SELECT COUNT(*) FROM idempotency_keys WHERE merchant_id = $1 AND key = ANY($2)`, sh.merchant, []string{old, fresh}).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 1 {
		t.Fatalf("expected only the fresh key to remain, %d left", left)
	}
}

// ---- 2. provider events ----

func TestWebhookDedupeUsesProviderEventsAndPopulatesPayment(t *testing.T) {
	sh, ord := transferShop(t, 5, 1)
	// A cashier cannot pre-claim the webhook's namespace.
	res, out := sh.admin.order("wh:paystack:77", sh.ikeja, "cash", "SERUM", 1)
	if res.StatusCode != 400 {
		t.Fatalf("squat %d %v", res.StatusCode, out)
	}
	ev := nextEvent()
	ref := ord["reference"].(string)
	res, paid := sh.admin.webhook(ev, ref, 100_000, "NGN")
	if res.StatusCode != 200 || paid["status"] != "paid" {
		t.Fatalf("webhook %d %v", res.StatusCode, paid)
	}
	var pe string
	if err := testPool.QueryRow(context.Background(), `SELECT provider_event_id FROM payments WHERE order_id = $1`, ord["id"]).Scan(&pe); err != nil || pe != fmt.Sprint(ev) {
		t.Fatalf("payments.provider_event_id = %q (%v), want %d", pe, err, ev)
	}
	var cnt int
	if err := testPool.QueryRow(context.Background(), `SELECT COUNT(*) FROM provider_events WHERE provider='paystack' AND event_id = $1 AND merchant_id = $2`, fmt.Sprint(ev), sh.merchant).Scan(&cnt); err != nil || cnt != 1 {
		t.Fatalf("provider_events rows %d %v", cnt, err)
	}
	if err := testPool.QueryRow(context.Background(), `SELECT COUNT(*) FROM idempotency_keys WHERE key LIKE 'wh:%' AND merchant_id = $1`, sh.merchant).Scan(&cnt); err != nil || cnt != 0 {
		t.Fatalf("webhook must not use idempotency_keys: %d %v", cnt, err)
	}
	before := sh.admin.identity()["journals"].(float64)
	res, again := sh.admin.webhook(ev, ref, 100_000, "NGN")
	if res.StatusCode != 200 || again["id"] != paid["id"] || res.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay %d %v", res.StatusCode, again)
	}
	if sh.admin.identity()["journals"].(float64) != before {
		t.Fatal("replay posted a journal")
	}
	sh.mustHold()
}

func TestConcurrentDuplicateWebhookPostsOnce(t *testing.T) {
	sh, ord := transferShop(t, 5, 1)
	ev := nextEvent()
	ref := ord["reference"].(string)
	var wg sync.WaitGroup
	statuses := make([]int, 6)
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, _ := sh.admin.webhook(ev, ref, 100_000, "NGN")
			statuses[i] = res.StatusCode
		}(i)
	}
	wg.Wait()
	for _, s := range statuses {
		if s != 200 {
			t.Fatalf("statuses %v", statuses)
		}
	}
	if got := sh.bal("1100"); got != 100_000 {
		t.Fatalf("clearing %d", got)
	}
	if n := len(sh.admin.listUnmatched(sh.ikeja, "")); n != 0 {
		t.Fatalf("duplicates created %d unmatched rows", n)
	}
	sh.mustHold()
}

func TestUnknownReferenceIsRecordedWithoutJournal(t *testing.T) {
	sh := openShop(t, 0)
	ev := nextEvent()
	res, out := sh.admin.webhook(ev, "NOPE-"+uuid.NewString()[:6], 5_000, "NGN")
	if res.StatusCode != 200 || out["reason"] != "unknown_reference" {
		t.Fatalf("%d %v", res.StatusCode, out)
	}
	var outcome string
	var merchant *string
	if err := testPool.QueryRow(context.Background(), `SELECT outcome, merchant_id::text FROM provider_events WHERE provider='paystack' AND event_id=$1`, fmt.Sprint(ev)).Scan(&outcome, &merchant); err != nil {
		t.Fatal(err)
	}
	if outcome != "unknown_reference" || merchant != nil {
		t.Fatalf("outcome %q merchant %v", outcome, merchant)
	}
}

// ---- 3. late / mismatched money ----

func TestLatePaymentGoesToSuspenseAndCanBeApplied(t *testing.T) {
	sh, ord := transferShop(t, 3, 1)
	id, ref := ord["id"].(string), ord["reference"].(string)
	sh.expireOrder(id)
	ev := nextEvent()
	res, out := sh.admin.webhook(ev, ref, 100_000, "NGN")
	if res.StatusCode != 200 || out["status"] != "unmatched" || out["reason"] != "order_expired" {
		t.Fatalf("late webhook %d %v", res.StatusCode, out)
	}
	if sh.bal("2300") != 100_000 || sh.bal("1100") != 100_000 {
		t.Fatalf("suspense %d clearing %d", sh.bal("2300"), sh.bal("1100"))
	}
	sh.mustHold()
	open := sh.admin.listUnmatched(sh.ikeja, "open")
	if len(open) != 1 || open[0]["reason"] != "order_expired" || open[0]["amount_minor"].(float64) != 100_000 {
		t.Fatalf("open %v", open)
	}
	uid := open[0]["id"].(string)

	// Cashiers cannot resolve.
	cEmail := fmt.Sprintf("c-%s@test.local", uuid.NewString()[:8])
	sh.admin.addStaff(cEmail, "cashier", sh.ikeja)
	cashier := loginEmail(t, cEmail)
	r2, _ := cashier.postKeyed("/v1/unmatched-payments/"+uid+"/apply", "x-"+uuid.NewString(), map[string]any{})
	if r2.StatusCode != 403 {
		t.Fatalf("cashier apply %d", r2.StatusCode)
	}

	res, out = sh.admin.postKeyed("/v1/unmatched-payments/"+uid+"/apply", "ap-"+uuid.NewString(), map[string]any{})
	if res.StatusCode != 200 || out["status"] != "applied" {
		t.Fatalf("apply %d %v", res.StatusCode, out)
	}
	if sh.bal("2300") != 0 || sh.bal("1100") != 100_000 {
		t.Fatalf("after apply suspense %d clearing %d", sh.bal("2300"), sh.bal("1100"))
	}
	if sh.admin.stockAvailable(sh.ikeja, "SERUM") != 2 {
		t.Fatalf("stock %d", sh.admin.stockAvailable(sh.ikeja, "SERUM"))
	}
	got := sh.admin.do(http.MethodGet, "/v1/orders/"+id, nil, nil)
	var ob map[string]any
	decode(t, got, &ob)
	if ob["status"] != "paid" {
		t.Fatalf("order %v", ob["status"])
	}
	sh.mustHold()
	res, out = sh.admin.postKeyed("/v1/unmatched-payments/"+uid+"/apply", "ap2-"+uuid.NewString(), map[string]any{})
	if res.StatusCode != 409 || errCode(out) != "unmatched_not_open" {
		t.Fatalf("second apply %d %v", res.StatusCode, out)
	}
}

func TestApplyFailsWhenStockGoneOrAmountDiffers(t *testing.T) {
	sh, ord := transferShop(t, 1, 1)
	sh.expireOrder(ord["id"].(string))
	res, out := sh.admin.webhook(nextEvent(), ord["reference"].(string), 100_000, "NGN")
	if res.StatusCode != 200 || out["status"] != "unmatched" {
		t.Fatalf("%d %v", res.StatusCode, out)
	}
	// Someone else buys the last unit.
	if r, o := sh.admin.order("buy-"+uuid.NewString(), sh.ikeja, "cash", "SERUM", 1); r.StatusCode != 201 {
		t.Fatalf("sale %d %v", r.StatusCode, o)
	}
	uid := sh.admin.listUnmatched(sh.ikeja, "open")[0]["id"].(string)
	res, out = sh.admin.postKeyed("/v1/unmatched-payments/"+uid+"/apply", "ap-"+uuid.NewString(), map[string]any{})
	if res.StatusCode != 409 || errCode(out) != "insufficient_stock" {
		t.Fatalf("apply without stock %d %v", res.StatusCode, out)
	}
	if sh.bal("2300") != 100_000 {
		t.Fatal("failed apply moved suspense")
	}

	// A different amount can never be applied.
	sh2, ord2 := transferShop(t, 3, 1)
	sh2.expireOrder(ord2["id"].(string))
	sh2.admin.webhook(nextEvent(), ord2["reference"].(string), 90_000, "NGN")
	uid2 := sh2.admin.listUnmatched(sh2.ikeja, "open")[0]["id"].(string)
	res, out = sh2.admin.postKeyed("/v1/unmatched-payments/"+uid2+"/apply", "ap-"+uuid.NewString(), map[string]any{})
	if res.StatusCode != 409 || errCode(out) != "unmatched_not_applicable" {
		t.Fatalf("apply wrong amount %d %v", res.StatusCode, out)
	}
	sh.mustHold()
	sh2.mustHold()
}

func TestMismatchAndDuplicatePaymentsRefundThroughSuspense(t *testing.T) {
	sh, ord := transferShop(t, 5, 1)
	ref := ord["reference"].(string)
	// Under-payment on a pending order.
	res, out := sh.admin.webhook(nextEvent(), ref, 60_000, "NGN")
	if res.StatusCode != 200 || out["reason"] != "amount_mismatch" {
		t.Fatalf("underpay %d %v", res.StatusCode, out)
	}
	// Wrong currency, right number.
	res, out = sh.admin.webhook(nextEvent(), ref, 100_000, "USD")
	if res.StatusCode != 200 || out["reason"] != "currency_mismatch" {
		t.Fatalf("currency %d %v", res.StatusCode, out)
	}
	// The order is still payable; pay it, then pay it again.
	res, out = sh.admin.webhook(nextEvent(), ref, 100_000, "NGN")
	if out["status"] != "paid" {
		t.Fatalf("pay %d %v", res.StatusCode, out)
	}
	res, out = sh.admin.webhook(nextEvent(), ref, 100_000, "NGN")
	if out["reason"] != "order_already_paid" {
		t.Fatalf("double pay %d %v", res.StatusCode, out)
	}
	if sh.bal("2300") != 60_000+100_000+100_000 {
		t.Fatalf("suspense %d", sh.bal("2300"))
	}
	sh.mustHold()
	open := sh.admin.listUnmatched(sh.ikeja, "open")
	if len(open) != 3 {
		t.Fatalf("open %d", len(open))
	}
	for _, u := range open {
		uid := u["id"].(string)
		res, out := sh.admin.postKeyed("/v1/unmatched-payments/"+uid+"/refund", "rf-"+uuid.NewString(), map[string]any{})
		if res.StatusCode != 200 || out["status"] != "refund_pending" {
			t.Fatalf("refund %d %v", res.StatusCode, out)
		}
	}
	if sh.bal("2300") != 0 || sh.bal("2310") != 260_000 {
		t.Fatalf("2300 %d 2310 %d", sh.bal("2300"), sh.bal("2310"))
	}
	sh.mustHold()
	// Settle one; the settle step needs refund_pending.
	pend := sh.admin.listUnmatched(sh.ikeja, "refund_pending")
	uid := pend[0]["id"].(string)
	res, out = sh.admin.postKeyed("/v1/unmatched-payments/"+uid+"/refunded", "rd-"+uuid.NewString(), map[string]any{})
	if res.StatusCode != 200 || out["status"] != "refunded" {
		t.Fatalf("refunded %d %v", res.StatusCode, out)
	}
	res, out = sh.admin.postKeyed("/v1/unmatched-payments/"+uid+"/refunded", "rd2-"+uuid.NewString(), map[string]any{})
	if res.StatusCode != 409 {
		t.Fatalf("double settle %d %v", res.StatusCode, out)
	}
	sh.mustHold()
}

// ---- 4. keys required ----

func TestStockAndProductChangesRequireIdempotencyKey(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("SERUM", "Serum", 100_000, 1)
	noKey := http.Header{}
	noKey.Set("X-Test-No-Key", "1")
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/v1/stock/receipts"},
		{http.MethodPost, "/v1/stock/adjustments"},
	} {
		res := sh.admin.do(tc.method, tc.path, map[string]any{"outlet_id": sh.ikeja, "sku": "SERUM", "qty": 1, "delta": 1, "reason": "x"}, noKey)
		var out map[string]any
		decode(t, res, &out)
		if res.StatusCode != 400 || errCode(out) != "idempotency_key_required" {
			t.Fatalf("%s: %d %v", tc.path, res.StatusCode, out)
		}
	}
	id := productID(t, sh.admin, "SERUM")
	res := sh.admin.do(http.MethodPatch, "/v1/products/"+id, map[string]any{"name": "Serum 2"}, noKey)
	var out map[string]any
	decode(t, res, &out)
	if res.StatusCode != 400 || errCode(out) != "idempotency_key_required" {
		t.Fatalf("patch: %d %v", res.StatusCode, out)
	}

	// A retried receipt does not double stock.
	k := keyHeader("rcpt-" + uuid.NewString())
	for i := 0; i < 2; i++ {
		res := sh.admin.do(http.MethodPost, "/v1/stock/receipts", map[string]any{"outlet_id": sh.ikeja, "sku": "SERUM", "qty": 4, "unit_cost_minor": 30_000}, k)
		decode(t, res, nil)
		if res.StatusCode != 200 {
			t.Fatalf("receipt %d", res.StatusCode)
		}
		if i == 1 && res.Header.Get("Idempotent-Replayed") != "true" {
			t.Fatal("missing replay header")
		}
	}
	if sh.admin.stockAvailable(sh.ikeja, "SERUM") != 4 || sh.bal("1200") != 120_000 {
		t.Fatalf("stock %d inventory %d", sh.admin.stockAvailable(sh.ikeja, "SERUM"), sh.bal("1200"))
	}
	sh.mustHold()
}

func productID(t *testing.T, c *apiClient, sku string) string {
	t.Helper()
	res := c.do(http.MethodGet, "/v1/products", nil, nil)
	var out struct {
		Products []struct {
			ID  string `json:"id"`
			SKU string `json:"sku"`
		} `json:"products"`
	}
	decode(t, res, &out)
	for _, p := range out.Products {
		if p.SKU == sku {
			return p.ID
		}
	}
	t.Fatalf("product %s missing", sku)
	return ""
}

// ---- 5. control accounts ----

func TestControlAccountsRefuseFreeFormPostings(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("SERUM", "Serum", 100_000, 1)
	sh.admin.receive(sh.ikeja, "SERUM", 2)
	cust := sh.admin.customer("Ada", "0803"+uuid.NewString()[:7])
	if r, o := sh.admin.postKeyed("/v1/customers/"+cust+"/topups", "tu-"+uuid.NewString(), map[string]any{"outlet_id": sh.ikeja, "amount_minor": 20_000}); r.StatusCode != 201 {
		t.Fatalf("topup %d %v", r.StatusCode, o)
	}
	cash, _ := sh.admin.accountByCode("1000-IKJ")
	for _, code := range []string{"1100", "1200", "2100", "2150", "2200", "2300", "2310", "5000"} {
		id, _ := sh.admin.accountByCode(code)
		res, out := sh.admin.postJournal("j-"+uuid.NewString(), map[string]any{
			"description": "sneaky",
			"postings": []map[string]any{
				{"account_id": cash, "direction": "debit", "amount_minor": 100},
				{"account_id": id, "direction": "credit", "amount_minor": 100},
			},
		})
		if res.StatusCode != 409 || errCode(out) != "control_account" {
			t.Fatalf("journal via %s: %d %v", code, res.StatusCode, out)
		}
	}
	wallet, _ := sh.admin.accountByCode("2200")
	res, out := sh.admin.postTransfer("t-"+uuid.NewString(), wallet, cash, 100, "drain wallet")
	if res.StatusCode != 409 || errCode(out) != "control_account" {
		t.Fatalf("transfer %d %v", res.StatusCode, out)
	}
	// Raw reversal of the top-up journal would skip the customer balance.
	var jid string
	if err := testPool.QueryRow(context.Background(), `SELECT id::text FROM journals WHERE merchant_id=$1 AND description LIKE 'wallet top-up%'`, sh.merchant).Scan(&jid); err != nil {
		t.Fatal(err)
	}
	res, out = sh.admin.postKeyed("/v1/journals/"+jid+"/reversal", "rv-"+uuid.NewString(), map[string]any{})
	if res.StatusCode != 409 || errCode(out) != "control_account" {
		t.Fatalf("reversal %d %v", res.StatusCode, out)
	}
	// Non-control accounts still work.
	a := sh.admin.createAccount("6100", "Rent", "expense")
	res, out = sh.admin.postJournal("ok-"+uuid.NewString(), map[string]any{
		"postings": []map[string]any{
			{"account_id": a, "direction": "debit", "amount_minor": 100},
			{"account_id": cash, "direction": "credit", "amount_minor": 100},
		},
	})
	if res.StatusCode != 201 {
		t.Fatalf("plain journal %d %v", res.StatusCode, out)
	}
	sh.mustHold()
}

// ---- 6. invariants ----

func TestInvariantsReportEachCheck(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("SERUM", "Serum", 100_000, 1)
	sh.admin.receive(sh.ikeja, "SERUM", 2)
	cust := sh.admin.customer("Ada", "0804"+uuid.NewString()[:7])
	sh.admin.postKeyed("/v1/customers/"+cust+"/topups", "tu-"+uuid.NewString(), map[string]any{"outlet_id": sh.ikeja, "amount_minor": 20_000})
	inv := sh.admin.identity()
	checks, _ := inv["checks"].([]any)
	names := map[string]map[string]any{}
	for _, c := range checks {
		m := c.(map[string]any)
		names[m["name"].(string)] = m
	}
	for _, n := range []string{"trial_balance_identity", "balance_cache_matches_journal", "wallet_matches_customer_balances",
		"suspense_matches_open_unmatched", "refunds_payable_matches_pending_refunds", "inventory_matches_stock_value"} {
		c, ok := names[n]
		if !ok || c["holds"] != true {
			t.Fatalf("check %s: %v", n, c)
		}
	}
	if names["wallet_matches_customer_balances"]["expected"].(float64) != 20_000 {
		t.Fatalf("wallet check %v", names["wallet_matches_customer_balances"])
	}
	// A wallet balance edited behind the ledger's back must be caught.
	if _, err := testPool.Exec(context.Background(), `UPDATE customers SET balance_minor = balance_minor + 7 WHERE id = $1`, cust); err != nil {
		t.Fatal(err)
	}
	inv = sh.admin.identity()
	if inv["holds"] != false {
		t.Fatalf("drift not detected: %v", inv)
	}
	for _, c := range inv["checks"].([]any) {
		m := c.(map[string]any)
		if m["name"] == "wallet_matches_customer_balances" && (m["holds"] != false || m["expected"].(float64) != 20_007 || m["actual"].(float64) != 20_000) {
			t.Fatalf("wallet check %v", m)
		}
	}
}

// ---- 7. database guards ----

func TestDatabaseRejectsUnbalancedAndLateInsertedPostings(t *testing.T) {
	sh := openShop(t, 0)
	cash, _ := sh.admin.accountByCode("1000-IKJ")
	sales, _ := sh.admin.accountByCode("4000-IKJ")
	ctx := context.Background()
	insJournal := func(tx pgx.Tx) uuid.UUID {
		id := uuid.Must(uuid.NewV7())
		if _, err := tx.Exec(ctx, `INSERT INTO journals (id, merchant_id, status, description) VALUES ($1,$2,'posted','raw')`, id, sh.merchant); err != nil {
			t.Fatal(err)
		}
		return id
	}
	insPosting := func(tx pgx.Tx, j uuid.UUID, acct, dir string, amt int64) error {
		_, err := tx.Exec(ctx, `INSERT INTO postings (id, journal_id, account_id, direction, amount_minor) VALUES ($1,$2,$3,$4,$5)`,
			uuid.Must(uuid.NewV7()), j, acct, dir, amt)
		return err
	}
	// Unbalanced journal fails at commit.
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	j := insJournal(tx)
	if err := insPosting(tx, j, cash, "debit", 100); err != nil {
		t.Fatal(err)
	}
	if err := insPosting(tx, j, sales, "credit", 90); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err == nil || !strings.Contains(err.Error(), "unbalanced") {
		t.Fatalf("commit of unbalanced journal: %v", err)
	}
	// A balanced one commits, and then no posting can be added to it.
	tx, err = testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	j = insJournal(tx)
	if err := insPosting(tx, j, cash, "debit", 100); err != nil {
		t.Fatal(err)
	}
	if err := insPosting(tx, j, sales, "credit", 100); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx, err = testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := insPosting(tx, j, cash, "credit", 5); err == nil || !strings.Contains(err.Error(), "transaction that created") {
		t.Fatalf("late posting accepted: %v", err)
	}
}

// ---- 7/9. refund columns and refund errors ----

func TestRefundRecordsLinkAndReason(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("SERUM", "Serum", 100_000, 1)
	sh.admin.receive(sh.ikeja, "SERUM", 3)
	_, sold := sh.admin.order("s-"+uuid.NewString(), sh.ikeja, "cash", "SERUM", 1)
	res, out := sh.admin.postKeyed("/v1/orders/"+sold["id"].(string)+"/refund", "r-"+uuid.NewString(), map[string]any{"reason": "wrong size"})
	if res.StatusCode != 201 || out["refund_reason"] != "wrong size" || out["refund_journal_id"] == nil || out["refunded_at"] == nil {
		t.Fatalf("refund %d %v", res.StatusCode, out)
	}
	var jid, by, reason string
	var at *time.Time
	if err := testPool.QueryRow(context.Background(), `SELECT refund_journal_id::text, refunded_by::text, refund_reason, refunded_at FROM orders WHERE id = $1`, sold["id"]).Scan(&jid, &by, &reason, &at); err != nil || at == nil || reason != "wrong size" || jid != out["refund_journal_id"] {
		t.Fatalf("columns %q %q %q %v %v", jid, by, reason, at, err)
	}
	sh.mustHold()
}

func TestRefundInsufficientFundsHasOwnCode(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("SERUM", "Serum", 100_000, 1)
	sh.admin.receive(sh.ikeja, "SERUM", 3)
	_, sold := sh.admin.order("s-"+uuid.NewString(), sh.ikeja, "cash", "SERUM", 1)
	cash, bal := sh.admin.accountByCode("1000-IKJ")
	rent := sh.admin.createAccount("6100", "Rent", "expense")
	res, out := sh.admin.postJournal("drain-"+uuid.NewString(), map[string]any{
		"postings": []map[string]any{
			{"account_id": rent, "direction": "debit", "amount_minor": bal},
			{"account_id": cash, "direction": "credit", "amount_minor": bal},
		},
	})
	if res.StatusCode != 201 {
		t.Fatalf("drain %d %v", res.StatusCode, out)
	}
	res, out = sh.admin.postKeyed("/v1/orders/"+sold["id"].(string)+"/refund", "r-"+uuid.NewString(), map[string]any{})
	if res.StatusCode != 409 || errCode(out) != "refund_insufficient_funds" {
		t.Fatalf("refund %d %v", res.StatusCode, out)
	}
	if !strings.Contains(fmt.Sprint(out), "1000-IKJ") {
		t.Fatalf("message should name the account: %v", out)
	}
	if sh.admin.stockAvailable(sh.ikeja, "SERUM") != 2 {
		t.Fatal("failed refund restocked")
	}
}

func TestTransferRefundCreditsRefundsPayableUntilSettled(t *testing.T) {
	sh, ord := transferShop(t, 3, 1)
	if res, out := sh.admin.webhook(nextEvent(), ord["reference"].(string), 100_000, "NGN"); out["status"] != "paid" {
		t.Fatalf("pay %d %v", res.StatusCode, out)
	}
	res, out := sh.admin.postKeyed("/v1/orders/"+ord["id"].(string)+"/refund", "r-"+uuid.NewString(), map[string]any{})
	if res.StatusCode != 201 || out["refund_record_id"] == nil {
		t.Fatalf("refund %d %v", res.StatusCode, out)
	}
	if sh.bal("2310") != 100_000 || sh.bal("1100") != 100_000 {
		t.Fatalf("2310 %d 1100 %d", sh.bal("2310"), sh.bal("1100"))
	}
	sh.mustHold()
	pend := sh.admin.listUnmatched(sh.ikeja, "refund_pending")
	if len(pend) != 1 || pend[0]["reason"] != "order_refund" || pend[0]["id"] != out["refund_record_id"] {
		t.Fatalf("pending %v", pend)
	}
	res, o2 := sh.admin.postKeyed("/v1/unmatched-payments/"+pend[0]["id"].(string)+"/refunded", "rd-"+uuid.NewString(), map[string]any{})
	if res.StatusCode != 200 || o2["status"] != "refunded" {
		t.Fatalf("settle %d %v", res.StatusCode, o2)
	}
	if sh.bal("2310") != 0 || sh.bal("1100") != 0 {
		t.Fatalf("after settle 2310 %d 1100 %d", sh.bal("2310"), sh.bal("1100"))
	}
	sh.mustHold()
}

// ---- 8. locks ----

func TestSalePricedFromLockedProductRow(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("SERUM", "Serum", 100_000, 1)
	sh.admin.receive(sh.ikeja, "SERUM", 3)
	ctx := context.Background()
	pid := productID(t, sh.admin, "SERUM")
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM products WHERE id = $1 FOR UPDATE`, pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan map[string]any, 1)
	go func() {
		_, out := sh.admin.order("lk-"+uuid.NewString(), sh.ikeja, "cash", "SERUM", 1)
		done <- out
	}()
	time.Sleep(700 * time.Millisecond) // the order has read the old price and now waits on the lock
	if _, err := tx.Exec(ctx, `UPDATE products SET price_minor = 150000 WHERE id = $1`, pid); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-done:
		if out["total_minor"] != float64(150_000) {
			t.Fatalf("sale used a stale price: %v", out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("sale did not finish")
	}
	sh.mustHold()
}

func TestProductPatchCannotRevertConcurrentReceiptCost(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("SERUM", "Serum", 100_000, 1)
	pid := productID(t, sh.admin, "SERUM")
	for i := 0; i < 15; i++ {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			res := sh.admin.do(http.MethodPatch, "/v1/products/"+pid, map[string]any{"name": fmt.Sprintf("Serum %d", i)}, nil)
			decode(t, res, nil)
		}()
		go func() {
			defer wg.Done()
			res := sh.admin.do(http.MethodPost, "/v1/stock/receipts", map[string]any{"outlet_id": sh.ikeja, "sku": "SERUM", "qty": 1, "unit_cost_minor": 10_000}, nil)
			decode(t, res, nil)
		}()
		wg.Wait()
	}
	var avg int64
	if err := testPool.QueryRow(context.Background(), `SELECT avg_cost_minor FROM products WHERE id = $1`, pid).Scan(&avg); err != nil {
		t.Fatal(err)
	}
	if avg != 10_000 {
		t.Fatalf("avg cost %d: a patch reverted a receipt", avg)
	}
	sh.mustHold()
}

// ---- 10. sweeper ----

func TestSweeperSkipsPoisonOrderAndKeepsGoing(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("AAA", "A", 100_000, 0)
	sh.admin.createProduct("BBB", "B", 100_000, 0)
	sh.admin.receive(sh.ikeja, "AAA", 3)
	sh.admin.receive(sh.ikeja, "BBB", 3)
	_, bad := sh.admin.order("pa-"+uuid.NewString(), sh.ikeja, "transfer", "AAA", 1)
	_, good := sh.admin.order("pb-"+uuid.NewString(), sh.ikeja, "transfer", "BBB", 1)
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `UPDATE orders SET expires_at = now() - interval '1 hour' WHERE id = ANY($1)`, []string{bad["id"].(string), good["id"].(string)}); err != nil {
		t.Fatal(err)
	}
	// Corrupt the first order's hold so releasing it fails.
	if _, err := testPool.Exec(ctx, `UPDATE stock_levels SET reserved = 0 WHERE outlet_id = $1 AND product_id = $2`, sh.ikeja, productID(t, sh.admin, "AAA")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Leave no poison order behind for the tests that sweep the shared database.
		_, _ = testPool.Exec(ctx, `UPDATE stock_levels SET reserved = 1 WHERE outlet_id = $1 AND product_id = $2`, sh.ikeja, productID(t, sh.admin, "AAA"))
		checkout.SweepExpired(ctx, store.New(testPool), nil, time.Now(), nil)
	})
	var logs bytes.Buffer
	failed := map[uuid.UUID]struct{}{}
	n := checkout.SweepExpired(ctx, store.New(testPool), slog.New(slog.NewTextHandler(&logs, nil)), time.Now(), failed)
	if n < 1 {
		t.Fatalf("released %d", n)
	}
	status := func(id string) string {
		var s string
		if err := testPool.QueryRow(ctx, `SELECT status FROM orders WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if status(good["id"].(string)) != "expired" {
		t.Fatal("good order was starved by the poison one")
	}
	if status(bad["id"].(string)) != "pending_payment" {
		t.Fatal("poison order should be left alone")
	}
	if !strings.Contains(logs.String(), bad["id"].(string)) {
		t.Fatalf("failure not logged: %s", logs.String())
	}
	if _, ok := failed[uuid.MustParse(bad["id"].(string))]; !ok {
		t.Fatal("failed order not remembered for this tick")
	}
}

// ---- 11. migrations ----

func TestConcurrentMigrateIsSerialised(t *testing.T) {
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- store.Migrate(context.Background(), testPool)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}
