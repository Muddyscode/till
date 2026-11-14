package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Muddyscode/till/internal/payments"
	"github.com/google/uuid"
)

// tillEvent posts a payment in the till format, signed with the connection's secret.
func (c *apiClient) tillEvent(connID, secret string, ev map[string]any) (*http.Response, map[string]any) {
	c.t.Helper()
	body, err := json.Marshal(ev)
	if err != nil {
		c.t.Fatal(err)
	}
	h := http.Header{}
	h.Set("Till-Signature", payments.SignTill(secret, time.Now().Unix(), body))
	res := c.doRaw(http.MethodPost, "/v1/webhooks/"+connID, string(body), h)
	var out map[string]any
	decode(c.t, res, &out)
	return res, out
}

func (sh *shop) stock(sku string, price int64, qty int) {
	sh.t.Helper()
	sh.admin.createProduct(sku, sku, price, 1)
	sh.admin.receive(sh.ikeja, sku, qty)
}

// A tap on a terminal is confirmed by the terminal's own connection, in the
// till format: the sale posts to that connection's clearing account, the
// provider's fee is booked, and a replay or a forged signature changes nothing.
func TestTapSaleConfirmedByTillFormatEvent(t *testing.T) {
	sh := openShop(t, 0)
	sh.stock("SERUM", 100_000, 3)
	connID, conn := sh.admin.connection("POS", "till", "", sh.ikeja)
	secret, _ := conn["secret"].(string)
	if secret == "" || conn["webhook_path"] != "/v1/webhooks/"+connID {
		t.Fatalf("connection %v", conn)
	}

	res, ord := sh.admin.order("tap-"+uuid.NewString(), sh.ikeja, "tap", "SERUM", 1)
	if res.StatusCode != 201 || ord["status"] != "pending_payment" || ord["payment_method"] != "tap" {
		t.Fatalf("order %d %v", res.StatusCode, ord)
	}
	ev := map[string]any{
		"id": "term-" + uuid.NewString(), "reference": ord["reference"], "amount_minor": 100_000,
		"fee_minor": 500, "method": "tap",
	}
	if res, _ := sh.admin.tillEvent(connID, "wrong-secret-wrong-secret", ev); res.StatusCode != 401 {
		t.Fatalf("forged event answered %d", res.StatusCode)
	}
	res, paid := sh.admin.tillEvent(connID, secret, ev)
	if res.StatusCode != 200 || paid["status"] != "paid" {
		t.Fatalf("event %d %v", res.StatusCode, paid)
	}
	if res, again := sh.admin.tillEvent(connID, secret, ev); res.Header.Get("Idempotent-Replayed") != "true" || again["id"] != paid["id"] {
		t.Fatalf("replay %v %v", res.Header, again)
	}
	// Clearing holds the sale less the fee; the fee is an expense.
	if sh.bal("1100-POS") != 99_500 || sh.bal("5100") != 500 {
		t.Fatalf("clearing %d fees %d", sh.bal("1100-POS"), sh.bal("5100"))
	}
	rep := sh.admin.getMap("/v1/reports/daily-sales?outlet_id=" + sh.ikeja)
	if tt := rep["tender_totals"].(map[string]any); num(tt, "tap_minor") != 100_000 || rep["matches_books"] != true {
		t.Fatalf("report %v", rep)
	}
	sh.mustHold()
}

