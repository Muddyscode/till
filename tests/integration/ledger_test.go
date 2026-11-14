package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

func book(t *testing.T) (*apiClient, string, string, string, string, string) {
	t.Helper()
	c := startAPI(t)
	c.register()
	cash := c.createAccount("1000", "Cash", "asset")
	alice := c.createAccount("2000", "Alice", "liability")
	bob := c.createAccount("2010", "Bob", "liability")
	equity := c.createAccount("3000", "Equity", "equity")
	fees := c.createAccount("4000", "Fees", "income")
	res, out := c.postJournal("open-"+uuid.NewString(), map[string]any{
		"description": "opening",
		"postings": []map[string]any{
			{"account_id": cash, "direction": "debit", "amount_minor": 1_000_000},
			{"account_id": equity, "direction": "credit", "amount_minor": 1_000_000},
		},
	})
	if res.StatusCode != 201 {
		t.Fatalf("opening %d %v", res.StatusCode, out)
	}
	return c, cash, alice, bob, equity, fees
}

func TestUnbalancedJournalRejected(t *testing.T) {
	c, cash, alice, _, _, _ := book(t)
	res, out := c.postJournal("bad-"+uuid.NewString(), map[string]any{
		"description": "unbalanced",
		"postings": []map[string]any{
			{"account_id": cash, "direction": "debit", "amount_minor": 50},
			{"account_id": alice, "direction": "credit", "amount_minor": 40},
		},
	})
	if res.StatusCode != 400 || errCode(out) != "unbalanced_journal" {
		t.Fatalf("got %d %v", res.StatusCode, out)
	}
}

func TestNonIntegerAmountRejected(t *testing.T) {
	c, cash, alice, _, _, _ := book(t)
	for _, amt := range []string{"10.5", "1e2"} {
		h := http.Header{}
		h.Set("Idempotency-Key", uuid.NewString())
		raw := fmt.Sprintf(`{"description":"x","postings":[{"account_id":%q,"direction":"debit","amount_minor":%s},{"account_id":%q,"direction":"credit","amount_minor":%s}]}`, cash, amt, alice, amt)
		res := c.doRaw(http.MethodPost, "/v1/journals", raw, h)
		var out map[string]any
		decode(t, res, &out)
		if res.StatusCode != 400 {
			t.Fatalf("%s accepted: %d %v", amt, res.StatusCode, out)
		}
	}
}

func TestOverdraftRejected(t *testing.T) {
	c, _, alice, bob, _, _ := book(t)
	res, out := c.postPair(uuid.NewString(), alice, bob, 1, "no funds")
	if res.StatusCode != 400 || errCode(out) != "insufficient_funds" {
		t.Fatalf("got %d %v", res.StatusCode, out)
	}
}

func TestIdempotentReplayAndConflict(t *testing.T) {
	c, cash, alice, _, _, _ := book(t)
	key := "idem-" + uuid.NewString()
	res1, out1 := c.postPair(key, cash, alice, 1000, "deposit")
	if res1.StatusCode != 201 {
		t.Fatalf("first %d %v", res1.StatusCode, out1)
	}
	res2, out2 := c.postPair(key, cash, alice, 1000, "deposit")
	if res2.StatusCode != 201 {
		t.Fatalf("replay %d %v", res2.StatusCode, out2)
	}
	if out1["id"] != out2["id"] {
		t.Fatalf("replay minted a new journal: %v vs %v", out1["id"], out2["id"])
	}
	res3, out3 := c.postPair(key, cash, alice, 2000, "different")
	if res3.StatusCode != 409 || errCode(out3) != "idempotency_conflict" {
		t.Fatalf("conflict %d %v", res3.StatusCode, out3)
	}
}

func TestReversalRestoresBalancesAndLeavesOriginal(t *testing.T) {
	c, cash, alice, _, _, _ := book(t)
	res, posted := c.postPair(uuid.NewString(), cash, alice, 8000, "deposit")
	if res.StatusCode != 201 {
		t.Fatalf("deposit %d %v", res.StatusCode, posted)
	}
	origID := posted["id"].(string)
	h := http.Header{}
	h.Set("Idempotency-Key", "rev-"+uuid.NewString())
	rev := c.do(http.MethodPost, "/v1/journals/"+origID+"/reversal", map[string]any{}, h)
	var revBody map[string]any
	decode(t, rev, &revBody)
	if rev.StatusCode != 201 {
		t.Fatalf("reversal %d %v", rev.StatusCode, revBody)
	}
	if c.account(alice)["balance_minor"].(float64) != 0 {
		t.Fatalf("alice not restored: %v", c.account(alice))
	}
	got := c.do(http.MethodGet, "/v1/journals/"+origID, nil, nil)
	var orig map[string]any
	decode(t, got, &orig)
	if got.StatusCode != 200 {
		t.Fatal("original journal missing")
	}
	posts, _ := orig["postings"].([]any)
	if len(posts) != 2 {
		t.Fatalf("original mutated: %v", orig)
	}
	h2 := http.Header{}
	h2.Set("Idempotency-Key", "rev2-"+uuid.NewString())
	rev2 := c.do(http.MethodPost, "/v1/journals/"+origID+"/reversal", map[string]any{}, h2)
	var rev2Body map[string]any
	decode(t, rev2, &rev2Body)
	if rev2.StatusCode != 409 || errCode(rev2Body) != "already_reversed" {
		t.Fatalf("second reversal %d %v", rev2.StatusCode, rev2Body)
	}
}

