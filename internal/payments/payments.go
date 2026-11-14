// Package payments turns a payment provider's notification into one common
// Event. It knows nothing about orders, stock or the ledger: an adapter only
// checks who sent the bytes and reads what they say.
//
// Adding a provider is one file: implement Adapter and register it in adapters.
package payments

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Payment methods that are confirmed by someone other than the cashier.
// Cash and wallet are settled at the till and never arrive as an Event.
const (
	MethodTransfer    = "transfer"
	MethodCard        = "card"
	MethodTap         = "tap" // contactless: card or phone tapped on a terminal or a phone (SoftPOS)
	MethodUSSD        = "ussd"
	MethodQR          = "qr"
	MethodMobileMoney = "mobile_money"
)

// AwaitedMethods are the methods an order can wait on.
var AwaitedMethods = []string{MethodTransfer, MethodCard, MethodTap, MethodUSSD, MethodQR, MethodMobileMoney}

// Awaited reports whether a method is confirmed by a payment event.
func Awaited(method string) bool {
	for _, m := range AwaitedMethods {
		if m == method {
			return true
		}
	}
	return false
}

const (
	StatusSuccess  = "success"
	StatusFailed   = "failed"
	StatusReversed = "reversed"
)

// Event is one payment as the provider reported it, in minor units.
type Event struct {
	// ID is the provider's own id for this payment. It is the dedupe key.
	ID string
	// Reference is the order reference the payer or the checkout carried. It is
	// empty for a plain transfer into the shop's account.
	Reference   string
	AmountMinor int64
	FeeMinor    int64
	Currency    string
	Status      string
	Method      string
	PaidAt      *time.Time
	// Who paid, when the provider says. Never used to decide anything.
	PayerName, PayerAccount, PayerBank string
	// AccountNumber is the account the money landed in (a virtual account, usually).
	AccountNumber string
	// SessionID is the NIP session id of a bank transfer, when given.
	SessionID string
}

// ErrIgnored means the notification is genuine but is not a payment (a
// subscription notice, a payout, a ping). The caller acknowledges it.
var ErrIgnored = errors.New("payments: not a payment event")

// Adapter reads one provider's notifications.
type Adapter interface {
	// Verify reports whether the body was sent by the holder of secret.
	Verify(secret string, h http.Header, body []byte, now time.Time) bool
	// Parse reads the body. It returns ErrIgnored for events that are not payments.
	Parse(body []byte) (Event, error)
}

var adapters = map[string]Adapter{
	"till":        tillAdapter{},
	"paystack":    paystackAdapter{},
	"flutterwave": flutterwaveAdapter{},
	"monnify":     monnifyAdapter{},
}

// For returns the adapter for a provider name.
func For(provider string) (Adapter, bool) {
	a, ok := adapters[provider]
	return a, ok
}

// Providers lists the supported provider names.
func Providers() []string {
	return []string{"till", "paystack", "flutterwave", "monnify"}
}

// Validate checks an Event before it is allowed near the books.
func (e Event) Validate() error {
	if strings.TrimSpace(e.ID) == "" || len(e.ID) > 200 {
		return fmt.Errorf("event id is required (at most 200 characters)")
	}
	if e.AmountMinor <= 0 {
		return fmt.Errorf("amount must be a positive whole number of minor units")
	}
	if e.FeeMinor < 0 || e.FeeMinor >= e.AmountMinor {
		return fmt.Errorf("fee must be zero or more and less than the amount")
	}
	if len(e.Currency) != 3 {
		return fmt.Errorf("currency must be a 3-letter code")
	}
	switch e.Status {
	case StatusSuccess, StatusFailed, StatusReversed:
	default:
		return fmt.Errorf("status must be success, failed or reversed")
	}
	if e.Method != "" && !Awaited(e.Method) {
		return fmt.Errorf("method must be one of %s", strings.Join(AwaitedMethods, ", "))
	}
	return nil
}

func hmacHex(newHash func() hash.Hash, secret string, parts ...[]byte) []byte {
	m := hmac.New(newHash, []byte(secret))
	for _, p := range parts {
		_, _ = m.Write(p)
	}
	return m.Sum(nil)
}

func hexEqual(want []byte, gotHex string) bool {
	got, err := hex.DecodeString(strings.TrimSpace(gotHex))
	return err == nil && len(got) > 0 && hmac.Equal(want, got)
}

// timestamped verifies "t=<unix>,v1=<sig>[,v1=<sig>]" headers: an HMAC of
// "t.body", accepted within five minutes of now. Each v1 may be "<key id>:<hex>".
func timestamped(newHash func() hash.Hash, secret, header string, body []byte, now time.Time) bool {
	var ts string
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			if ts != "" {
				return false
			}
			ts = v
		case "v1":
			if i := strings.LastIndex(v, ":"); i >= 0 {
				v = v[i+1:]
			}
			sigs = append(sigs, v)
		}
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || len(sigs) == 0 {
		return false
	}
	if d := now.Unix() - unix; d > 300 || d < -300 {
		return false
	}
	want := hmacHex(newHash, secret, []byte(ts), []byte("."), body)
	for _, s := range sigs {
		if hexEqual(want, s) {
			return true
		}
	}
	return false
}

// SignTill returns the Till-Signature header for the standard format:
// t=<unix>,v1=<hex HMAC-SHA256 of "t.body">. It is the scheme Till's own
// outbound webhooks use.
func SignTill(secret string, ts int64, body []byte) string {
	t := strconv.FormatInt(ts, 10)
	return "t=" + t + ",v1=" + hex.EncodeToString(hmacHex(sha256.New, secret, []byte(t), []byte("."), body))
}

// SignPaystackLegacy returns the x-paystack-signature value: hex HMAC-SHA512 of the body.
func SignPaystackLegacy(secret string, body []byte) string {
	return hex.EncodeToString(hmacHex(sha512.New, secret, body))
}

// minorInt reads a JSON integer of minor units. Floats and strings are refused.
func minorInt(raw json.RawMessage) (int64, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("amount must be a whole number of minor units")
	}
	return n, nil
}

// majorToMinor converts a JSON number in major units (naira, "4500" or
// "4500.50") to minor units without going through a float.
func majorToMinor(raw json.RawMessage) (int64, error) {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" {
		return 0, nil
	}
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	if strings.ContainsAny(s, "eE-+") {
		return 0, fmt.Errorf("amount %q is not a plain decimal", s)
	}
	// Providers pad or trim decimals ("100", "100.5", "100.50", "100.500").
	for len(frac) > 2 {
		if frac[len(frac)-1] != '0' {
			return 0, fmt.Errorf("amount %q has more than two decimal places", s)
		}
		frac = frac[:len(frac)-1]
	}
	for len(frac) < 2 {
		frac += "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w > (1<<62)/100 {
		return 0, fmt.Errorf("amount %q is not a valid number", s)
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("amount %q is not a valid number", s)
	}
	return w*100 + f, nil
}

func parseTime(s string, layouts ...string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, l := range append([]string{time.RFC3339Nano}, layouts...) {
		if t, err := time.Parse(l, s); err == nil {
			return &t
		}
	}
	return nil
}

func rawID(raw json.RawMessage) string {
	return strings.Trim(strings.TrimSpace(string(raw)), `"`)
}
