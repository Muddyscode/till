package events

import (
	"net"
	"testing"
)

func TestBlockedIP(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "192.168.0.9", "172.16.0.1", "169.254.169.254", "100.64.0.1", "0.0.0.0", "::1", "fe80::1", "fc00::1", "224.0.0.1"} {
		if !BlockedIP(net.ParseIP(ip)) {
			t.Errorf("%s should be blocked", ip)
		}
	}
	for _, ip := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if BlockedIP(net.ParseIP(ip)) {
			t.Errorf("%s should be allowed", ip)
		}
	}
}

func TestValidateURL(t *testing.T) {
	cases := []struct {
		url  string
		prod bool
		ok   bool
	}{
		{"https://example.com/hook", true, true},
		{"https://example.com/hook", false, true},
		{"http://example.com/hook", false, false},
		{"http://localhost:8080/h", false, true},
		{"http://127.0.0.1:9/h", false, true},
		{"http://localhost:8080/h", true, false},
		{"https://127.0.0.1/h", true, false},
		{"https://169.254.169.254/latest", true, false},
		{"https://10.0.0.5/h", true, false},
		{"https://user:pw@example.com/h", true, false},
		{"ftp://example.com/h", false, false},
		{"https:///nohost", true, false},
	}
	for _, c := range cases {
		if err := ValidateURL(c.url, c.prod); (err == nil) != c.ok {
			t.Errorf("ValidateURL(%q, prod=%v) = %v, want ok=%v", c.url, c.prod, err, c.ok)
		}
	}
}

func TestSignIsStable(t *testing.T) {
	a := Sign("s", 100, []byte(`{"a":1}`))
	if a != Sign("s", 100, []byte(`{"a":1}`)) || a == Sign("s", 101, []byte(`{"a":1}`)) || a == Sign("t", 100, []byte(`{"a":1}`)) {
		t.Fatal("signature not a function of secret, time and body")
	}
}
