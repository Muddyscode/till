package integration

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Muddyscode/till/internal/auth"
	"github.com/Muddyscode/till/internal/checkout"
	"github.com/Muddyscode/till/internal/httpserver"
	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type shop struct {
	t        *testing.T
	admin    *apiClient
	ikeja    string
	sul      string
	merchant string
}

func openShop(t *testing.T, vat int) *shop {
	t.Helper()
	st := store.New(testPool)
	ctx := context.Background()
	mid := uuid.Must(uuid.NewV7())
	sid := uuid.Must(uuid.NewV7())
	email := fmt.Sprintf("admin-%s@test.local", uuid.NewString()[:8])
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := st.CreateMerchant(ctx, tx, mid, "Shop", vat); err != nil {
			return err
		}
		_, err := st.CreateStaff(ctx, tx, store.Staff{
			ID: sid, MerchantID: mid, Email: email, PasswordHash: cachedHash(t), Role: auth.RoleAdmin,
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	c := startAPI(t)
	res := c.do(http.MethodPost, "/v1/auth/login", map[string]string{"email": email, "password": "change-me-now-12"}, nil)
	var out struct {
		Token string `json:"token"`
	}
	decode(t, res, &out)
	if res.StatusCode != 200 {
		t.Fatalf("login %d", res.StatusCode)
	}
	c.token = out.Token
	ikeja := c.createOutlet("Ikeja", "IKJ")
	sul := c.createOutlet("Surulere", "SUL")
	return &shop{t: t, admin: c, ikeja: ikeja, sul: sul, merchant: mid.String()}
}

func (c *apiClient) createOutlet(name, code string) string {
	c.t.Helper()
	res := c.do(http.MethodPost, "/v1/outlets", map[string]any{"name": name, "code": code}, nil)
	var out struct {
		ID string `json:"id"`
	}
	if res.StatusCode != http.StatusCreated {
		var body map[string]any
		decode(c.t, res, &body)
		c.t.Fatalf("outlet %s: %d %v", code, res.StatusCode, body)
	}
	decode(c.t, res, &out)
	return out.ID
}

func (c *apiClient) createProduct(sku, name string, price int64, reorder int) {
	c.t.Helper()
	res := c.do(http.MethodPost, "/v1/products", map[string]any{
		"sku": sku, "name": name, "price_minor": price, "reorder_level": reorder,
	}, nil)
	if res.StatusCode != http.StatusCreated {
		var out map[string]any
		decode(c.t, res, &out)
		c.t.Fatalf("product %s: %d %v", sku, res.StatusCode, out)
	}
	decode(c.t, res, nil)
}

func (c *apiClient) receive(outlet, sku string, qty int) {
	c.t.Helper()
	res := c.do(http.MethodPost, "/v1/stock/receipts", map[string]any{
		"outlet_id": outlet, "sku": sku, "qty": qty,
	}, nil)
	if res.StatusCode != http.StatusOK {
		var out map[string]any
		decode(c.t, res, &out)
		c.t.Fatalf("receive %d %v", res.StatusCode, out)
	}
	decode(c.t, res, nil)
}

func (c *apiClient) order(key, outlet, method, sku string, qty int) (*http.Response, map[string]any) {
	c.t.Helper()
	h := http.Header{}
	h.Set("Idempotency-Key", key)
	res := c.do(http.MethodPost, "/v1/orders", map[string]any{
		"outlet_id": outlet, "payment_method": method,
		"lines": []map[string]any{{"sku": sku, "qty": qty}},
	}, h)
	var out map[string]any
	decode(c.t, res, &out)
	return res, out
}

func (c *apiClient) stockAvailable(outlet, sku string) int {
	c.t.Helper()
	res := c.do(http.MethodGet, "/v1/reports/stock-on-hand?outlet_id="+outlet, nil, nil)
	var out struct {
		Stock []struct {
			SKU       string `json:"sku"`
			OnHand    int    `json:"on_hand"`
			Reserved  int    `json:"reserved"`
			Available int    `json:"available"`
		} `json:"stock"`
	}
	decode(c.t, res, &out)
	if res.StatusCode != 200 {
		c.t.Fatalf("stock %d", res.StatusCode)
	}
	for _, row := range out.Stock {
		if row.SKU == sku {
			return row.Available
		}
	}
	c.t.Fatalf("sku %s missing", sku)
	return 0
}

func (c *apiClient) addStaff(email, role, outlet string) {
	c.t.Helper()
	body := map[string]any{"email": email, "password": "change-me-now-12", "role": role}
	if outlet != "" {
		body["outlet_ids"] = []string{outlet}
	}
	res := c.do(http.MethodPost, "/v1/staff", body, nil)
	if res.StatusCode != http.StatusCreated {
		var out map[string]any
		decode(c.t, res, &out)
		c.t.Fatalf("staff %s: %d %v", email, res.StatusCode, out)
	}
	decode(c.t, res, nil)
}

func loginEmail(t *testing.T, email string) *apiClient {
	t.Helper()
	c := startAPI(t)
	res := c.do(http.MethodPost, "/v1/auth/login", map[string]string{"email": email, "password": "change-me-now-12"}, nil)
	var out struct {
		Token string `json:"token"`
	}
	decode(t, res, &out)
	if res.StatusCode != 200 {
		t.Fatalf("login %s: %d", email, res.StatusCode)
	}
	c.token = out.Token
	return c
}

func TestCashSaleDeductsStockAndBalances(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("SERUM", "Serum", 450_000, 2)
	sh.admin.receive(sh.ikeja, "SERUM", 5)
	res, out := sh.admin.order("cash-"+uuid.NewString(), sh.ikeja, "cash", "SERUM", 2)
	if res.StatusCode != 201 {
		t.Fatalf("sale %d %v", res.StatusCode, out)
	}
	if out["total_minor"].(float64) != 900_000 || out["vat_minor"].(float64) != 0 {
		t.Fatalf("totals %v", out)
	}
	if sh.admin.stockAvailable(sh.ikeja, "SERUM") != 3 {
		t.Fatalf("stock %d", sh.admin.stockAvailable(sh.ikeja, "SERUM"))
	}
	if sh.admin.identity()["holds"] != true {
		t.Fatal("identity broken")
	}
}

func TestCashSaleVATMatchesIntegerFormula(t *testing.T) {
	sh := openShop(t, 750)
	sh.admin.createProduct("SHEA", "Shea", 450_000, 2)
	sh.admin.receive(sh.ikeja, "SHEA", 4)
	res, out := sh.admin.order("vat-"+uuid.NewString(), sh.ikeja, "cash", "SHEA", 1)
	if res.StatusCode != 201 {
		t.Fatalf("sale %d %v", res.StatusCode, out)
	}
	if out["vat_minor"].(float64) != 31395 || out["net_minor"].(float64) != 418605 || out["total_minor"].(float64) != 450000 {
		t.Fatalf("vat split %v", out)
	}
	if sh.admin.identity()["holds"] != true {
		t.Fatal("identity broken")
	}
}

func TestOversellRollsBack(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("SOAP", "Soap", 200_000, 5)
	sh.admin.receive(sh.ikeja, "SOAP", 2)
	before := sh.admin.identity()["journals"].(float64)
	res, out := sh.admin.order("over-"+uuid.NewString(), sh.ikeja, "cash", "SOAP", 3)
	if res.StatusCode != 409 || errCode(out) != "insufficient_stock" {
		t.Fatalf("got %d %v", res.StatusCode, out)
	}
	if sh.admin.stockAvailable(sh.ikeja, "SOAP") != 2 {
		t.Fatal("stock changed")
	}
	if sh.admin.identity()["journals"].(float64) != before {
		t.Fatal("journal was written")
	}
}

func TestOrderIdempotencyDoesNotDoubleDeduct(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("TOWEL", "Towel", 120_000, 2)
	sh.admin.receive(sh.ikeja, "TOWEL", 4)
	key := "idem-" + uuid.NewString()
	res1, out1 := sh.admin.order(key, sh.ikeja, "cash", "TOWEL", 1)
	res2, out2 := sh.admin.order(key, sh.ikeja, "cash", "TOWEL", 1)
	if res1.StatusCode != 201 || res2.StatusCode != 201 {
		t.Fatalf("%d %v / %d %v", res1.StatusCode, out1, res2.StatusCode, out2)
	}
	if out1["id"] != out2["id"] {
		t.Fatal("replay minted a second order")
	}
	if sh.admin.stockAvailable(sh.ikeja, "TOWEL") != 3 {
		t.Fatal("double deduct")
	}
	res3, out3 := sh.admin.order(key, sh.ikeja, "cash", "TOWEL", 2)
	if res3.StatusCode != 409 || errCode(out3) != "idempotency_conflict" {
		t.Fatalf("conflict %d %v", res3.StatusCode, out3)
	}
}

func TestConcurrentLastUnit(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("LAST", "Last", 100_000, 0)
	sh.admin.receive(sh.ikeja, "LAST", 1)
	const n = 30
	var okCount atomic.Int64
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			res, _ := sh.admin.order(fmt.Sprintf("race-%d-%s", i, uuid.NewString()), sh.ikeja, "cash", "LAST", 1)
			if res.StatusCode == 201 {
				okCount.Add(1)
			}
		}()
	}
	wg.Wait()
	if okCount.Load() != 1 {
		t.Fatalf("successes %d", okCount.Load())
	}
	if sh.admin.stockAvailable(sh.ikeja, "LAST") != 0 {
		t.Fatal("stock not zero")
	}
}

