package integration

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func (c *apiClient) accountByCode(code string) (string, int64) {
	c.t.Helper()
	res := c.do(http.MethodGet, "/v1/accounts", nil, nil)
	var out struct {
		Accounts []struct {
			ID      string `json:"id"`
			Code    string `json:"code"`
			Balance int64  `json:"balance_minor"`
		} `json:"accounts"`
	}
	decode(c.t, res, &out)
	if res.StatusCode != 200 {
		c.t.Fatalf("accounts %d", res.StatusCode)
	}
	for _, a := range out.Accounts {
		if a.Code == code {
			return a.ID, a.Balance
		}
	}
	c.t.Fatalf("account %s missing", code)
	return "", 0
}

func (c *apiClient) customer(name, phone string) string {
	c.t.Helper()
	res := c.do(http.MethodPost, "/v1/customers", map[string]any{"name": name, "phone": phone}, nil)
	var out struct {
		ID string `json:"id"`
	}
	decode(c.t, res, &out)
	if res.StatusCode != http.StatusCreated || out.ID == "" {
		c.t.Fatalf("customer %d %s", res.StatusCode, out.ID)
	}
	return out.ID
}

func (c *apiClient) customerBalance(id string) int64 {
	c.t.Helper()
	res := c.do(http.MethodGet, "/v1/customers/"+id, nil, nil)
	var out struct {
		Balance int64 `json:"balance_minor"`
	}
	decode(c.t, res, &out)
	if res.StatusCode != 200 {
		c.t.Fatalf("get customer %d", res.StatusCode)
	}
	return out.Balance
}

func TestDiscountChangesVATAndMatchesBooks(t *testing.T) {
	sh := openShop(t, 750)
	sh.admin.createProduct("SHEA", "Shea", 450_000, 2)
	res := sh.admin.do(http.MethodPost, "/v1/stock/receipts", map[string]any{
		"outlet_id": sh.ikeja, "sku": "SHEA", "qty": 10, "unit_cost_minor": 100_000,
	}, nil)
	if res.StatusCode != 200 {
		var body map[string]any
		decode(t, res, &body)
		t.Fatalf("receipt %d %v", res.StatusCode, body)
	}
	decode(t, res, nil)

	h := http.Header{}
	h.Set("Idempotency-Key", "disc-"+uuid.NewString())
	sale := sh.admin.do(http.MethodPost, "/v1/orders", map[string]any{
		"outlet_id": sh.ikeja, "payment_method": "cash", "discount_minor": 50_000,
		"lines": []map[string]any{{"sku": "SHEA", "qty": 1}},
	}, h)
	var cash map[string]any
	decode(t, sale, &cash)
	if sale.StatusCode != 201 {
		t.Fatalf("discounted sale %d %v", sale.StatusCode, cash)
	}
	if cash["total_minor"].(float64) != 400_000 || cash["vat_minor"].(float64) != 27_906 || cash["net_minor"].(float64) != 372_094 {
		t.Fatalf("vat on discounted payable %v", cash)
	}
	if cash["discount_minor"].(float64) != 50_000 || cash["cost_minor"].(float64) != 100_000 {
		t.Fatalf("discount or cost %v", cash)
	}
	if cash["vat_minor"].(float64)+cash["net_minor"].(float64) != cash["total_minor"].(float64) {
		t.Fatal("net plus vat must equal the payable")
	}

	cust := sh.admin.customer("Ada", "08030001111")
	th := http.Header{}
	th.Set("Idempotency-Key", "top-"+uuid.NewString())
	top := sh.admin.do(http.MethodPost, "/v1/customers/"+cust+"/topups", map[string]any{
		"outlet_id": sh.ikeja, "amount_minor": 450_000,
	}, th)
	if top.StatusCode != 201 {
		var body map[string]any
		decode(t, top, &body)
		t.Fatalf("topup %d %v", top.StatusCode, body)
	}
	decode(t, top, nil)
	wh := http.Header{}
	wh.Set("Idempotency-Key", "wall-"+uuid.NewString())
	walletSale := sh.admin.do(http.MethodPost, "/v1/orders", map[string]any{
		"outlet_id": sh.ikeja, "payment_method": "wallet", "customer_id": cust,
		"lines": []map[string]any{{"sku": "SHEA", "qty": 1}},
	}, wh)
	var paid map[string]any
	decode(t, walletSale, &paid)
	if walletSale.StatusCode != 201 || paid["total_minor"].(float64) != 450_000 || paid["cost_minor"].(float64) != 100_000 {
		t.Fatalf("wallet sale %d %v", walletSale.StatusCode, paid)
	}

	day := sh.admin.do(http.MethodGet, "/v1/reports/daily-sales?outlet_id="+sh.ikeja, nil, nil)
	var report map[string]any
	decode(t, day, &report)
	if day.StatusCode != 200 || report["matches_books"] != true || report["total_minor"].(float64) != 850_000 {
		t.Fatalf("daily %d %v", day.StatusCode, report)
	}
	if sh.admin.identity()["holds"] != true {
		t.Fatal("identity broken")
	}
}

