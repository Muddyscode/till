package integration

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Muddyscode/till/internal/events"
	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
)

// Phase 3B: shifts, event feed + webhooks, transfers, counts, suppliers, VAT.

func (c *apiClient) mustStatus(res *http.Response, want int) map[string]any {
	c.t.Helper()
	var out map[string]any
	decode(c.t, res, &out)
	if res.StatusCode != want {
		c.t.Fatalf("status %d, want %d: %v", res.StatusCode, want, out)
	}
	return out
}

func (c *apiClient) receiveWith(outlet, sku string, qty int, unit int64, extra map[string]any) map[string]any {
	c.t.Helper()
	body := map[string]any{"outlet_id": outlet, "sku": sku, "qty": qty, "unit_cost_minor": unit}
	for k, v := range extra {
		body[k] = v
	}
	return c.mustStatus(c.do(http.MethodPost, "/v1/stock/receipts", body, nil), 200)
}

func onHand(t *testing.T, c *apiClient, outlet, sku string) int {
	t.Helper()
	return stockField(t, c, outlet, sku, "on_hand")
}

func TestShiftCashUpShortAndOver(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("TEE", "Tee", 100_000, 0)
	sh.admin.receive(sh.ikeja, "TEE", 20)
	cashier := sh.staffClient("cashier", sh.ikeja)

	open := cashier.mustStatus(cashier.do(http.MethodPost, "/v1/shifts/open", map[string]any{"outlet_id": sh.ikeja, "opening_float_minor": 500_000}, nil), 201)
	shiftID := open["id"].(string)
	dup := cashier.mustStatus(cashier.do(http.MethodPost, "/v1/shifts/open", map[string]any{"outlet_id": sh.ikeja, "opening_float_minor": 0}, nil), 409)
	if errCode(dup) != "shift_already_open" {
		t.Fatalf("dup open: %v", dup)
	}

	for i := 0; i < 2; i++ {
		if res, out := cashier.order("shift-"+uuid.NewString(), sh.ikeja, "cash", "TEE", 1+i); res.StatusCode != 201 {
			t.Fatalf("order %d %v", res.StatusCode, out)
		}
	}
	// A refund by the admin is not the cashier's cash movement.
	_, sold := sh.admin.order("adm-"+uuid.NewString(), sh.ikeja, "cash", "TEE", 1)
	sh.admin.mustStatus(sh.admin.do(http.MethodPost, "/v1/orders/"+sold["id"].(string)+"/refund", map[string]any{}, keyHeader("rf-"+uuid.NewString())), 201)

	live := cashier.mustStatus(cashier.do(http.MethodGet, "/v1/shifts/"+shiftID+"/z-report", nil, nil), 200)
	if live["expected_cash_minor"].(float64) != 800_000 || live["sales_count"].(float64) != 2 {
		t.Fatalf("live z-report %v", live)
	}

	// Cashier cannot close somebody else's shift; the admin may open and close anyone's.
	adminShift := sh.admin.mustStatus(sh.admin.do(http.MethodPost, "/v1/shifts/open", map[string]any{"outlet_id": sh.ikeja, "opening_float_minor": 10_000}, nil), 201)
	deny := cashier.do(http.MethodPost, "/v1/shifts/"+adminShift["id"].(string)+"/close", map[string]any{"counted_cash_minor": 0}, nil)
	deny.Body.Close()
	if deny.StatusCode != 403 && deny.StatusCode != 404 {
		t.Fatalf("cashier closing admin shift: %d", deny.StatusCode)
	}

	closed := cashier.mustStatus(cashier.do(http.MethodPost, "/v1/shifts/"+shiftID+"/close", map[string]any{"counted_cash_minor": 795_000, "note": "short"}, nil), 200)
	z := closed["z_report"].(map[string]any)
	if closed["status"] != "closed" || closed["variance_minor"].(float64) != -5_000 || z["expected_cash_minor"].(float64) != 800_000 ||
		z["cash_sales_minor"].(float64) != 300_000 || z["sales_count"].(float64) != 2 || z["counted_cash_minor"].(float64) != 795_000 {
		t.Fatalf("close %v", closed)
	}
	if closed["variance_journal_id"] == nil {
		t.Fatal("variance journal missing")
	}
	if got := sh.bal("5300"); got != 5_000 {
		t.Fatalf("5300 = %d, want 5000", got)
	}
	again := cashier.mustStatus(cashier.do(http.MethodPost, "/v1/shifts/"+shiftID+"/close", map[string]any{"counted_cash_minor": 795_000}, nil), 409)
	if errCode(again) != "already_closed" {
		t.Fatalf("second close %v", again)
	}
	frozen := cashier.mustStatus(cashier.do(http.MethodGet, "/v1/shifts/"+shiftID+"/z-report", nil, nil), 200)
	if frozen["variance_minor"].(float64) != -5_000 {
		t.Fatalf("frozen z-report %v", frozen)
	}
	sh.mustHold()

	// Manager/admin closes another staff member's shift; exact count posts no journal.
	other := cashier.mustStatus(cashier.do(http.MethodPost, "/v1/shifts/open", map[string]any{"outlet_id": sh.ikeja, "opening_float_minor": 20_000}, nil), 201)
	exact := sh.admin.mustStatus(sh.admin.do(http.MethodPost, "/v1/shifts/"+other["id"].(string)+"/close", map[string]any{"counted_cash_minor": 20_000}, nil), 200)
	if exact["variance_minor"].(float64) != 0 || exact["variance_journal_id"] != nil {
		t.Fatalf("exact close %v", exact)
	}

	// Overage on a second outlet credits 5300.
	sh.admin.receive(sh.sul, "TEE", 5)
	s2 := sh.admin.mustStatus(sh.admin.do(http.MethodPost, "/v1/shifts/open", map[string]any{"outlet_id": sh.sul, "opening_float_minor": 0}, nil), 201)
	sh.admin.order("sul-"+uuid.NewString(), sh.sul, "cash", "TEE", 1)
	over := sh.admin.mustStatus(sh.admin.do(http.MethodPost, "/v1/shifts/"+s2["id"].(string)+"/close", map[string]any{"counted_cash_minor": 102_000}, nil), 200)
	if over["variance_minor"].(float64) != 2_000 {
		t.Fatalf("over %v", over)
	}
	if got := sh.bal("5300"); got != 3_000 {
		t.Fatalf("5300 = %d, want 3000 (5000 short less 2000 over)", got)
	}
	list := sh.admin.mustStatus(sh.admin.do(http.MethodGet, "/v1/shifts?outlet_id="+sh.ikeja+"&status=closed", nil, nil), 200)
	if n := len(list["shifts"].([]any)); n != 2 {
		t.Fatalf("closed ikeja shifts %d", n)
	}
	sh.mustHold()
}