func TestTransferReservesUntilWebhook(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("SERUM", "Serum", 100_000, 2)
	sh.admin.receive(sh.ikeja, "SERUM", 5)
	beforeJ := sh.admin.identity()["journals"].(float64)
	res, out := sh.admin.order("hold-"+uuid.NewString(), sh.ikeja, "transfer", "SERUM", 2)
	if res.StatusCode != 201 || out["status"] != "pending_payment" {
		t.Fatalf("reserve %d %v", res.StatusCode, out)
	}
	if _, ok := out["journal_id"]; ok {
		t.Fatal("transfer posted a journal")
	}
	if sh.admin.stockAvailable(sh.ikeja, "SERUM") != 3 {
		t.Fatalf("available %d", sh.admin.stockAvailable(sh.ikeja, "SERUM"))
	}
	onHand := stockField(t, sh.admin, sh.ikeja, "SERUM", "on_hand")
	reserved := stockField(t, sh.admin, sh.ikeja, "SERUM", "reserved")
	if onHand != 5 || reserved != 2 {
		t.Fatalf("on_hand %v reserved %v", onHand, reserved)
	}
	if sh.admin.identity()["journals"].(float64) != beforeJ {
		t.Fatal("journal written before payment")
	}

	body := []byte(fmt.Sprintf(`{"event":"charge.success","data":{"id":9001,"reference":%q,"amount":200000,"currency":"NGN","status":"success"}}`, out["reference"]))
	h := http.Header{}
	h.Set("x-paystack-signature", httpserver.SignPaystack("test-paystack-secret", body))
	paid := sh.admin.doRaw(http.MethodPost, "/v1/webhooks/paystack", string(body), h)
	var paidBody map[string]any
	decode(t, paid, &paidBody)
	if paid.StatusCode != 200 || paidBody["status"] != "paid" {
		t.Fatalf("webhook %d %v", paid.StatusCode, paidBody)
	}
	again := sh.admin.doRaw(http.MethodPost, "/v1/webhooks/paystack", string(body), h)
	var againBody map[string]any
	decode(t, again, &againBody)
	if again.StatusCode != 200 || againBody["id"] != paidBody["id"] {
		t.Fatalf("replay %d %v", again.StatusCode, againBody)
	}
	if stockField(t, sh.admin, sh.ikeja, "SERUM", "on_hand") != 3 || stockField(t, sh.admin, sh.ikeja, "SERUM", "reserved") != 0 {
		t.Fatal("stock not committed")
	}
	if sh.admin.identity()["holds"] != true {
		t.Fatal("identity broken")
	}
}