// A customer who transfers into the shop's account sends no order reference.
// The one pending order waiting for that exact amount is paid; when two orders
// wait for the same amount nothing is guessed and a manager picks.
func TestTransferWithoutReferenceIsMatchedByAmount(t *testing.T) {
	sh := openShop(t, 0)
	sh.stock("SERUM", 100_000, 5)
	sh.stock("TOWEL", 30_000, 5)
	connID, conn := sh.admin.connection("BANK", "till", "", sh.ikeja)
	secret := conn["secret"].(string)
	event := func(amount int64) (*http.Response, map[string]any) {
		return sh.admin.tillEvent(connID, secret, map[string]any{
			"id": "nip-" + uuid.NewString(), "amount_minor": amount, "method": "transfer",
			"payer": map[string]any{"name": "ADA OKONKWO", "bank": "GTB"},
		})
	}

	_, towel := sh.admin.order("t-"+uuid.NewString(), sh.ikeja, "transfer", "TOWEL", 1)
	if res, out := event(30_000); res.StatusCode != 200 || out["status"] != "paid" || out["id"] != towel["id"] {
		t.Fatalf("single candidate not paid: %d %v", res.StatusCode, out)
	}

	_, a := sh.admin.order("a-"+uuid.NewString(), sh.ikeja, "transfer", "SERUM", 1)
	_, b := sh.admin.order("b-"+uuid.NewString(), sh.ikeja, "transfer", "SERUM", 1)
	res, out := event(100_000)
	if res.StatusCode != 200 || out["status"] != "unmatched" || out["reason"] != "ambiguous_amount" {
		t.Fatalf("two candidates: %d %v", res.StatusCode, out)
	}
	if sh.bal("2300") != 100_000 {
		t.Fatalf("suspense %d", sh.bal("2300"))
	}
	open := sh.admin.listUnmatched(sh.ikeja, "open")
	if len(open) != 1 || open[0]["details"].(map[string]any)["payer_name"] != "ADA OKONKWO" {
		t.Fatalf("unmatched rows %v", open)
	}
	// The manager says it was for order b.
	path := "/v1/unmatched-payments/" + out["unmatched_payment_id"].(string) + "/apply"
	if res, applied := sh.admin.postKeyed(path, "ap-"+uuid.NewString(), map[string]any{"order_id": b["id"]}); res.StatusCode != 200 || applied["status"] != "applied" {
		t.Fatalf("apply %d %v", res.StatusCode, applied)
	}
	if got := sh.admin.getMap("/v1/orders/" + b["id"].(string)); got["status"] != "paid" {
		t.Fatalf("order b %v", got["status"])
	}
	if got := sh.admin.getMap("/v1/orders/" + a["id"].(string)); got["status"] != "pending_payment" {
		t.Fatalf("order a %v", got["status"])
	}
	if sh.bal("2300") != 0 || sh.bal("1100-BANK") != 130_000 {
		t.Fatalf("suspense %d clearing %d", sh.bal("2300"), sh.bal("1100-BANK"))
	}
	sh.mustHold()
}

// A merchant with no provider integration can still take a transfer sale: a
// manager confirms it on the connection after checking the bank. A cashier
// cannot, one merchant's connection cannot pay another merchant's order, and a
// disabled connection takes nothing.
func TestManagerRecordsPaymentAndConnectionsAreIsolated(t *testing.T) {
	sh := openShop(t, 0)
	sh.stock("SERUM", 100_000, 3)
	connID, conn := sh.admin.connection("GTB", "till", "", "")
	_, ord := sh.admin.order("m-"+uuid.NewString(), sh.ikeja, "transfer", "SERUM", 1)
	body := map[string]any{"id": "bank-" + uuid.NewString(), "reference": ord["reference"], "amount_minor": 100_000, "method": "transfer"}
	path := "/v1/payment-connections/" + connID + "/payments"

	cashier := sh.staffClient("cashier", sh.ikeja)
	if res := cashier.do(http.MethodPost, path, body, nil); res.StatusCode != 403 {
		t.Fatalf("cashier recorded a payment: %d", res.StatusCode)
	} else {
		res.Body.Close()
	}
	manager := sh.staffClient("manager", sh.ikeja)
	res := manager.do(http.MethodPost, path, body, nil)
	var paid map[string]any
	decode(t, res, &paid)
	if res.StatusCode != 200 || paid["status"] != "paid" {
		t.Fatalf("manager record %d %v", res.StatusCode, paid)
	}

	// Another merchant holding this order's reference cannot pay it.
	other := openShop(t, 0)
	other.stock("SERUM", 100_000, 1)
	_, theirs := sh.admin.order("o-"+uuid.NewString(), sh.ikeja, "transfer", "SERUM", 1)
	otherID, otherConn := other.admin.connection("POS", "till", "", "")
	res, out := other.admin.tillEvent(otherID, otherConn["secret"].(string), map[string]any{
		"id": "x-" + uuid.NewString(), "reference": theirs["reference"], "amount_minor": 100_000,
	})
	if res.StatusCode != 200 || out["status"] != "unmatched" || out["reason"] != "no_matching_order" {
		t.Fatalf("cross-merchant event: %d %v", res.StatusCode, out)
	}
	if got := sh.admin.getMap("/v1/orders/" + theirs["id"].(string)); got["status"] != "pending_payment" {
		t.Fatalf("another merchant's connection paid the order: %v", got["status"])
	}

	if res := sh.admin.do(http.MethodDelete, "/v1/payment-connections/"+connID, nil, nil); res.StatusCode != 204 {
		t.Fatalf("disable %d", res.StatusCode)
	}
	if res, _ := sh.admin.tillEvent(connID, conn["secret"].(string), map[string]any{"id": "late", "amount_minor": 1_000}); res.StatusCode != 401 {
		t.Fatalf("disabled connection answered %d", res.StatusCode)
	}
	list := sh.admin.getMap("/v1/payment-connections")["payment_connections"].([]any)
	if len(list) != 1 || fmt.Sprint(list[0].(map[string]any)["secret"]) != "<nil>" {
		t.Fatalf("list must not show secrets: %v", list)
	}
	sh.mustHold()
	other.mustHold()
}