type hookReceiver struct {
	mu      sync.Mutex
	hits    map[string]int // event id -> attempts
	bodies  map[string][]byte
	sigs    map[string]string
	failFor map[string]int // event type -> failures to serve first
	redir   int
	srv     *httptest.Server
}

func newReceiver(t *testing.T, failFor map[string]int) *hookReceiver {
	h := &hookReceiver{hits: map[string]int{}, bodies: map[string][]byte{}, sigs: map[string]string{}, failFor: failFor}
	mux := http.NewServeMux()
	mux.HandleFunc("/hook", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		id := r.Header.Get("Till-Event-Id")
		h.mu.Lock()
		h.hits[id]++
		n := h.hits[id]
		h.bodies[id] = b
		h.sigs[id] = r.Header.Get("Till-Signature")
		typ := ""
		for k := range h.failFor {
			if strings.Contains(strings.ReplaceAll(string(b), `": "`, `":"`), `"type":"`+k+`"`) {
				typ = k
			}
		}
		fail := typ != "" && n <= h.failFor[typ]
		h.mu.Unlock()
		if fail {
			http.Error(w, "nope", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/landed", http.StatusFound)
	})
	mux.HandleFunc("/landed", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.redir++
		h.mu.Unlock()
	})
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

func TestEventFeedAndWebhooks(t *testing.T) {
	sh := openShop(t, 0)
	rcv := newReceiver(t, map[string]int{"order.paid": 2})

	bad := sh.admin.do(http.MethodPost, "/v1/webhook-endpoints", map[string]any{"url": "http://example.com/hook"}, nil)
	bad.Body.Close()
	if bad.StatusCode != 400 {
		t.Fatalf("plain http to a public host: %d", bad.StatusCode)
	}
	ep := sh.admin.mustStatus(sh.admin.do(http.MethodPost, "/v1/webhook-endpoints", map[string]any{
		"url": rcv.srv.URL + "/hook", "event_types": []string{"order.paid", "stock.low", "shift.closed"},
	}, nil), 201)
	secret, _ := ep["secret"].(string)
	if !strings.HasPrefix(secret, "whsec_") {
		t.Fatalf("no secret: %v", ep)
	}
	ep2 := sh.admin.mustStatus(sh.admin.do(http.MethodPost, "/v1/webhook-endpoints", map[string]any{"url": rcv.srv.URL + "/redirect"}, nil), 201)
	listed := sh.admin.mustStatus(sh.admin.do(http.MethodGet, "/v1/webhook-endpoints", nil, nil), 200)
	for _, e := range listed["webhook_endpoints"].([]any) {
		if _, has := e.(map[string]any)["secret"]; has {
			t.Fatal("secret listed again")
		}
	}

	sh.admin.createProduct("MUG", "Mug", 100_000, 5)
	sh.admin.receive(sh.ikeja, "MUG", 6)
	_, ord := sh.admin.order("hook-"+uuid.NewString(), sh.ikeja, "cash", "MUG", 2) // available 4 <= reorder 5
	orderID := ord["id"].(string)

	w := &events.Worker{Store: store.New(testPool), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Client: events.NewClient(false), BackoffBase: 5 * time.Millisecond, Interval: 10 * time.Millisecond}
	deadline := time.Now().Add(20 * time.Second)
	var deliveries []any
	for {
		if err := w.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		d := sh.admin.mustStatus(sh.admin.do(http.MethodGet, "/v1/webhook-endpoints/"+ep["id"].(string)+"/deliveries", nil, nil), 200)
		deliveries = d["deliveries"].([]any)
		done := len(deliveries) >= 2
		for _, x := range deliveries {
			if x.(map[string]any)["status"] != "delivered" {
				done = false
			}
		}
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("deliveries not done: %v", deliveries)
		}
		time.Sleep(15 * time.Millisecond)
	}
	types := map[string]map[string]any{}
	for _, x := range deliveries {
		d := x.(map[string]any)
		types[d["event_type"].(string)] = d
	}
	paid, low := types["order.paid"], types["stock.low"]
	if paid == nil || low == nil || paid["attempts"].(float64) < 3 {
		t.Fatalf("deliveries %v", deliveries)
	}
	rcv.mu.Lock()
	body := rcv.bodies[paid["event_id"].(string)]
	sig := rcv.sigs[paid["event_id"].(string)]
	redirHits := rcv.redir
	rcv.mu.Unlock()
	var ts int64
	var mac string
	if _, err := fmt.Sscanf(strings.Replace(strings.Replace(sig, "t=", "", 1), ",v1=", " ", 1), "%d %s", &ts, &mac); err != nil {
		t.Fatalf("signature %q: %v", sig, err)
	}
	if sig != events.Sign(secret, ts, body) {
		t.Fatalf("bad signature %q", sig)
	}
	if !strings.Contains(string(body), orderID) || !strings.Contains(strings.ReplaceAll(string(body), `": "`, `":"`), `"type":"order.paid"`) {
		t.Fatalf("payload %s", body)
	}
	if redirHits != 0 {
		t.Fatal("redirect was followed")
	}
	// The unfiltered endpoint got the redirect answer (302), never followed.
	for i := 0; i < 3; i++ {
		_ = w.Tick(context.Background())
	}
	d2 := sh.admin.mustStatus(sh.admin.do(http.MethodGet, "/v1/webhook-endpoints/"+ep2["id"].(string)+"/deliveries", nil, nil), 200)
	if len(d2["deliveries"].([]any)) == 0 || d2["deliveries"].([]any)[0].(map[string]any)["last_status"].(float64) != 302 {
		t.Fatalf("redirect endpoint deliveries %v", d2)
	}

	// The polled feed carries the same order.paid and pages by cursor.
	feed := sh.admin.mustStatus(sh.admin.do(http.MethodGet, "/v1/events?types=order.paid,stock.received", nil, nil), 200)
	evs := feed["events"].([]any)
	if len(evs) != 2 {
		t.Fatalf("feed %v", feed)
	}
	first := evs[0].(map[string]any)
	if first["type"] != "stock.received" || first["id"] == nil || first["occurred_at"] == nil || first["data"] == nil {
		t.Fatalf("event shape %v", first)
	}
	next := sh.admin.mustStatus(sh.admin.do(http.MethodGet, "/v1/events?cursor="+feed["next_cursor"].(string), nil, nil), 200)
	if len(next["events"].([]any)) != 0 || next["next_cursor"] != feed["next_cursor"] {
		t.Fatalf("second page %v", next)
	}
	badType := sh.admin.do(http.MethodGet, "/v1/events?types=nope", nil, nil)
	badType.Body.Close()
	if badType.StatusCode != 400 {
		t.Fatalf("unknown type %d", badType.StatusCode)
	}

	del := sh.admin.do(http.MethodDelete, "/v1/webhook-endpoints/"+ep["id"].(string), nil, nil)
	del.Body.Close()
	if del.StatusCode != 204 {
		t.Fatalf("delete %d", del.StatusCode)
	}
	gone := sh.admin.do(http.MethodGet, "/v1/webhook-endpoints/"+ep["id"].(string)+"/deliveries", nil, nil)
	gone.Body.Close()
	if gone.StatusCode != 404 {
		t.Fatalf("deleted endpoint deliveries %d", gone.StatusCode)
	}
	cashier := sh.staffClient("cashier", sh.ikeja)
	forb := cashier.do(http.MethodGet, "/v1/webhook-endpoints", nil, nil)
	forb.Body.Close()
	if forb.StatusCode != 403 {
		t.Fatalf("cashier webhook list %d", forb.StatusCode)
	}
	sh.mustHold()
}