func TestWebhookRejectsBadSignatureAndWrongAmount(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("SERUM", "Serum", 100_000, 2)
	sh.admin.receive(sh.ikeja, "SERUM", 5)
	res, out := sh.admin.order("bad-"+uuid.NewString(), sh.ikeja, "transfer", "SERUM", 1)
	if res.StatusCode != 201 {
		t.Fatal(out)
	}
	body := []byte(fmt.Sprintf(`{"event":"charge.success","data":{"id":42,"reference":%q,"amount":100000,"currency":"NGN","status":"success"}}`, out["reference"]))
	h := http.Header{}
	h.Set("x-paystack-signature", "deadbeef")
	bad := sh.admin.doRaw(http.MethodPost, "/v1/webhooks/paystack", string(body), h)
	if bad.StatusCode != 401 {
		t.Fatalf("sig %d", bad.StatusCode)
	}
	decode(t, bad, nil)
	if stockField(t, sh.admin, sh.ikeja, "SERUM", "reserved") != 1 {
		t.Fatal("bad signature touched stock")
	}
	wrong := []byte(fmt.Sprintf(`{"event":"charge.success","data":{"id":43,"reference":%q,"amount":50,"currency":"NGN","status":"success"}}`, out["reference"]))
	h2 := http.Header{}
	h2.Set("x-paystack-signature", httpserver.SignPaystack("test-paystack-secret", wrong))
	ig := sh.admin.doRaw(http.MethodPost, "/v1/webhooks/paystack", string(wrong), h2)
	var igBody map[string]any
	decode(t, ig, &igBody)
	if ig.StatusCode != 200 || igBody["reason"] != "amount_mismatch" {
		t.Fatalf("mismatch %d %v", ig.StatusCode, igBody)
	}
	if stockField(t, sh.admin, sh.ikeja, "SERUM", "on_hand") != 5 {
		t.Fatal("mismatch deducted stock")
	}
}

