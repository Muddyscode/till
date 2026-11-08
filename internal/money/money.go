package money

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

const (
	CurrencyNGN  = "NGN"
	KoboPerNaira = int64(100)
)

// ParseJSONAmount accepts a JSON integer only (e.g. 5000).
// Strings, floats, scientific notation, null, and non-positive values are rejected.
func ParseJSONAmount(raw json.RawMessage) (int64, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, fmt.Errorf("amount_minor is required")
	}
	if s[0] == '"' || s[0] == '{' || s[0] == '[' {
		return 0, fmt.Errorf("amount_minor must be a JSON integer (kobo), not a string or object")
	}
	for _, r := range s {
		if r == '-' {
			continue
		}
		if !unicode.IsDigit(r) {
			return 0, fmt.Errorf("amount_minor must be a JSON integer in kobo (no floats)")
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("amount_minor is not a valid int64")
	}
	if n <= 0 {
		return 0, fmt.Errorf("amount_minor must be > 0")
	}
	return n, nil
}

// ParseOptionalKobo accepts an omitted value, JSON null, 0, or a positive integer.
// Floats, strings, and negative numbers are rejected. A sale price still uses ParseJSONAmount.
func ParseOptionalKobo(raw json.RawMessage) (int64, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, nil
	}
	if s[0] == '"' || s[0] == '{' || s[0] == '[' {
		return 0, fmt.Errorf("amount must be a JSON integer (kobo), not a string or object")
	}
	for _, r := range s {
		if r == '-' {
			continue
		}
		if !unicode.IsDigit(r) {
			return 0, fmt.Errorf("amount must be a JSON integer in kobo (no floats)")
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("amount is not a valid int64")
	}
	if n < 0 {
		return 0, fmt.Errorf("amount must be >= 0")
	}
	return n, nil
}

func FormatNGN(kobo int64) string {
	sign := ""
	if kobo < 0 {
		sign = "-"
		kobo = -kobo
	}
	naira := kobo / KoboPerNaira
	frac := kobo % KoboPerNaira
	return fmt.Sprintf("%s₦%s.%02d", sign, withCommas(naira), frac)
}

func withCommas(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre == 0 {
		pre = 3
	}
	b.WriteString(s[:pre])
	for i := pre; i < len(s); i += 3 {
		b.WriteByte(',')
		b.WriteString(s[i : i+3])
	}
	return b.String()
}