func TestStockTransfers(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("LAMP", "Lamp", 500_000, 0)
	sh.admin.receiveWith(sh.ikeja, "LAMP", 10, 200_000, nil)
	mgr := sh.staffClient("manager", sh.ikeja)
	body := map[string]any{"from_outlet_id": sh.ikeja, "to_outlet_id": sh.sul, "lines": []map[string]any{{"sku": "LAMP", "quantity": 4}}, "note": "restock"}

	key := "tr-" + uuid.NewString()
	res := mgr.do(http.MethodPost, "/v1/stock/transfers", body, keyHeader(key))
	tr := mgr.mustStatus(res, 201)
	if tr["status"] != "in_transit" {
		t.Fatalf("transfer %v", tr)
	}
	replay := mgr.do(http.MethodPost, "/v1/stock/transfers", body, keyHeader(key))
	if replay.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatal("expected replay")
	}
	if rb := mgr.mustStatus(replay, 201); rb["id"] != tr["id"] {
		t.Fatal("replay returned a different transfer")
	}
	if got := onHand(t, sh.admin, sh.ikeja, "LAMP"); got != 6 {
		t.Fatalf("source on_hand %d, want 6 (deducted once)", got)
	}
	sh.mustHold() // units in transit still count as inventory

	id := tr["id"].(string)
	// The source manager is not assigned to the destination.
	deny := mgr.do(http.MethodPost, "/v1/stock/transfers/"+id+"/receive", nil, nil)
	deny.Body.Close()
	if deny.StatusCode != 403 {
		t.Fatalf("source manager receiving: %d", deny.StatusCode)
	}
	sh.admin.mustStatus(sh.admin.do(http.MethodPost, "/v1/stock/transfers/"+id+"/receive", nil, nil), 200)
	if got := onHand(t, sh.admin, sh.sul, "LAMP"); got != 4 {
		t.Fatalf("destination on_hand %d", got)
	}
	again := sh.admin.mustStatus(sh.admin.do(http.MethodPost, "/v1/stock/transfers/"+id+"/receive", nil, nil), 409)
	if errCode(again) != "transfer_not_in_transit" {
		t.Fatalf("double receive %v", again)
	}
	late := sh.admin.mustStatus(sh.admin.do(http.MethodPost, "/v1/stock/transfers/"+id+"/cancel", nil, nil), 409)
	if errCode(late) != "transfer_not_in_transit" {
		t.Fatalf("cancel after receive %v", late)
	}

	// Cancel returns the units to the source.
	body["lines"] = []map[string]any{{"sku": "LAMP", "quantity": 3}}
	t2 := mgr.mustStatus(mgr.do(http.MethodPost, "/v1/stock/transfers", body, nil), 201)
	if got := onHand(t, sh.admin, sh.ikeja, "LAMP"); got != 3 {
		t.Fatalf("after second send %d", got)
	}
	sh.mustHold()
	mgr.mustStatus(mgr.do(http.MethodPost, "/v1/stock/transfers/"+t2["id"].(string)+"/cancel", nil, nil), 200)
	if got := onHand(t, sh.admin, sh.ikeja, "LAMP"); got != 6 {
		t.Fatalf("after cancel %d", got)
	}

	// Cannot send more than is available, and reserved units stay put.
	body["lines"] = []map[string]any{{"sku": "LAMP", "quantity": 100}}
	short := mgr.mustStatus(mgr.do(http.MethodPost, "/v1/stock/transfers", body, nil), 409)
	if errCode(short) != "insufficient_stock" {
		t.Fatalf("oversend %v", short)
	}
	if res, out := sh.admin.order("hold-"+uuid.NewString(), sh.ikeja, "transfer", "LAMP", 5); res.StatusCode != 201 {
		t.Fatalf("reserve %v", out)
	}
	body["lines"] = []map[string]any{{"sku": "LAMP", "quantity": 2}}
	held := mgr.mustStatus(mgr.do(http.MethodPost, "/v1/stock/transfers", body, nil), 409)
	if errCode(held) != "insufficient_stock" {
		t.Fatalf("send reserved %v", held)
	}
	list := mgr.mustStatus(mgr.do(http.MethodGet, "/v1/stock/transfers", nil, nil), 200)
	if len(list["transfers"].([]any)) != 2 {
		t.Fatalf("list %v", list)
	}
	cashier := sh.staffClient("cashier", sh.ikeja)
	f := cashier.do(http.MethodGet, "/v1/stock/transfers", nil, nil)
	f.Body.Close()
	if f.StatusCode != 403 {
		t.Fatalf("cashier transfers %d", f.StatusCode)
	}
	sh.mustHold()
}