func TestReservationExpiryReleasesStock(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("SERUM", "Serum", 100_000, 2)
	sh.admin.receive(sh.ikeja, "SERUM", 5)
	res, out := sh.admin.order("exp-"+uuid.NewString(), sh.ikeja, "transfer", "SERUM", 2)
	if res.StatusCode != 201 {
		t.Fatal(out)
	}
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `UPDATE orders SET expires_at = now() - interval '1 minute' WHERE id = $1`, out["id"]); err != nil {
		t.Fatal(err)
	}
	st := store.New(testPool)
	var n int
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		n, err = checkout.ReleaseExpired(ctx, tx, st, time.Now())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Fatalf("released %d", n)
	}
	got := sh.admin.do(http.MethodGet, "/v1/orders/"+out["id"].(string), nil, nil)
	var order map[string]any
	decode(t, got, &order)
	if order["status"] != "expired" {
		t.Fatalf("status %v", order)
	}
	if stockField(t, sh.admin, sh.ikeja, "SERUM", "on_hand") != 5 || stockField(t, sh.admin, sh.ikeja, "SERUM", "reserved") != 0 {
		t.Fatal("stock not released")
	}
	if sh.admin.identity()["holds"] != true {
		t.Fatal("identity")
	}
}

func TestRolesAndOutletScope(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("SERUM", "Serum", 100_000, 2)
	sh.admin.receive(sh.ikeja, "SERUM", 3)
	sh.admin.receive(sh.sul, "SERUM", 3)
	ikejaCash := fmt.Sprintf("cashier-ikeja-%s@test.local", uuid.NewString()[:8])
	sulCash := fmt.Sprintf("cashier-sul-%s@test.local", uuid.NewString()[:8])
	mgr := fmt.Sprintf("manager-%s@test.local", uuid.NewString()[:8])
	sh.admin.addStaff(ikejaCash, "cashier", sh.ikeja)
	sh.admin.addStaff(sulCash, "cashier", sh.sul)
	sh.admin.addStaff(mgr, "manager", sh.ikeja)

	cashier := loginEmail(t, ikejaCash)
	res, out := cashier.order("c-"+uuid.NewString(), sh.sul, "cash", "SERUM", 1)
	if res.StatusCode != 403 || errCode(out) != "outlet_forbidden" {
		t.Fatalf("cross outlet %d %v", res.StatusCode, out)
	}
	res = cashier.do(http.MethodPost, "/v1/staff", map[string]any{
		"email": "x@test.local", "password": "change-me-now-12", "role": "cashier", "outlet_ids": []string{sh.ikeja},
	}, nil)
	if res.StatusCode != 403 {
		t.Fatalf("cashier staff %d", res.StatusCode)
	}
	decode(t, res, nil)
	sale, sold := cashier.order("ok-"+uuid.NewString(), sh.ikeja, "cash", "SERUM", 1)
	if sale.StatusCode != 201 {
		t.Fatalf("own outlet %d %v", sale.StatusCode, sold)
	}
	h := http.Header{}
	h.Set("Idempotency-Key", "rf-"+uuid.NewString())
	refund := cashier.do(http.MethodPost, "/v1/orders/"+sold["id"].(string)+"/refund", map[string]any{}, h)
	if refund.StatusCode != 403 {
		t.Fatalf("cashier refund %d", refund.StatusCode)
	}
	decode(t, refund, nil)

	manager := loginEmail(t, mgr)
	res = manager.do(http.MethodPost, "/v1/outlets", map[string]any{"name": "Yaba", "code": "YAB"}, nil)
	if res.StatusCode != 403 {
		t.Fatalf("manager outlet %d", res.StatusCode)
	}
	decode(t, res, nil)

	other := startAPI(t)
	other.register()
	res = other.do(http.MethodGet, "/v1/orders/"+sold["id"].(string), nil, nil)
	if res.StatusCode != 404 {
		t.Fatalf("idor %d", res.StatusCode)
	}
	decode(t, res, nil)

	short := sh.admin.do(http.MethodPost, "/v1/staff", map[string]any{
		"email": "short@test.local", "password": "too-short", "role": "cashier", "outlet_ids": []string{sh.ikeja},
	}, nil)
	if short.StatusCode != 400 {
		t.Fatalf("short password %d", short.StatusCode)
	}
	decode(t, short, nil)
}