func TestWalletTopUpPaysForSaleAndRefundRestores(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("SOAP", "Soap", 450_000, 1)
	sh.admin.receive(sh.ikeja, "SOAP", 5)
	phone := "0803" + uuid.NewString()[:8]
	id := sh.admin.customer("Chidi", phone)
	dup := sh.admin.do(http.MethodPost, "/v1/customers", map[string]any{"name": "Other", "phone": phone}, nil)
	var dupBody map[string]any
	decode(t, dup, &dupBody)
	if dup.StatusCode != 409 || errCode(dupBody) != "phone_taken" {
		t.Fatalf("duplicate phone %d %v", dup.StatusCode, dupBody)
	}

	key := "purse-" + uuid.NewString()
	h := http.Header{}
	h.Set("Idempotency-Key", key)
	top := sh.admin.do(http.MethodPost, "/v1/customers/"+id+"/topups", map[string]any{
		"outlet_id": sh.ikeja, "amount_minor": 1_000_000,
	}, h)
	if top.StatusCode != 201 {
		var body map[string]any
		decode(t, top, &body)
		t.Fatalf("topup %d %v", top.StatusCode, body)
	}
	decode(t, top, nil)
	again := sh.admin.do(http.MethodPost, "/v1/customers/"+id+"/topups", map[string]any{
		"outlet_id": sh.ikeja, "amount_minor": 1_000_000,
	}, h)
	if again.StatusCode != 201 {
		t.Fatalf("replay %d", again.StatusCode)
	}
	decode(t, again, nil)
	if sh.admin.customerBalance(id) != 1_000_000 {
		t.Fatalf("balance %d", sh.admin.customerBalance(id))
	}
	_, walletBal := sh.admin.accountByCode("2200")
	if walletBal != 1_000_000 {
		t.Fatalf("wallet gl %d", walletBal)
	}

	saleKey := "spend-" + uuid.NewString()
	shdr := http.Header{}
	shdr.Set("Idempotency-Key", saleKey)
	sale := sh.admin.do(http.MethodPost, "/v1/orders", map[string]any{
		"outlet_id": sh.ikeja, "payment_method": "wallet", "customer_id": id,
		"lines": []map[string]any{{"sku": "SOAP", "qty": 1}},
	}, shdr)
	var sold map[string]any
	decode(t, sale, &sold)
	if sale.StatusCode != 201 {
		t.Fatalf("sale %d %v", sale.StatusCode, sold)
	}
	replay := sh.admin.do(http.MethodPost, "/v1/orders", map[string]any{
		"outlet_id": sh.ikeja, "payment_method": "wallet", "customer_id": id,
		"lines": []map[string]any{{"sku": "SOAP", "qty": 1}},
	}, shdr)
	var replayed map[string]any
	decode(t, replay, &replayed)
	if replay.StatusCode != 201 || replayed["id"] != sold["id"] {
		t.Fatalf("wallet replay %d %v", replay.StatusCode, replayed)
	}
	if sh.admin.customerBalance(id) != 550_000 || sh.admin.stockAvailable(sh.ikeja, "SOAP") != 4 {
		t.Fatalf("after sale balance %d stock %d", sh.admin.customerBalance(id), sh.admin.stockAvailable(sh.ikeja, "SOAP"))
	}

	over := http.Header{}
	over.Set("Idempotency-Key", "over-"+uuid.NewString())
	short := sh.admin.do(http.MethodPost, "/v1/orders", map[string]any{
		"outlet_id": sh.ikeja, "payment_method": "wallet", "customer_id": id,
		"lines": []map[string]any{{"sku": "SOAP", "qty": 2}},
	}, over)
	var shortBody map[string]any
	decode(t, short, &shortBody)
	if short.StatusCode != 409 || errCode(shortBody) != "insufficient_wallet" {
		t.Fatalf("overdraw %d %v", short.StatusCode, shortBody)
	}
	if sh.admin.stockAvailable(sh.ikeja, "SOAP") != 4 || sh.admin.customerBalance(id) != 550_000 {
		t.Fatal("overdraw changed stock or balance")
	}

	rh := http.Header{}
	rh.Set("Idempotency-Key", "refund-"+uuid.NewString())
	refund := sh.admin.do(http.MethodPost, "/v1/orders/"+sold["id"].(string)+"/refund", map[string]any{}, rh)
	var refunded map[string]any
	decode(t, refund, &refunded)
	if refund.StatusCode != 201 || refunded["status"] != "refunded" {
		t.Fatalf("refund %d %v", refund.StatusCode, refunded)
	}
	if sh.admin.customerBalance(id) != 1_000_000 || sh.admin.stockAvailable(sh.ikeja, "SOAP") != 5 {
		t.Fatalf("restored balance %d stock %d", sh.admin.customerBalance(id), sh.admin.stockAvailable(sh.ikeja, "SOAP"))
	}
	_, walletBal = sh.admin.accountByCode("2200")
	if walletBal != 1_000_000 {
		t.Fatalf("wallet gl after refund %d", walletBal)
	}

	email := "cashier-" + uuid.NewString()[:8] + "@test.local"
	sh.admin.addStaff(email, "cashier", sh.ikeja)
	cashier := loginEmail(t, email)
	cid := cashier.customer("Cashier Customer", "0803999"+uuid.NewString()[:6])
	ch := http.Header{}
	ch.Set("Idempotency-Key", "csh-"+uuid.NewString())
	ctop := cashier.do(http.MethodPost, "/v1/customers/"+cid+"/topups", map[string]any{
		"outlet_id": sh.ikeja, "amount_minor": 50_000,
	}, ch)
	if ctop.StatusCode != 201 {
		var body map[string]any
		decode(t, ctop, &body)
		t.Fatalf("cashier topup %d %v", ctop.StatusCode, body)
	}
	decode(t, ctop, nil)
	if cashier.customerBalance(cid) != 50_000 {
		t.Fatal("cashier topup balance")
	}
	_, walletBal = sh.admin.accountByCode("2200")
	if walletBal != 1_050_000 {
		t.Fatalf("wallet gl with two customers %d", walletBal)
	}
	if sh.admin.identity()["holds"] != true {
		t.Fatal("identity broken")
	}
}