func TestStockCount(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("CUP", "Cup", 100_000, 0)
	sh.admin.receiveWith(sh.ikeja, "CUP", 10, 20_000, nil)
	invBefore := sh.bal("1200")

	count := func(counted int) (*http.Response, map[string]any) {
		res := sh.admin.do(http.MethodPost, "/v1/stock/counts", map[string]any{
			"outlet_id": sh.ikeja, "lines": []map[string]any{{"sku": "CUP", "counted": counted}},
		}, nil)
		var out map[string]any
		decode(t, res, &out)
		return res, out
	}
	res, out := count(7)
	if res.StatusCode != 201 {
		t.Fatalf("count %d %v", res.StatusCode, out)
	}
	line := out["lines"].([]any)[0].(map[string]any)
	if line["variance"].(float64) != -3 || line["expected_on_hand"].(float64) != 10 || line["variance_value_minor"].(float64) != -60_000 {
		t.Fatalf("line %v", line)
	}
	if onHand(t, sh.admin, sh.ikeja, "CUP") != 7 || sh.bal("1200") != invBefore-60_000 || sh.bal("5200") != 60_000 {
		t.Fatalf("books after shrink: inv %d adj %d", sh.bal("1200"), sh.bal("5200"))
	}
	sh.mustHold()
	got := sh.admin.mustStatus(sh.admin.do(http.MethodGet, "/v1/stock/counts/"+out["id"].(string), nil, nil), 200)
	if got["net_variance_value_minor"].(float64) != -60_000 {
		t.Fatalf("get count %v", got)
	}

	if res, out := count(9); res.StatusCode != 201 || out["lines"].([]any)[0].(map[string]any)["variance"].(float64) != 2 {
		t.Fatalf("count up %d %v", res.StatusCode, out)
	}
	if onHand(t, sh.admin, sh.ikeja, "CUP") != 9 {
		t.Fatal("on_hand not set to counted")
	}
	// Below reserved is a conflict and changes nothing.
	if r, o := sh.admin.order("res-"+uuid.NewString(), sh.ikeja, "transfer", "CUP", 4); r.StatusCode != 201 {
		t.Fatalf("reserve %v", o)
	}
	res, out = count(2)
	if res.StatusCode != 409 || errCode(out) != "insufficient_stock" {
		t.Fatalf("below reserved %d %v", res.StatusCode, out)
	}
	if onHand(t, sh.admin, sh.ikeja, "CUP") != 9 {
		t.Fatal("failed count changed stock")
	}
	cashier := sh.staffClient("cashier", sh.ikeja)
	f := cashier.do(http.MethodPost, "/v1/stock/counts", map[string]any{"outlet_id": sh.ikeja, "lines": []map[string]any{{"sku": "CUP", "counted": 1}}}, nil)
	f.Body.Close()
	if f.StatusCode != 403 {
		t.Fatalf("cashier count %d", f.StatusCode)
	}
	sh.mustHold()
}

