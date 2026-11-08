package httpserver

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func fp(method, path, body string) []byte {
	r := httptest.NewRequest(method, path, nil)
	return fingerprint(r, []byte(body))
}

func TestFingerprintKeepsLargeIntegerPrecision(t *testing.T) {
	// float64 cannot tell these apart; json.Number can.
	a := fp(http.MethodPost, "/v1/x", `{"amount_minor":9007199254740993}`)
	b := fp(http.MethodPost, "/v1/x", `{"amount_minor":9007199254740992}`)
	if bytes.Equal(a, b) {
		t.Fatal("large integers collided")
	}
}

func TestFingerprintIgnoresKeyOrderButNotRoute(t *testing.T) {
	a := fp(http.MethodPost, "/v1/orders/1/refund", `{"a":1,"b":2}`)
	b := fp(http.MethodPost, "/v1/orders/1/refund", "{ \"b\": 2,\n \"a\": 1 }")
	if !bytes.Equal(a, b) {
		t.Fatal("key order changed the fingerprint")
	}
	if bytes.Equal(a, fp(http.MethodPost, "/v1/orders/2/refund", `{"a":1,"b":2}`)) {
		t.Fatal("different order id shares a fingerprint")
	}
	if bytes.Equal(a, fp(http.MethodPatch, "/v1/orders/1/refund", `{"a":1,"b":2}`)) {
		t.Fatal("different method shares a fingerprint")
	}
}