func TestReceiptAverageAndSalePostsCOGS(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("OIL", "Oil", 100_000, 1)
	sh.admin.createProduct("FREE", "Free sample", 20_000, 0)
	first := sh.admin.do(http.MethodPost, "/v1/stock/receipts", map[string]any{
		"outlet_id": sh.ikeja, "sku": "OIL", "qty": 4, "unit_cost_minor": 100_000,
	}, nil)
	var got map[string]any
	decode(t, first, &got)
	if first.StatusCode != 200 || got["avg_cost_minor"].(float64) != 100_000 {
		t.Fatalf("first receipt %d %v", first.StatusCode, got)
	}
	second := sh.admin.do(http.MethodPost, "/v1/stock/receipts", map[string]any{
		"outlet_id": sh.ikeja, "sku": "OIL", "qty": 4, "unit_cost_minor": 300_000,
	}, nil)
	decode(t, second, &got)
	if second.StatusCode != 200 || got["avg_cost_minor"].(float64) != 200_000 {
		t.Fatalf("second receipt %d %v", second.StatusCode, got)
	}
	sh.admin.receive(sh.ikeja, "FREE", 3)
	if stockField(t, sh.admin, sh.ikeja, "OIL", "value_minor") != 1_600_000 {
		t.Fatalf("stock value %d", stockField(t, sh.admin, sh.ikeja, "OIL", "value_minor"))
	}
	_, inv := sh.admin.accountByCode("1200")
	_, goods := sh.admin.accountByCode("2150")
	if inv != 1_600_000 || goods != 1_600_000 {
		t.Fatalf("inventory %d goods %d", inv, goods)
	}

	res, sold := sh.admin.order("cogs-"+uuid.NewString(), sh.ikeja, "cash", "OIL", 2)
	if res.StatusCode != 201 || sold["cost_minor"].(float64) != 400_000 {
		t.Fatalf("cogs sale %d %v", res.StatusCode, sold)
	}
	jid, _ := sold["journal_id"].(string)
	jres := sh.admin.do(http.MethodGet, "/v1/journals/"+jid, nil, nil)
	var journal struct {
		Postings []struct {
			Direction string `json:"direction"`
			Amount    int64  `json:"amount_minor"`
		} `json:"postings"`
	}
	decode(t, jres, &journal)
	if jres.StatusCode != 200 || len(journal.Postings) != 4 {
		t.Fatalf("journal %d lines %d", jres.StatusCode, len(journal.Postings))
	}
	var sawCost bool
	for _, p := range journal.Postings {
		if p.Direction == "debit" && p.Amount == 400_000 {
			sawCost = true
		}
	}
	if !sawCost {
		t.Fatalf("missing cogs debit %v", journal.Postings)
	}

	free, freeBody := sh.admin.order("free-"+uuid.NewString(), sh.ikeja, "cash", "FREE", 1)
	if free.StatusCode != 201 || freeBody["cost_minor"].(float64) != 0 {
		t.Fatalf("zero cost %d %v", free.StatusCode, freeBody)
	}
	fj := sh.admin.do(http.MethodGet, "/v1/journals/"+freeBody["journal_id"].(string), nil, nil)
	var plain struct {
		Postings []struct {
			Amount int64 `json:"amount_minor"`
		} `json:"postings"`
	}
	decode(t, fj, &plain)
	if len(plain.Postings) != 2 {
		t.Fatalf("zero-cost journal should not post cogs, lines %d", len(plain.Postings))
	}

	products := sh.admin.do(http.MethodGet, "/v1/products", nil, nil)
	var list struct {
		Products []struct {
			ID   string `json:"id"`
			SKU  string `json:"sku"`
			Cost int64  `json:"avg_cost_minor"`
		} `json:"products"`
	}
	decode(t, products, &list)
	var oilID string
	for _, p := range list.Products {
		if p.SKU == "OIL" {
			oilID = p.ID
			if p.Cost != 200_000 {
				t.Fatalf("listed cost %d", p.Cost)
			}
		}
	}
	if oilID == "" {
		t.Fatal("oil missing")
	}
	patch := sh.admin.do(http.MethodPatch, "/v1/products/"+oilID, map[string]any{"avg_cost_minor": 250_000}, nil)
	var patched map[string]any
	decode(t, patch, &patched)
	if patch.StatusCode != 200 || patched["avg_cost_minor"].(float64) != 250_000 {
		t.Fatalf("patch %d %v", patch.StatusCode, patched)
	}
	_, inv = sh.admin.accountByCode("1200")
	// 1,600,000 received, 400,000 sold, then 50,000 * 6 units still on hand.
	if inv != 1_500_000 {
		t.Fatalf("inventory after revalue %d", inv)
	}
	if stockField(t, sh.admin, sh.ikeja, "OIL", "on_hand")*250_000 != int(inv) {
		t.Fatal("layer and inventory account diverged")
	}
	if sh.admin.identity()["holds"] != true {
		t.Fatal("identity broken")
	}
}