func TestIDOR(t *testing.T) {
	a := startAPI(t)
	a.register()
	id := a.createAccount("1000", "Cash A", "asset")
	b := startAPI(t)
	b.register()
	res := b.do(http.MethodGet, "/v1/accounts/"+id, nil, nil)
	var out map[string]any
	decode(t, res, &out)
	if res.StatusCode != 404 {
		t.Fatalf("IDOR leaked: %d %v", res.StatusCode, out)
	}
}

func TestNLeggedJournal(t *testing.T) {
	c, cash, alice, bob, _, fees := book(t)
	_, _ = c.postPair(uuid.NewString(), cash, alice, 10_000, "fund")
	res, out := c.postJournal("split-"+uuid.NewString(), map[string]any{
		"description": "p2p with fee",
		"postings": []map[string]any{
			{"account_id": alice, "direction": "debit", "amount_minor": 5100},
			{"account_id": bob, "direction": "credit", "amount_minor": 5000},
			{"account_id": fees, "direction": "credit", "amount_minor": 100},
		},
	})
	if res.StatusCode != 201 {
		t.Fatalf("n-leg %d %v", res.StatusCode, out)
	}
	if c.identity()["holds"] != true {
		t.Fatal("identity broken")
	}
}

func TestConcurrentTransfersPreserveInvariant(t *testing.T) {
	c, cash, alice, bob, _, _ := book(t)
	_, _ = c.postPair(uuid.NewString(), cash, alice, 100_000, "fund")
	const n = 50
	var ok atomic.Int64
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			res, _ := c.postPair(fmt.Sprintf("c-%d-%s", i, uuid.NewString()), alice, bob, 100, "burst")
			if res.StatusCode == 201 {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != n {
		t.Fatalf("only %d/%d succeeded", ok.Load(), n)
	}
	if c.account(alice)["balance_minor"].(float64) != 95_000 {
		t.Fatalf("alice %v", c.account(alice))
	}
	if c.account(bob)["balance_minor"].(float64) != 5_000 {
		t.Fatalf("bob %v", c.account(bob))
	}
	if c.identity()["holds"] != true {
		t.Fatalf("invariant %#v", c.identity())
	}
}

func TestConcurrentSameIdempotencyKey(t *testing.T) {
	c, cash, alice, _, _, _ := book(t)
	key := "same-" + uuid.NewString()
	var ids []string
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(20)
	for i := 0; i < 20; i++ {
		go func() {
			defer wg.Done()
			res, out := c.postPair(key, cash, alice, 2500, "once")
			if res.StatusCode == 201 {
				mu.Lock()
				ids = append(ids, out["id"].(string))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(ids) == 0 {
		t.Fatal("no success")
	}
	first := ids[0]
	for _, id := range ids {
		if id != first {
			t.Fatalf("duplicate journals %v", ids)
		}
	}
	if c.account(alice)["balance_minor"].(float64) != 2500 {
		t.Fatalf("double posted: %v", c.account(alice))
	}
}

func TestStatementRunningBalance(t *testing.T) {
	c, cash, alice, _, _, _ := book(t)
	_, _ = c.postPair(uuid.NewString(), cash, alice, 1000, "d1")
	_, _ = c.postPair(uuid.NewString(), cash, alice, 2000, "d2")
	res := c.do(http.MethodGet, "/v1/accounts/"+alice+"/statement", nil, nil)
	var out struct {
		Balance int64 `json:"balance_minor"`
		Entries []struct {
			Running int64 `json:"running_balance_minor"`
			Amount  int64 `json:"amount_minor"`
		} `json:"entries"`
	}
	decode(t, res, &out)
	if res.StatusCode != 200 {
		t.Fatalf("statement %d", res.StatusCode)
	}
	if out.Balance != 3000 {
		t.Fatalf("balance %d", out.Balance)
	}
	if len(out.Entries) != 2 {
		t.Fatalf("entries %d", len(out.Entries))
	}
	if out.Entries[0].Running != 1000 || out.Entries[1].Running != 3000 {
		b, _ := json.Marshal(out.Entries)
		t.Fatalf("running %s", b)
	}
}

func TestPostedJournalImmutable(t *testing.T) {
	c, cash, alice, _, _, _ := book(t)
	res, posted := c.postPair(uuid.NewString(), cash, alice, 100, "lock")
	if res.StatusCode != 201 {
		t.Fatalf("post %d %v", res.StatusCode, posted)
	}
	id := posted["id"].(string)
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `UPDATE journals SET description = 'hacked' WHERE id = $1`, id); err == nil {
		t.Fatal("UPDATE on posted journal was allowed")
	}
	if _, err := testPool.Exec(ctx, `DELETE FROM postings WHERE journal_id = $1`, id); err == nil {
		t.Fatal("DELETE on postings was allowed")
	}
}

func TestUnauthorizedWithoutToken(t *testing.T) {
	c := startAPI(t)
	res := c.do(http.MethodGet, "/v1/accounts", nil, nil)
	if res.StatusCode != 401 {
		t.Fatalf("got %d", res.StatusCode)
	}
	decode(t, res, nil)
}
