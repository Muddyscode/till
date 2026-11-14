package money

import (
	"encoding/json"
	"testing"
)

func TestParseJSONAmountAcceptsIntegerKobo(t *testing.T) {
	n, err := ParseJSONAmount(json.RawMessage("5000"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 5000 {
		t.Fatalf("got %d", n)
	}
}

func TestParseJSONAmountRejectsNonIntegers(t *testing.T) {
	cases := []string{
		`10.5`,
		`"1000"`,
		`1e2`,
		`1E2`,
		`null`,
		``,
		`0`,
		`-1`,
		`true`,
		`{}`,
		`9223372036854775808`,
	}
	for _, c := range cases {
		if _, err := ParseJSONAmount(json.RawMessage(c)); err == nil {
			t.Fatalf("expected error for %s", c)
		}
	}
}

func TestParseJSONAmountRejectsFloatThatWouldPassAtoi(t *testing.T) {
	// A broken parser using json.Number.Int64() can coerce 1e2 → 100.
	if _, err := ParseJSONAmount(json.RawMessage(`1e2`)); err == nil {
		t.Fatal("scientific notation must not be accepted as money")
	}
}

func TestParseOptionalKoboAllowsZero(t *testing.T) {
	n, err := ParseOptionalKobo(nil)
	if err != nil || n != 0 {
		t.Fatalf("omitted: %d %v", n, err)
	}
	n, err = ParseOptionalKobo(json.RawMessage("0"))
	if err != nil || n != 0 {
		t.Fatalf("zero: %d %v", n, err)
	}
	n, err = ParseOptionalKobo(json.RawMessage("1500"))
	if err != nil || n != 1500 {
		t.Fatalf("positive: %d %v", n, err)
	}
	for _, c := range []string{`10.5`, `"0"`, `-1`, `1e2`} {
		if _, err := ParseOptionalKobo(json.RawMessage(c)); err == nil {
			t.Fatalf("expected error for %s", c)
		}
	}
}