func TestDailySalesMatchBooksAndLowStock(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("SOAP", "Soap", 200_000, 5)
	sh.admin.createProduct("SERUM", "Serum", 100_000, 2)
	sh.admin.receive(sh.ikeja, "SOAP", 3)
	sh.admin.receive(sh.ikeja, "SERUM", 6)
	res, out := sh.admin.order("day-"+uuid.NewString(), sh.ikeja, "cash", "SERUM", 1)
	if res.StatusCode != 201 {
		t.Fatal(out)
	}
	loc, err := time.LoadLocation("Africa/Lagos")
	if err != nil {
		t.Fatal(err)
	}
	day := time.Now().In(loc).Format("2006-01-02")
	rep := sh.admin.do(http.MethodGet, "/v1/reports/daily-sales?outlet_id="+sh.ikeja+"&date="+day, nil, nil)
	var body map[string]any
	decode(t, rep, &body)
	if rep.StatusCode != 200 || body["matches_books"] != true || body["total_minor"].(float64) != 100_000 {
		t.Fatalf("report %d %v", rep.StatusCode, body)
	}
	low := sh.admin.do(http.MethodGet, "/v1/reports/low-stock?outlet_id="+sh.ikeja, nil, nil)
	var lowBody struct {
		Low []struct {
			SKU string `json:"sku"`
		} `json:"low_stock"`
	}
	decode(t, low, &lowBody)
	if low.StatusCode != 200 || len(lowBody.Low) != 1 || lowBody.Low[0].SKU != "SOAP" {
		t.Fatalf("low %#v", lowBody)
	}
	hold, held := sh.admin.order("holdlow-"+uuid.NewString(), sh.ikeja, "transfer", "SERUM", 4)
	if hold.StatusCode != 201 {
		t.Fatal(held)
	}
	low2 := sh.admin.do(http.MethodGet, "/v1/reports/low-stock?outlet_id="+sh.ikeja, nil, nil)
	var low2Body struct {
		Low []struct {
			SKU string `json:"sku"`
		} `json:"low_stock"`
	}
	decode(t, low2, &low2Body)
	seen := map[string]bool{}
	for _, row := range low2Body.Low {
		seen[row.SKU] = true
	}
	if !seen["SOAP"] || !seen["SERUM"] {
		t.Fatalf("expected soap and serum low, got %#v", low2Body.Low)
	}
}

