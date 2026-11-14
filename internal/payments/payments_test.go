package payments

import (
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestMajorToMinorIsExact(t *testing.T) {
	good := map[string]int64{"4500": 450000, "4500.5": 450050, "4500.50": 450050, "0.07": 7, "100.500": 10050, `"19.99"`: 1999}
	for in, want := range good {
		got, err := majorToMinor(json.RawMessage(in))
		if err != nil || got != want {
			t.Errorf("%s: got %d %v, want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"1e3", "-5", "10.005", "abc", "1.2.3"} {
		if _, err := majorToMinor(json.RawMessage(in)); err == nil {
			t.Errorf("%s: expected an error", in)
		}
	}
}

func TestPaystackBothSignatures(t *testing.T) {
	a, _ := For("paystack")
	body := []byte(`{"event":"charge.success","data":{"id":9001,"reference":"IKJ-1","amount":450000,"fees":6750,"currency":"NGN","status":"success","channel":"dedicated_nuban","paid_at":"2026-11-13T10:00:00.000Z","authorization":{"sender_name":"ADA O","sender_bank":"GTB","sender_bank_account_number":"XXXXXX1234","receiver_bank_account_number":"9900112233"}}}`)
	now := time.Unix(1_800_000_000, 0)

	legacy := http.Header{}
	legacy.Set("x-paystack-signature", SignPaystackLegacy("sk_test", body))
	if !a.Verify("sk_test", legacy, body, now) || a.Verify("other", legacy, body, now) {
		t.Fatal("legacy signature check is wrong")
	}

	ts := strconv.FormatInt(now.Unix(), 10)
	sig := "t=" + ts + ",v1=whk_1:" + hex.EncodeToString(hmacHex(sha512.New, "whsec_x", []byte(ts), []byte("."), body))
	h := http.Header{}
	h.Set("Paystack-Signature", sig)
	if !a.Verify("whsec_x", h, body, now) {
		t.Fatal("timestamped signature should verify")
	}
	if a.Verify("whsec_x", h, body, now.Add(6*time.Minute)) {
		t.Fatal("a signature older than five minutes must be refused")
	}
	if a.Verify("whsec_x", h, append(body, ' '), now) {
		t.Fatal("a changed body must be refused")
	}

	e, err := a.Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != "9001" || e.Reference != "IKJ-1" || e.AmountMinor != 450000 || e.FeeMinor != 6750 ||
		e.Status != StatusSuccess || e.Method != MethodTransfer || e.AccountNumber != "9900112233" || e.PayerName != "ADA O" {
		t.Fatalf("unexpected event %+v", e)
	}
	if _, err := a.Parse([]byte(`{"event":"transfer.success","data":{}}`)); !errors.Is(err, ErrIgnored) {
		t.Fatalf("other events are ignored, got %v", err)
	}
}

func TestTillStandard(t *testing.T) {
	a, _ := For("till")
	body := []byte(`{"id":"tap-77","amount_minor":120000,"method":"tap"}`)
	now := time.Unix(1_800_000_000, 0)
	h := http.Header{}
	h.Set("Till-Signature", SignTill("s3cret", now.Unix(), body))
	if !a.Verify("s3cret", h, body, now) || a.Verify("nope", h, body, now) {
		t.Fatal("till signature check is wrong")
	}
	e, err := a.Parse(body)
	if err != nil || e.Currency != "NGN" || e.Status != StatusSuccess || e.Method != MethodTap || e.AmountMinor != 120000 {
		t.Fatalf("unexpected %+v %v", e, err)
	}
	for _, bad := range []string{
		`{"id":"x","amount_minor":10.5}`, `{"id":"x","amount_minor":"100"}`, `{"amount_minor":100}`,
		`{"id":"x","amount_minor":100,"method":"cash"}`, `{"id":"x","amount_minor":100,"fee_minor":100}`,
	} {
		if _, err := a.Parse([]byte(bad)); err == nil {
			t.Errorf("%s: expected an error", bad)
		}
	}
}

func TestMajorUnitProviders(t *testing.T) {
	fw, _ := For("flutterwave")
	h := http.Header{}
	h.Set("verif-hash", "my-hash")
	if !fw.Verify("my-hash", h, nil, time.Now()) || fw.Verify("else", h, nil, time.Now()) || fw.Verify("", http.Header{}, nil, time.Now()) {
		t.Fatal("verif-hash check is wrong")
	}
	e, err := fw.Parse([]byte(`{"event":"charge.completed","data":{"id":285959875,"tx_ref":"IKJ-2","amount":4500.5,"app_fee":63.01,"currency":"NGN","status":"successful","payment_type":"bank_transfer"}}`))
	if err != nil || e.AmountMinor != 450050 || e.FeeMinor != 6301 || e.Method != MethodTransfer || e.Reference != "IKJ-2" {
		t.Fatalf("flutterwave %+v %v", e, err)
	}

	mn, _ := For("monnify")
	body := []byte(`{"eventType":"SUCCESSFUL_TRANSACTION","eventData":{"transactionReference":"MNFY|1","paymentReference":"","amountPaid":2000.00,"paymentStatus":"PAID","paymentMethod":"ACCOUNT_TRANSFER","paidOn":"2026-11-13 10:00:00.000","destinationAccountInformation":{"accountNumber":"5000112233"},"paymentSourceInformation":[{"accountName":"CHIDI","accountNumber":"0011223344","bankCode":"058","sessionId":"000013261113100000000000000001"}]}}`)
	mh := http.Header{}
	mh.Set("monnify-signature", SignPaystackLegacy("client-secret", body))
	if !mn.Verify("client-secret", mh, body, time.Now()) {
		t.Fatal("monnify signature should verify")
	}
	e, err = mn.Parse(body)
	if err != nil || e.AmountMinor != 200000 || e.Reference != "" || e.AccountNumber != "5000112233" || e.SessionID == "" || e.Currency != "NGN" {
		t.Fatalf("monnify %+v %v", e, err)
	}
}
