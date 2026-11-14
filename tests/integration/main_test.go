package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Muddyscode/till/internal/auth"
	"github.com/Muddyscode/till/internal/config"
	"github.com/Muddyscode/till/internal/httpserver"
	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var testPool *pgxpool.Pool

var (
	hashOnce sync.Once
	hashVal  string
	hashErr  error
)

func cachedHash(t *testing.T) string {
	t.Helper()
	hashOnce.Do(func() {
		hashVal, hashErr = auth.HashPassword("change-me-now-12")
	})
	if hashErr != nil {
		t.Fatal(hashErr)
	}
	return hashVal
}

func TestMain(m *testing.M) {
	dsn := os.Getenv("TILL_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://till:till@127.0.0.1:5432/till_test?sslmode=disable"
	}
	// This laptop is slow right after Postgres recovery. A 4s ping looks like a dead database.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		if os.Getenv("TILL_REQUIRE_DB") == "1" {
			fmt.Fprintln(os.Stderr, "TILL_REQUIRE_DB=1 but postgres is unreachable:", dsn, err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "integration tests skipped: no postgres (set TILL_TEST_DATABASE_URL)")
		os.Exit(0)
	}
	if err = pool.Ping(ctx); err != nil {
		if os.Getenv("TILL_REQUIRE_DB") == "1" {
			fmt.Fprintln(os.Stderr, "TILL_REQUIRE_DB=1 but postgres ping failed:", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "integration tests skipped: no postgres (set TILL_TEST_DATABASE_URL)")
		os.Exit(0)
	}
	if err := store.Migrate(context.Background(), pool); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
	testPool = pool
	code := m.Run()
	pool.Close()
	os.Exit(code)
}

type apiClient struct {
	t     *testing.T
	srv   *httptest.Server
	token string
	pstk  string // id of the merchant's Paystack test connection
}

func startAPI(t *testing.T) *apiClient {
	t.Helper()
	st := store.New(testPool)
	cfg := config.Config{
		JWTSecret:   "test-secret-must-be-32-bytes-ok!",
		HoldMinutes: 15,
	}
	api := httpserver.New(cfg, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(api.Router())
	t.Cleanup(srv.Close)
	return &apiClient{t: t, srv: srv}
}

func (c *apiClient) do(method, path string, body any, extra http.Header) *http.Response {
	c.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.srv.URL+path, rdr)
	if err != nil {
		c.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for k, vs := range extra {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	autoKey(req)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	return res
}

func (c *apiClient) doRaw(method, path, raw string, extra http.Header) *http.Response {
	c.t.Helper()
	req, err := http.NewRequest(method, c.srv.URL+path, bytes.NewBufferString(raw))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for k, vs := range extra {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	autoKey(req)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	return res
}

// autoKey adds a fresh Idempotency-Key to the endpoints that now require one
// (stock receipts/adjustments, product PATCH) so older tests keep their shape.
// A test that wants no key sets X-Test-No-Key.
func autoKey(req *http.Request) {
	if req.Header.Get("X-Test-No-Key") != "" {
		req.Header.Del("X-Test-No-Key")
		return
	}
	needs := req.Method == http.MethodPatch && strings.HasPrefix(req.URL.Path, "/v1/products/") ||
		req.Method == http.MethodPost && strings.HasPrefix(req.URL.Path, "/v1/stock/")
	if needs && req.Header.Get("Idempotency-Key") == "" {
		req.Header.Set("Idempotency-Key", "auto-"+uuid.NewString())
	}
}

func decode(t *testing.T, res *http.Response, dest any) {
	t.Helper()
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if dest == nil {
		return
	}
	if err := json.Unmarshal(b, dest); err != nil {
		t.Fatalf("decode %s: %s", err, b)
	}
}

func (c *apiClient) register() {
	c.t.Helper()
	c.loginStaff("admin", nil)
}

func (c *apiClient) loginStaff(role string, outlets []string) string {
	c.t.Helper()
	st := store.New(testPool)
	ctx := context.Background()
	mid := uuid.Must(uuid.NewV7())
	sid := uuid.Must(uuid.NewV7())
	email := fmt.Sprintf("%s-%s@test.local", role, uuid.NewString()[:8])
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := st.CreateMerchant(ctx, tx, mid, "Test Books", 0); err != nil {
			return err
		}
		_, err := st.CreateStaff(ctx, tx, store.Staff{
			ID: sid, MerchantID: mid, Email: email, PasswordHash: cachedHash(c.t), Role: role,
		})
		return err
	})
	if err != nil {
		c.t.Fatal(err)
	}
	_ = outlets
	res := c.do(http.MethodPost, "/v1/auth/login", map[string]string{
		"email": email, "password": "change-me-now-12",
	}, nil)
	var out struct {
		Token string `json:"token"`
	}
	decode(c.t, res, &out)
	if res.StatusCode != http.StatusOK || out.Token == "" {
		c.t.Fatalf("login %d", res.StatusCode)
	}
	c.token = out.Token
	return email
}

func (c *apiClient) createAccount(code, name, typ string) string {
	c.t.Helper()
	res := c.do(http.MethodPost, "/v1/accounts", map[string]any{
		"code": code, "name": name, "type": typ,
	}, nil)
	var out struct {
		ID string `json:"id"`
	}
	decode(c.t, res, &out)
	if res.StatusCode != http.StatusCreated {
		c.t.Fatalf("create account %s: %d", code, res.StatusCode)
	}
	return out.ID
}

func (c *apiClient) postJournal(key string, body any) (*http.Response, map[string]any) {
	c.t.Helper()
	h := http.Header{}
	h.Set("Idempotency-Key", key)
	res := c.do(http.MethodPost, "/v1/journals", body, h)
	var out map[string]any
	decode(c.t, res, &out)
	return res, out
}

// postPair posts a two-line journal: debit one account, credit another.
func (c *apiClient) postPair(key, debit, credit string, amount int64, desc string) (*http.Response, map[string]any) {
	c.t.Helper()
	return c.postJournal(key, map[string]any{
		"description": desc,
		"postings": []map[string]any{
			{"account_id": debit, "direction": "debit", "amount_minor": amount},
			{"account_id": credit, "direction": "credit", "amount_minor": amount},
		},
	})
}

func (c *apiClient) identity() map[string]any {
	c.t.Helper()
	res := c.do(http.MethodGet, "/v1/invariants", nil, nil)
	var out map[string]any
	decode(c.t, res, &out)
	if res.StatusCode != 200 {
		c.t.Fatalf("invariants %d %v", res.StatusCode, out)
	}
	return out
}

func (c *apiClient) account(id string) map[string]any {
	c.t.Helper()
	res := c.do(http.MethodGet, "/v1/accounts/"+id, nil, nil)
	var out map[string]any
	decode(c.t, res, &out)
	if res.StatusCode != 200 {
		c.t.Fatalf("get account %d %v", res.StatusCode, out)
	}
	return out
}

func errCode(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	if e == nil {
		return ""
	}
	s, _ := e["code"].(string)
	return s
}
