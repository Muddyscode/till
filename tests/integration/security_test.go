package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Muddyscode/till/internal/config"
	"github.com/Muddyscode/till/internal/httpserver"
	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
)

var phoneSeq atomic.Int64

// uniquePhone returns a valid Nigerian mobile number that has not been used in this run.
func uniquePhone() string {
	n := (time.Now().UnixNano()/1000 + phoneSeq.Add(1)) % 100_000_000
	return fmt.Sprintf("080%08d", n)
}

func startAPICfg(t *testing.T, mutate func(*config.Config)) (*apiClient, *httpserver.Server) {
	t.Helper()
	cfg := config.Config{
		JWTSecret:   "test-secret-must-be-32-bytes-ok!",
		HoldMinutes: 15,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	api := httpserver.New(cfg, store.New(testPool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(api.Router())
	t.Cleanup(srv.Close)
	return &apiClient{t: t, srv: srv}, api
}

func (c *apiClient) as(token string) *apiClient { return &apiClient{t: c.t, srv: c.srv, token: token} }

func (c *apiClient) login(email, password string) (*http.Response, map[string]any) {
	c.t.Helper()
	res := c.do(http.MethodPost, "/v1/auth/login", map[string]string{"email": email, "password": password}, nil)
	var out map[string]any
	decode(c.t, res, &out)
	return res, out
}

func (c *apiClient) status(method, path string, body any) int {
	c.t.Helper()
	res := c.do(method, path, body, nil)
	decode(c.t, res, nil)
	return res.StatusCode
}

func (c *apiClient) staffID(email string) string {
	c.t.Helper()
	res := c.do(http.MethodGet, "/v1/staff", nil, nil)
	var out struct {
		Staff []struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"staff"`
	}
	decode(c.t, res, &out)
	for _, s := range out.Staff {
		if s.Email == email {
			return s.ID
		}
	}
	c.t.Fatalf("staff %s not listed", email)
	return ""
}

// newCashier adds a cashier at the outlet and returns the client on a fresh server plus the email.
func (sh *shop) newStaff(role, outlet string) (string, string) {
	sh.t.Helper()
	email := fmt.Sprintf("%s-%s@test.local", role, uuid.NewString()[:8])
	sh.admin.addStaff(email, role, outlet)
	return email, sh.admin.staffID(email)
}

func TestLoginLockoutAfterTenFailures(t *testing.T) {
	sh := openShop(t, 0)
	email, id := sh.newStaff("cashier", sh.ikeja)
	c, _ := startAPICfg(t, func(cfg *config.Config) { cfg.LoginRatePerIP, cfg.LoginRatePerEmail = 1000, 1000 })

	for i := 0; i < 10; i++ {
		res, body := c.login(email, "wrong-password-xx")
		if res.StatusCode != 401 || errCode(body) != "unauthorized" {
			t.Fatalf("attempt %d: %d %v", i, res.StatusCode, body)
		}
	}
	// The correct password is refused while locked.
	res, body := c.login(email, "change-me-now-12")
	if res.StatusCode != 429 || errCode(body) != "account_locked" || res.Header.Get("Retry-After") == "" {
		t.Fatalf("locked: %d %v retry=%q", res.StatusCode, body, res.Header.Get("Retry-After"))
	}
	var count int
	var locked *time.Time
	if err := testPool.QueryRow(context.Background(), `SELECT failed_login_count, locked_until FROM staff WHERE id=$1`, id).Scan(&count, &locked); err != nil {
		t.Fatal(err)
	}
	if count != 10 || locked == nil || !locked.After(time.Now()) {
		t.Fatalf("persisted state: count=%d locked=%v", count, locked)
	}
	// Lockout expiry: move it into the past; a good login then clears the counter.
	if _, err := testPool.Exec(context.Background(), `UPDATE staff SET locked_until = now() - interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if res, body := c.login(email, "change-me-now-12"); res.StatusCode != 200 {
		t.Fatalf("login after lock expiry: %d %v", res.StatusCode, body)
	}
	if err := testPool.QueryRow(context.Background(), `SELECT failed_login_count, locked_until FROM staff WHERE id=$1`, id).Scan(&count, &locked); err != nil {
		t.Fatal(err)
	}
	if count != 0 || locked != nil {
		t.Fatalf("counter not cleared: %d %v", count, locked)
	}
}

func TestLoginFailuresOutsideWindowDoNotLock(t *testing.T) {
	sh := openShop(t, 0)
	email, id := sh.newStaff("cashier", sh.ikeja)
	// High rate limits: this test is about the lockout counter, not the limiter.
	c, _ := startAPICfg(t, func(cfg *config.Config) { cfg.LoginRatePerIP, cfg.LoginRatePerEmail = 1000, 1000 })
	for i := 0; i < 5; i++ {
		c.login(email, "wrong-password-xx")
	}
	// Pretend those failures were 20 minutes ago.
	if _, err := testPool.Exec(context.Background(), `UPDATE staff SET last_failed_login_at = now() - interval '20 minutes' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		c.login(email, "wrong-password-xx")
	}
	var count int
	if err := testPool.QueryRow(context.Background(), `SELECT failed_login_count FROM staff WHERE id=$1`, id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 6 {
		t.Fatalf("window should have reset: count=%d", count)
	}
	if res, _ := c.login(email, "change-me-now-12"); res.StatusCode != 200 {
		t.Fatalf("not locked at 6 failures: %d", res.StatusCode)
	}
}

func TestLoginRateLimitPerIPAndMetrics(t *testing.T) {
	c, _ := startAPICfg(t, func(cfg *config.Config) { cfg.LoginRatePerIP = 3 })
	for i := 0; i < 3; i++ {
		res, body := c.login("nobody@test.local", "wrong-password-xx")
		if res.StatusCode != 401 {
			t.Fatalf("attempt %d: %d %v", i, res.StatusCode, body)
		}
	}
	res, body := c.login("nobody@test.local", "wrong-password-xx")
	if res.StatusCode != 429 || errCode(body) != "rate_limited" || res.Header.Get("Retry-After") == "" {
		t.Fatalf("expected 429: %d %v", res.StatusCode, body)
	}
	m := c.get("/metrics")
	for _, want := range []string{
		"login_failures_total 3",
		`rate_limited_total{scope="ip"} 1`,
		`http_requests_total{route="/v1/auth/login",status="401"} 3`,
		`http_requests_total{route="/v1/auth/login",status="429"} 1`,
		"http_request_duration_seconds_count{route=\"/v1/auth/login\"} 4",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics missing %q\n%s", want, m)
		}
	}
}

func TestLoginRateLimitPerEmail(t *testing.T) {
	c, _ := startAPICfg(t, func(cfg *config.Config) { cfg.LoginRatePerIP = 1000 })
	email := "ghost-" + uuid.NewString()[:8] + "@test.local"
	limited := 0
	for i := 0; i < 20; i++ {
		res, _ := c.login(email, "wrong-password-xx")
		if res.StatusCode == 429 {
			limited++
		}
	}
	if limited == 0 {
		t.Fatal("per-email limiter never engaged")
	}
}

func TestUnknownEmailLooksLikeWrongPassword(t *testing.T) {
	sh := openShop(t, 0)
	email, _ := sh.newStaff("cashier", sh.ikeja)
	c, _ := startAPICfg(t, nil)
	r1, b1 := c.login(email, "wrong-password-xx")
	r2, b2 := c.login("ghost-"+uuid.NewString()[:8]+"@test.local", "wrong-password-xx")
	if r1.StatusCode != 401 || r2.StatusCode != 401 || fmt.Sprint(b1) != fmt.Sprint(b2) {
		t.Fatalf("responses differ: %d %v / %d %v", r1.StatusCode, b1, r2.StatusCode, b2)
	}
}

func TestLoginBodyTooLarge(t *testing.T) {
	c, _ := startAPICfg(t, nil)
	res := c.doRaw(http.MethodPost, "/v1/auth/login", `{"email":"a@b.c","password":"`+strings.Repeat("x", 8000)+`"}`, nil)
	var body map[string]any
	decode(t, res, &body)
	if res.StatusCode != 413 || errCode(body) != "request_too_large" {
		t.Fatalf("%d %v", res.StatusCode, body)
	}
}

func TestTokenTTLAndClaims(t *testing.T) {
	sh := openShop(t, 0)
	email, _ := sh.newStaff("cashier", sh.ikeja)
	c, _ := startAPICfg(t, func(cfg *config.Config) { cfg.TokenTTL = 2 * time.Hour })
	_, body := c.login(email, "change-me-now-12")
	exp, err := time.Parse(time.RFC3339, body["expires_at"].(string))
	if err != nil || time.Until(exp) > 2*time.Hour+time.Minute || time.Until(exp) < 119*time.Minute {
		t.Fatalf("ttl: %v %v", exp, err)
	}
	// Default is 8h.
	c2, _ := startAPICfg(t, nil)
	_, body = c2.login(email, "change-me-now-12")
	exp, _ = time.Parse(time.RFC3339, body["expires_at"].(string))
	if time.Until(exp) < 7*time.Hour+50*time.Minute || time.Until(exp) > 8*time.Hour+time.Minute {
		t.Fatalf("default ttl: %v", exp)
	}
}

func TestLogoutRevokesToken(t *testing.T) {
	sh := openShop(t, 0)
	email, _ := sh.newStaff("cashier", sh.ikeja)
	c := loginEmail(t, email)
	old := c.token
	if c.status(http.MethodGet, "/v1/auth/me", nil) != 200 {
		t.Fatal("me before logout")
	}
	if c.status(http.MethodPost, "/v1/auth/logout", nil) != 200 {
		t.Fatal("logout")
	}
	if got := c.as(old).status(http.MethodGet, "/v1/auth/me", nil); got != 401 {
		t.Fatalf("token still valid after logout: %d", got)
	}
	// A new login works and gets a working token.
	c2 := loginEmail(t, email)
	if c2.status(http.MethodGet, "/v1/auth/me", nil) != 200 {
		t.Fatal("fresh token should work")
	}
}

func TestChangeOwnPassword(t *testing.T) {
	sh := openShop(t, 0)
	email, _ := sh.newStaff("cashier", sh.ikeja)
	c := loginEmail(t, email)
	old := c.token

	if got := c.status(http.MethodPost, "/v1/auth/password", map[string]string{"current_password": "nope-nope-nope", "new_password": "brand-new-pass-1"}); got != 401 {
		t.Fatalf("wrong current: %d", got)
	}
	if got := c.status(http.MethodPost, "/v1/auth/password", map[string]string{"current_password": "change-me-now-12", "new_password": "short"}); got != 400 {
		t.Fatalf("short password: %d", got)
	}
	if got := c.status(http.MethodPost, "/v1/auth/password", map[string]string{"current_password": "change-me-now-12", "new_password": "change-me-now-12"}); got != 400 {
		t.Fatalf("same password: %d", got)
	}
	res := c.do(http.MethodPost, "/v1/auth/password", map[string]string{"current_password": "change-me-now-12", "new_password": "brand-new-pass-1"}, nil)
	var out struct {
		Token string `json:"token"`
	}
	decode(t, res, &out)
	if res.StatusCode != 200 || out.Token == "" {
		t.Fatalf("change: %d", res.StatusCode)
	}
	if got := c.as(old).status(http.MethodGet, "/v1/auth/me", nil); got != 401 {
		t.Fatalf("old token survives password change: %d", got)
	}
	if got := c.as(out.Token).status(http.MethodGet, "/v1/auth/me", nil); got != 200 {
		t.Fatalf("returned token should work: %d", got)
	}
	lc, _ := startAPICfg(t, nil)
	if r, _ := lc.login(email, "change-me-now-12"); r.StatusCode != 401 {
		t.Fatal("old password still works")
	}
	if r, _ := lc.login(email, "brand-new-pass-1"); r.StatusCode != 200 {
		t.Fatal("new password does not work")
	}
}

func TestAdminPasswordReset(t *testing.T) {
	sh := openShop(t, 0)
	email, id := sh.newStaff("cashier", sh.ikeja)
	cashier := loginEmail(t, email)
	old := cashier.token

	// A cashier cannot reset passwords.
	if got := cashier.status(http.MethodPost, "/v1/staff/"+id+"/password-reset", map[string]string{"new_password": "reset-by-admin-1"}); got != 403 {
		t.Fatalf("cashier reset: %d", got)
	}
	if got := sh.admin.status(http.MethodPost, "/v1/staff/"+id+"/password-reset", map[string]string{"new_password": "short"}); got != 400 {
		t.Fatalf("weak reset: %d", got)
	}
	if got := sh.admin.status(http.MethodPost, "/v1/staff/"+id+"/password-reset", map[string]string{"new_password": "reset-by-admin-1"}); got != 200 {
		t.Fatalf("reset: %d", got)
	}
	if got := cashier.as(old).status(http.MethodGet, "/v1/auth/me", nil); got != 401 {
		t.Fatalf("old token survives reset: %d", got)
	}
	lc, _ := startAPICfg(t, nil)
	if r, _ := lc.login(email, "reset-by-admin-1"); r.StatusCode != 200 {
		t.Fatal("new password does not work")
	}
	// Another merchant's admin gets 404.
	other := openShop(t, 0)
	if got := other.admin.status(http.MethodPost, "/v1/staff/"+id+"/password-reset", map[string]string{"new_password": "reset-by-admin-1"}); got != 404 {
		t.Fatalf("cross-merchant reset: %d", got)
	}
}

func TestAPIKeyLifecycle(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("KEYSKU", "Key product", 100_000, 0)
	sh.admin.receive(sh.ikeja, "KEYSKU", 10)

	create := func(role string, outlets ...string) map[string]any {
		res := sh.admin.do(http.MethodPost, "/v1/api-keys", map[string]any{"name": "till-1", "role": role, "outlet_ids": outlets}, nil)
		var out map[string]any
		decode(t, res, &out)
		if res.StatusCode != 201 {
			t.Fatalf("create key: %d %v", res.StatusCode, out)
		}
		return out
	}
	// Validation.
	if got := sh.admin.status(http.MethodPost, "/v1/api-keys", map[string]any{"name": "x", "role": "admin", "outlet_ids": []string{sh.ikeja}}); got != 400 {
		t.Fatalf("admin-role key: %d", got)
	}
	if got := sh.admin.status(http.MethodPost, "/v1/api-keys", map[string]any{"name": "x", "role": "cashier", "outlet_ids": []string{}}); got != 400 {
		t.Fatalf("no outlets: %d", got)
	}
	if got := sh.admin.status(http.MethodPost, "/v1/api-keys", map[string]any{"name": "x", "role": "cashier", "outlet_ids": []string{uuid.NewString()}}); got != 404 {
		t.Fatalf("foreign outlet: %d", got)
	}

	key := create("cashier", sh.ikeja)
	secret := key["secret"].(string)
	if !strings.HasPrefix(secret, "till_live_"+key["prefix"].(string)+"_") {
		t.Fatalf("secret format: %s", secret)
	}

	k := sh.admin.as(secret)
	if got := k.status(http.MethodGet, "/v1/auth/me", nil); got != 200 {
		t.Fatalf("me with key: %d", got)
	}
	// Sale at the assigned outlet works; the other outlet is refused by authorizeOutlet.
	res, out := k.order("apikey-"+uuid.NewString(), sh.ikeja, "cash", "KEYSKU", 1)
	if res.StatusCode != 201 {
		t.Fatalf("sale with key: %d %v", res.StatusCode, out)
	}
	res, out = k.order("apikey-"+uuid.NewString(), sh.sul, "cash", "KEYSKU", 1)
	if res.StatusCode != 403 || errCode(out) != "outlet_forbidden" {
		t.Fatalf("other outlet: %d %v", res.StatusCode, out)
	}
	// Role limits apply: a cashier key cannot create products, list keys or manage sessions.
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/v1/products", map[string]any{"sku": "X", "name": "x", "price_minor": 100}},
		{http.MethodGet, "/v1/api-keys", nil},
		{http.MethodPost, "/v1/auth/logout", nil},
		{http.MethodPost, "/v1/auth/password", map[string]string{"current_password": "a", "new_password": "bbbbbbbbbbbbb"}},
	} {
		if got := k.status(tc.method, tc.path, tc.body); got != 403 {
			t.Errorf("%s %s with cashier key: %d", tc.method, tc.path, got)
		}
	}
	// Tampered secret and unknown prefix fail.
	bad := secret[:len(secret)-1] + "0"
	if secret[len(secret)-1] == '0' {
		bad = secret[:len(secret)-1] + "1"
	}
	if got := sh.admin.as(bad).status(http.MethodGet, "/v1/auth/me", nil); got != 401 {
		t.Fatalf("tampered key: %d", got)
	}
	if got := sh.admin.as("till_live_00000000_"+strings.Repeat("0", 48)).status(http.MethodGet, "/v1/auth/me", nil); got != 401 {
		t.Fatalf("unknown key: %d", got)
	}

	// Listing never shows the secret, shows use, and hides the service staff row.
	lres := sh.admin.do(http.MethodGet, "/v1/api-keys", nil, nil)
	var list struct {
		Keys []map[string]any `json:"api_keys"`
	}
	decode(t, lres, &list)
	if len(list.Keys) != 1 || list.Keys[0]["secret"] != nil || list.Keys[0]["last_used_at"] == nil {
		t.Fatalf("list: %v", list.Keys)
	}
	sres := sh.admin.do(http.MethodGet, "/v1/staff", nil, nil)
	var staff struct {
		Staff []map[string]any `json:"staff"`
	}
	decode(t, sres, &staff)
	for _, s := range staff.Staff {
		if strings.Contains(fmt.Sprint(s["email"]), "api-key.invalid") {
			t.Fatal("service staff leaked into /v1/staff")
		}
	}
	// The sale is attributed to the key's backing staff row in the audit trail.
	ares := sh.admin.do(http.MethodGet, "/v1/audit?limit=50", nil, nil)
	var audit struct {
		Events []map[string]any `json:"events"`
	}
	decode(t, ares, &audit)
	found := false
	for _, e := range audit.Events {
		if e["staff_id"] == key["staff_id"] {
			found = true
		}
	}
	if !found {
		t.Fatal("no audit event attributed to the key's staff_id")
	}

	// Manager key may receive stock; revoked key stops at once.
	mkey := create("manager", sh.ikeja)
	mk := sh.admin.as(mkey["secret"].(string))
	if got := mk.status(http.MethodPost, "/v1/stock/receipts", map[string]any{"outlet_id": sh.ikeja, "sku": "KEYSKU", "qty": 1}); got != 200 {
		t.Fatalf("manager key receipt: %d", got)
	}
	if got := sh.admin.status(http.MethodDelete, "/v1/api-keys/"+key["id"].(string), nil); got != 200 {
		t.Fatalf("revoke: %d", got)
	}
	if got := k.status(http.MethodGet, "/v1/auth/me", nil); got != 401 {
		t.Fatalf("revoked key still works: %d", got)
	}
	if got := mk.status(http.MethodGet, "/v1/auth/me", nil); got != 200 {
		t.Fatalf("other key affected by revoke: %d", got)
	}
	// Other merchants cannot see or revoke it.
	other := openShop(t, 0)
	if got := other.admin.status(http.MethodDelete, "/v1/api-keys/"+mkey["id"].(string), nil); got != 404 {
		t.Fatalf("cross-merchant revoke: %d", got)
	}
	if got := sh.admin.status(http.MethodDelete, "/v1/api-keys/"+uuid.NewString(), nil); got != 404 {
		t.Fatalf("unknown revoke: %d", got)
	}
}

func TestProductPriceAndCostAreAdminOnly(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("PRICEY", "Pricey", 100_000, 1)
	mEmail, _ := sh.newStaff("manager", sh.ikeja)
	mgr := loginEmail(t, mEmail)
	cEmail, _ := sh.newStaff("cashier", sh.ikeja)
	cash := loginEmail(t, cEmail)

	pres := sh.admin.do(http.MethodGet, "/v1/products", nil, nil)
	var plist struct {
		Products []struct {
			ID  string `json:"id"`
			SKU string `json:"sku"`
		} `json:"products"`
	}
	decode(t, pres, &plist)
	id := ""
	for _, p := range plist.Products {
		if p.SKU == "PRICEY" {
			id = p.ID
		}
	}
	if id == "" {
		t.Fatal("product not listed")
	}

	if got := mgr.status(http.MethodPost, "/v1/products", map[string]any{"sku": "MGR1", "name": "m", "price_minor": 5000}); got != 403 {
		t.Fatalf("manager create: %d", got)
	}
	for _, body := range []string{`{"price_minor":120000}`, `{"avg_cost_minor":50000}`, `{"PRICE_MINOR":120000}`, `{"name":"x","Avg_Cost_Minor":1}`} {
		res := mgr.doRaw(http.MethodPatch, "/v1/products/"+id, body, nil)
		decode(t, res, nil)
		if res.StatusCode != 403 {
			t.Errorf("manager patch %s: %d", body, res.StatusCode)
		}
	}
	// Non-price fields remain open to managers.
	res := mgr.doRaw(http.MethodPatch, "/v1/products/"+id, `{"name":"Pricey Renamed","reorder_level":3}`, nil)
	decode(t, res, nil)
	if res.StatusCode != 200 {
		t.Fatalf("manager rename: %d", res.StatusCode)
	}
	if got := cash.status(http.MethodPatch, "/v1/products/"+id, map[string]any{"name": "z"}); got != 403 {
		t.Fatalf("cashier patch: %d", got)
	}
	res = sh.admin.doRaw(http.MethodPatch, "/v1/products/"+id, `{"price_minor":120000}`, nil)
	decode(t, res, nil)
	if res.StatusCode != 200 {
		t.Fatalf("admin price: %d", res.StatusCode)
	}
}

func (c *apiClient) customerList(query string) (int, []map[string]any) {
	c.t.Helper()
	res := c.do(http.MethodGet, "/v1/customers"+query, nil, nil)
	var out struct {
		Customers []map[string]any `json:"customers"`
	}
	decode(c.t, res, &out)
	return res.StatusCode, out.Customers
}

func TestCustomerPrivacyAndSearch(t *testing.T) {
	sh := openShop(t, 0)
	mEmail, _ := sh.newStaff("manager", sh.ikeja)
	mgr := loginEmail(t, mEmail)
	cEmail, _ := sh.newStaff("cashier", sh.ikeja)
	cash := loginEmail(t, cEmail)

	// Normalisation: any common Nigerian spelling becomes E.164.
	res := sh.admin.do(http.MethodPost, "/v1/customers", map[string]any{"name": "Folake Adeyemi", "phone": "0803 123 4567"}, nil)
	var created map[string]any
	decode(t, res, &created)
	if res.StatusCode != 201 || created["phone"] != "+2348031234567" {
		t.Fatalf("create: %d %v", res.StatusCode, created)
	}
	// The same number written differently is a duplicate.
	res = sh.admin.do(http.MethodPost, "/v1/customers", map[string]any{"name": "Dup", "phone": "+234 803 123 4567"}, nil)
	decode(t, res, nil)
	if res.StatusCode != 409 {
		t.Fatalf("duplicate in another format: %d", res.StatusCode)
	}
	res = sh.admin.do(http.MethodPost, "/v1/customers", map[string]any{"name": "Bad", "phone": "12345"}, nil)
	var bad map[string]any
	decode(t, res, &bad)
	if res.StatusCode != 400 || errCode(bad) != "invalid_phone" {
		t.Fatalf("invalid phone: %d %v", res.StatusCode, bad)
	}
	sh.admin.customer("Chinedu Obi", uniquePhone())

	// Cashiers must search (3+ chars) and only see a masked phone.
	if code, _ := cash.customerList(""); code != 400 {
		t.Fatalf("cashier list without q: %d", code)
	}
	if code, _ := cash.customerList("?q=Fo"); code != 400 {
		t.Fatalf("cashier short q: %d", code)
	}
	code, list := cash.customerList("?q=folake")
	if code != 200 || len(list) != 1 || list[0]["phone"] != "0803****567" {
		t.Fatalf("cashier name search: %d %v", code, list)
	}
	// Phone search accepts local, international and partial forms.
	for _, q := range []string{"08031234567", "%2B2348031234567", "0803123", "803123"} {
		code, list = cash.customerList("?q=" + q)
		if code != 200 || len(list) != 1 || list[0]["name"] != "Folake Adeyemi" {
			t.Errorf("phone search %q: %d %v", q, code, list)
		}
	}
	// Cashier-created and fetched-by-id customers are masked too.
	cid := created["id"].(string)
	gres := cash.do(http.MethodGet, "/v1/customers/"+cid, nil, nil)
	var got map[string]any
	decode(t, gres, &got)
	if got["phone"] != "0803****567" {
		t.Fatalf("cashier get: %v", got)
	}
	// Managers and admins see the full number, and q is optional for them.
	for name, c := range map[string]*apiClient{"manager": mgr, "admin": sh.admin} {
		code, list = c.customerList("")
		if code != 200 || len(list) != 2 {
			t.Fatalf("%s list: %d %v", name, code, list)
		}
		code, list = c.customerList("?q=folake&limit=1")
		if code != 200 || len(list) != 1 || list[0]["phone"] != "+2348031234567" {
			t.Fatalf("%s search: %d %v", name, code, list)
		}
	}
	if code, list = sh.admin.customerList("?limit=1"); code != 200 || len(list) != 1 {
		t.Fatalf("limit: %d %v", code, list)
	}
	// LIKE wildcards in q are literals.
	if code, list = sh.admin.customerList("?q=%25"); code != 200 || len(list) != 0 {
		t.Fatalf("wildcard q matched %v", list)
	}
}

func (c *apiClient) get(path string) string {
	c.t.Helper()
	res := c.do(http.MethodGet, path, nil, nil)
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return string(b)
}

func TestHTTPHygiene(t *testing.T) {
	sh := openShop(t, 0)

	// JSON envelopes for 404 and 405.
	res := sh.admin.do(http.MethodGet, "/v1/nope", nil, nil)
	var body map[string]any
	decode(t, res, &body)
	if res.StatusCode != 404 || errCode(body) != "not_found" || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("404: %d %v %s", res.StatusCode, body, res.Header.Get("Content-Type"))
	}
	res = sh.admin.do(http.MethodDelete, "/v1/auth/me", nil, nil)
	body = nil
	decode(t, res, &body)
	if res.StatusCode != 405 || errCode(body) != "method_not_allowed" {
		t.Fatalf("405: %d %v", res.StatusCode, body)
	}

	// Request IDs: echoed when sane, generated otherwise; no-store on /v1.
	h := http.Header{}
	h.Set("X-Request-ID", "client-req-123")
	res = sh.admin.do(http.MethodGet, "/v1/auth/me", nil, h)
	decode(t, res, nil)
	if res.Header.Get("X-Request-ID") != "client-req-123" {
		t.Fatalf("request id not echoed: %q", res.Header.Get("X-Request-ID"))
	}
	if res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("cache-control: %q", res.Header.Get("Cache-Control"))
	}
	h.Set("X-Request-ID", "bad id with spaces\t")
	res = sh.admin.do(http.MethodGet, "/livez", nil, h)
	decode(t, res, nil)
	if id := res.Header.Get("X-Request-ID"); id == "" || strings.Contains(id, " ") {
		t.Fatalf("generated id: %q", id)
	}

	// 413 on oversized bodies; unknown fields rejected on accounts.
	big := `{"code":"1000","name":"` + strings.Repeat("a", 1100000) + `","type":"asset"}`
	res = sh.admin.doRaw(http.MethodPost, "/v1/accounts", big, nil)
	body = nil
	decode(t, res, &body)
	if res.StatusCode != 413 || errCode(body) != "request_too_large" {
		t.Fatalf("413: %d %v", res.StatusCode, body)
	}
	res = sh.admin.doRaw(http.MethodPost, "/v1/accounts", `{"code":"1000","name":"Cash","type":"asset","surprise":true}`, nil)
	body = nil
	decode(t, res, &body)
	if res.StatusCode != 400 || errCode(body) != "invalid_json" {
		t.Fatalf("unknown field: %d %v", res.StatusCode, body)
	}
}

func TestCORSAllowlist(t *testing.T) {
	preflight := func(c *apiClient, origin string) *http.Response {
		req, _ := http.NewRequest(http.MethodOptions, c.srv.URL+"/v1/orders", bytes.NewReader(nil))
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "POST")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}
	on, _ := startAPICfg(t, func(cfg *config.Config) { cfg.CORSOrigins = []string{"https://admin.example.com"} })
	res := preflight(on, "https://admin.example.com")
	if res.StatusCode != 204 || res.Header.Get("Access-Control-Allow-Origin") != "https://admin.example.com" ||
		!strings.Contains(res.Header.Get("Access-Control-Allow-Headers"), "Idempotency-Key") {
		t.Fatalf("allowed origin: %d %v", res.StatusCode, res.Header)
	}
	res = preflight(on, "https://evil.example.com")
	if res.Header.Get("Access-Control-Allow-Origin") != "" || res.StatusCode == 204 {
		t.Fatalf("disallowed origin: %d %v", res.StatusCode, res.Header)
	}
	off, _ := startAPICfg(t, nil)
	res = preflight(off, "https://admin.example.com")
	if res.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("CORS must be off when no origins are configured")
	}
}

func TestLivenessReadinessAndSweeperHeartbeat(t *testing.T) {
	c, api := startAPICfg(t, nil)
	if got := c.status(http.MethodGet, "/livez", nil); got != 200 {
		t.Fatalf("livez %d", got)
	}
	if got := c.status(http.MethodGet, "/readyz", nil); got != 200 {
		t.Fatalf("readyz without sweeper check: %d", got)
	}
	api.ExpectSweeper(100 * time.Millisecond)
	if got := c.status(http.MethodGet, "/readyz", nil); got != 200 {
		t.Fatalf("readyz right after start: %d", got)
	}
	time.Sleep(450 * time.Millisecond) // > 3 x interval with no beat
	res := c.do(http.MethodGet, "/readyz", nil, nil)
	var body map[string]any
	decode(t, res, &body)
	if res.StatusCode != 503 || fmt.Sprint(body["checks"]) != "map[database:ok sweeper:stale]" {
		t.Fatalf("stale sweeper: %d %v", res.StatusCode, body)
	}
	api.SweeperBeat()
	if got := c.status(http.MethodGet, "/readyz", nil); got != 200 {
		t.Fatalf("readyz after beat: %d", got)
	}
	api.SetDraining()
	if got := c.status(http.MethodGet, "/readyz", nil); got != 503 {
		t.Fatalf("readyz while draining: %d", got)
	}
	if got := c.status(http.MethodGet, "/livez", nil); got != 200 {
		t.Fatalf("livez while draining: %d", got)
	}
	m := c.get("/metrics")
	if !strings.Contains(m, `http_requests_total{route="/readyz",status="503"} 2`) ||
		!strings.Contains(m, "# TYPE http_request_duration_seconds histogram") ||
		!strings.Contains(m, `http_request_duration_seconds_bucket{route="/livez",le="+Inf"}`) {
		t.Fatalf("metrics:\n%s", m)
	}
}

func TestCursorPaginationAndOrderFilters(t *testing.T) {
	sh := openShop(t, 0)
	sh.admin.createProduct("PAGE", "Page item", 10_000, 0)
	sh.admin.receive(sh.ikeja, "PAGE", 50)
	custID := sh.admin.customer("Pager", uniquePhone())

	var refs []string
	for i := 0; i < 5; i++ {
		res, out := sh.admin.order(fmt.Sprintf("page-%d-%s", i, uuid.NewString()), sh.ikeja, "cash", "PAGE", 1)
		if res.StatusCode != 201 {
			t.Fatalf("order %d: %d %v", i, res.StatusCode, out)
		}
		refs = append(refs, out["reference"].(string))
		time.Sleep(3 * time.Millisecond)
	}
	// One pending transfer so the status filter has something to exclude.
	res, tr := sh.admin.order("page-tr-"+uuid.NewString(), sh.ikeja, "transfer", "PAGE", 1)
	if res.StatusCode != 201 {
		t.Fatalf("transfer: %d %v", res.StatusCode, tr)
	}

	type page struct {
		Orders     []map[string]any `json:"orders"`
		NextCursor *string          `json:"next_cursor"`
	}
	walk := func(query string) []map[string]any {
		var all []map[string]any
		cursor := ""
		for i := 0; i < 20; i++ {
			q := "/v1/orders?" + query + "&limit=2"
			if cursor != "" {
				q += "&cursor=" + cursor
			}
			r := sh.admin.do(http.MethodGet, q, nil, nil)
			var p page
			decode(t, r, &p)
			if r.StatusCode != 200 || len(p.Orders) > 2 {
				t.Fatalf("page %d: %d %v", i, r.StatusCode, p)
			}
			all = append(all, p.Orders...)
			if p.NextCursor == nil {
				return all
			}
			cursor = *p.NextCursor
		}
		t.Fatal("pagination did not terminate")
		return nil
	}
	all := walk("outlet_id=" + sh.ikeja)
	if len(all) != 6 {
		t.Fatalf("expected 6 orders across pages, got %d", len(all))
	}
	seen := map[string]bool{}
	var prev time.Time
	for i, o := range all {
		id := o["id"].(string)
		if seen[id] {
			t.Fatalf("duplicate %s across pages", id)
		}
		seen[id] = true
		ts, _ := time.Parse(time.RFC3339Nano, o["created_at"].(string))
		if i > 0 && ts.After(prev) {
			t.Fatal("orders not newest-first")
		}
		prev = ts
	}
	if got := walk("status=paid"); len(got) != 5 {
		t.Fatalf("status=paid: %d", len(got))
	}
	if got := walk("status=pending_payment"); len(got) != 1 {
		t.Fatalf("status=pending_payment: %d", len(got))
	}
	if got := walk("reference=" + refs[2]); len(got) != 1 || got[0]["reference"] != refs[2] {
		t.Fatalf("reference filter: %v", got)
	}
	if got := walk("customer_id=" + custID); len(got) != 0 {
		t.Fatalf("customer filter: %v", got)
	}
	today := time.Now().In(time.FixedZone("WAT", 3600)).Format("2006-01-02")
	if got := walk("from=" + today + "&to=" + today); len(got) != 6 {
		t.Fatalf("today range: %d", len(got))
	}
	if got := walk("to=2020-01-01"); len(got) != 0 {
		t.Fatalf("old range: %d", len(got))
	}
	for _, q := range []string{"status=bogus", "customer_id=zzz", "from=yesterday", "cursor=!!!"} {
		if got := sh.admin.status(http.MethodGet, "/v1/orders?"+q, nil); got != 400 {
			t.Errorf("%s: %d", q, got)
		}
	}

	// Journals/transfers and audit page the same way and end with next_cursor null.
	for _, tc := range []struct{ path, key string }{{"/v1/journals", "journals"}, {"/v1/audit", "events"}} {
		seen := map[string]bool{}
		cursor, pages := "", 0
		for {
			q := tc.path + "?limit=2"
			if cursor != "" {
				q += "&cursor=" + cursor
			}
			r := sh.admin.do(http.MethodGet, q, nil, nil)
			var p map[string]any
			decode(t, r, &p)
			if r.StatusCode != 200 {
				t.Fatalf("%s: %d %v", tc.path, r.StatusCode, p)
			}
			items := p[tc.key].([]any)
			if len(items) > 2 {
				t.Fatalf("%s limit ignored", tc.path)
			}
			for _, it := range items {
				id := it.(map[string]any)["id"].(string)
				if seen[id] {
					t.Fatalf("%s duplicate %s", tc.path, id)
				}
				seen[id] = true
			}
			pages++
			next, _ := p["next_cursor"].(string)
			if next == "" {
				break
			}
			cursor = next
			if pages > 50 {
				t.Fatal("no end")
			}
		}
		if len(seen) < 5 || pages < 3 {
			t.Fatalf("%s: %d items over %d pages", tc.path, len(seen), pages)
		}
	}
}