func TestAuditRecordsActorAndRefundRestocks(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("SERUM", "Serum", 100_000, 1)
	sh.admin.receive(sh.ikeja, "SERUM", 4)
	email := fmt.Sprintf("cashier-%s@test.local", uuid.NewString()[:8])
	sh.admin.addStaff(email, "cashier", sh.ikeja)
	cashier := loginEmail(t, email)
	res, sold := cashier.order("aud-"+uuid.NewString(), sh.ikeja, "cash", "SERUM", 1)
	if res.StatusCode != 201 {
		t.Fatal(sold)
	}
	me := cashier.do(http.MethodGet, "/v1/auth/me", nil, nil)
	var meBody struct {
		Staff struct {
			ID string `json:"id"`
		} `json:"staff"`
	}
	decode(t, me, &meBody)
	aud := sh.admin.do(http.MethodGet, "/v1/audit?outlet_id="+sh.ikeja, nil, nil)
	var audBody struct {
		Events []struct {
			Action  string  `json:"action"`
			StaffID *string `json:"staff_id"`
		} `json:"events"`
	}
	decode(t, aud, &audBody)
	found := false
	for _, ev := range audBody.Events {
		if ev.Action == "order.paid" && ev.StaffID != nil && *ev.StaffID == meBody.Staff.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit missing cashier: %#v", audBody.Events)
	}
	h := http.Header{}
	h.Set("Idempotency-Key", "rev-"+uuid.NewString())
	blocked := sh.admin.do(http.MethodPost, "/v1/journals/"+sold["journal_id"].(string)+"/reversal", map[string]any{}, h)
	var blockedBody map[string]any
	decode(t, blocked, &blockedBody)
	if blocked.StatusCode != 409 || errCode(blockedBody) != "refund_via_order" {
		t.Fatalf("guard %d %v", blocked.StatusCode, blockedBody)
	}
	h2 := http.Header{}
	h2.Set("Idempotency-Key", "refund-"+uuid.NewString())
	refund := sh.admin.do(http.MethodPost, "/v1/orders/"+sold["id"].(string)+"/refund", map[string]any{}, h2)
	var refundBody map[string]any
	decode(t, refund, &refundBody)
	if refund.StatusCode != 201 || refundBody["status"] != "refunded" {
		t.Fatalf("refund %d %v", refund.StatusCode, refundBody)
	}
	if sh.admin.stockAvailable(sh.ikeja, "SERUM") != 4 {
		t.Fatalf("stock %d", sh.admin.stockAvailable(sh.ikeja, "SERUM"))
	}
	if sh.admin.identity()["holds"] != true {
		t.Fatal("identity")
	}
	mgrEmail := fmt.Sprintf("mgr-%s@test.local", uuid.NewString()[:8])
	sh.admin.addStaff(mgrEmail, "manager", sh.ikeja)
	_ = mgrEmail
}

func TestAdjustmentCannotPassReserved(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("SERUM", "Serum", 100_000, 2)
	sh.admin.receive(sh.ikeja, "SERUM", 5)
	res, out := sh.admin.order("adj-"+uuid.NewString(), sh.ikeja, "transfer", "SERUM", 3)
	if res.StatusCode != 201 {
		t.Fatal(out)
	}
	bad := sh.admin.do(http.MethodPost, "/v1/stock/adjustments", map[string]any{
		"outlet_id": sh.ikeja, "sku": "SERUM", "delta": -3, "reason": "damage",
	}, nil)
	var body map[string]any
	decode(t, bad, &body)
	if bad.StatusCode != 409 || errCode(body) != "insufficient_stock" {
		t.Fatalf("adjust %d %v", bad.StatusCode, body)
	}
	if stockField(t, sh.admin, sh.ikeja, "SERUM", "on_hand") != 5 {
		t.Fatal("on_hand changed")
	}
}

func stockField(t *testing.T, c *apiClient, outlet, sku, field string) int {
	t.Helper()
	res := c.do(http.MethodGet, "/v1/reports/stock-on-hand?outlet_id="+outlet, nil, nil)
	var out struct {
		Stock []map[string]any `json:"stock"`
	}
	decode(t, res, &out)
	for _, row := range out.Stock {
		if row["sku"] == sku {
			return int(row[field].(float64))
		}
	}
	t.Fatalf("missing %s", sku)
	return 0
}