func TestSuppliersAndPayments(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("RICE", "Rice", 100_000, 0)
	sup := sh.admin.mustStatus(sh.admin.do(http.MethodPost, "/v1/suppliers", map[string]any{"name": "Mama Put Ltd", "phone": uniquePhone(), "note": "weekly"}, nil), 201)
	supID := sup["id"].(string)
	patched := sh.admin.mustStatus(sh.admin.do(http.MethodPatch, "/v1/suppliers/"+supID, map[string]any{"note": "monthly"}, nil), 200)
	if patched["note"] != "monthly" || patched["name"] != "Mama Put Ltd" {
		t.Fatalf("patch %v", patched)
	}
	missing := sh.admin.do(http.MethodPost, "/v1/stock/receipts", map[string]any{"outlet_id": sh.ikeja, "sku": "RICE", "qty": 1, "supplier_id": uuid.NewString()}, nil)
	missing.Body.Close()
	if missing.StatusCode != 404 {
		t.Fatalf("receipt with unknown supplier %d", missing.StatusCode)
	}
	sh.admin.receiveWith(sh.ikeja, "RICE", 10, 30_000, map[string]any{"supplier_id": supID})
	if sh.bal("2150") != 300_000 {
		t.Fatalf("2150 = %d", sh.bal("2150"))
	}

	pay := func(method string, amount int64) (*http.Response, map[string]any) {
		res := sh.admin.do(http.MethodPost, "/v1/supplier-payments", map[string]any{
			"supplier_id": supID, "outlet_id": sh.ikeja, "amount_minor": amount, "method": method, "note": "part",
		}, keyHeader("sp-"+uuid.NewString()))
		var out map[string]any
		decode(t, res, &out)
		return res, out
	}
	// Cash needs cash in the drawer: sell something first.
	sh.admin.receive(sh.ikeja, "RICE", 5)
	sh.admin.order("cash-"+uuid.NewString(), sh.ikeja, "cash", "RICE", 2) // 200_000 in the till
	if res, out := pay("cash", 100_000); res.StatusCode != 201 {
		t.Fatalf("cash payment %d %v", res.StatusCode, out)
	}
	if res, out := pay("cash", 5_000_000); res.StatusCode == 201 {
		t.Fatalf("overdrawn cash payment accepted %v", out)
	}
	// Bank payments draw on 1050 Bank: a paid transfer sale lands in the
	// provider's clearing account and a settlement moves it to the bank.
	_, ord := sh.admin.order("tr-"+uuid.NewString(), sh.ikeja, "transfer", "RICE", 1)
	if res, _ := sh.admin.webhook(nextEvent(), ord["reference"].(string), 100_000, "NGN"); res.StatusCode != 200 {
		t.Fatalf("webhook %d", res.StatusCode)
	}
	if res, out := pay("bank", 50_000); res.StatusCode == 201 {
		t.Fatalf("bank payment accepted before any money reached the bank %v", out)
	}
	if res, out := sh.admin.postKeyed("/v1/payment-connections/"+sh.admin.paystackConn()+"/settlements", "st-"+uuid.NewString(), map[string]any{"amount_minor": 100_000, "reference": "PAYOUT-1"}); res.StatusCode != 201 {
		t.Fatalf("settlement %d %v", res.StatusCode, out)
	}
	if sh.bal("1100-PSTK") != 0 || sh.bal("1050") != 100_000 {
		t.Fatalf("after settlement clearing %d bank %d", sh.bal("1100-PSTK"), sh.bal("1050"))
	}
	if res, out := pay("bank", 50_000); res.StatusCode != 201 {
		t.Fatalf("bank payment %d %v", res.StatusCode, out)
	}
	st := sh.admin.mustStatus(sh.admin.do(http.MethodGet, "/v1/suppliers/"+supID+"/statement", nil, nil), 200)
	if st["total_received_minor"].(float64) != 300_000 || st["total_paid_minor"].(float64) != 150_000 || st["balance_owed_minor"].(float64) != 150_000 || len(st["entries"].([]any)) != 3 {
		t.Fatalf("statement %v", st)
	}
	if sh.bal("2150") != 300_000 { // 450k credited (incl. an uncosted-line receipt at avg cost) less 150k paid
		t.Fatalf("2150 = %d after payments", sh.bal("2150"))
	}
	cashier := sh.staffClient("cashier", sh.ikeja)
	f := cashier.do(http.MethodPost, "/v1/supplier-payments", map[string]any{"supplier_id": supID, "outlet_id": sh.ikeja, "amount_minor": 1, "method": "cash"}, keyHeader("x-"+uuid.NewString()))
	f.Body.Close()
	if f.StatusCode != 403 {
		t.Fatalf("cashier payment %d", f.StatusCode)
	}
	del := sh.admin.do(http.MethodDelete, "/v1/suppliers/"+supID, nil, nil)
	del.Body.Close()
	if del.StatusCode != 204 {
		t.Fatalf("delete supplier %d", del.StatusCode)
	}
	sh.mustHold()
}

func TestVATReport(t *testing.T) {
	sh := openShop(t, 750)
	sh.admin.createProduct("PEN", "Pen", 107_500, 0)
	sh.admin.receive(sh.ikeja, "PEN", 10)
	_, a := sh.admin.order("v1-"+uuid.NewString(), sh.ikeja, "cash", "PEN", 2)
	sh.admin.order("v2-"+uuid.NewString(), sh.ikeja, "cash", "PEN", 1)
	sh.admin.mustStatus(sh.admin.do(http.MethodPost, "/v1/orders/"+a["id"].(string)+"/refund", map[string]any{}, keyHeader("rf-"+uuid.NewString())), 201)

	rep := sh.admin.mustStatus(sh.admin.do(http.MethodGet, "/v1/reports/vat", nil, nil), 200)
	tot := rep["totals"].(map[string]any)
	if tot["output_vat_minor"].(float64) != 22_500 || tot["reversed_vat_minor"].(float64) != 15_000 ||
		tot["net_output_vat_minor"].(float64) != 7_500 || tot["input_vat_minor"].(float64) != 0 {
		t.Fatalf("totals %v", tot)
	}
	if int64(tot["net_output_vat_minor"].(float64)) != sh.bal("2100") {
		t.Fatal("report does not match account 2100")
	}
	if len(rep["months"].([]any)) != 1 {
		t.Fatalf("months %v", rep["months"])
	}
	empty := sh.admin.mustStatus(sh.admin.do(http.MethodGet, "/v1/reports/vat?from=2001-01-01&to=2001-12-31", nil, nil), 200)
	if len(empty["months"].([]any)) != 0 {
		t.Fatalf("old range %v", empty)
	}
	mgr := sh.staffClient("manager", sh.ikeja)
	f := mgr.do(http.MethodGet, "/v1/reports/vat", nil, nil)
	f.Body.Close()
	if f.StatusCode != 403 {
		t.Fatalf("manager vat %d", f.StatusCode)
	}
	sh.mustHold()
}
