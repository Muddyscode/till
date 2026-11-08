package store

import (
	"strings"

	"github.com/Muddyscode/till/internal/ledger"
)

// NormalizePhone returns an E.164 number. Nigerian mobile numbers (0803..., 803...,
// 234803..., +234 803...) become +234XXXXXXXXXX. Other international numbers must start
// with + and are kept as +digits. Anything else is rejected.
func NormalizePhone(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	plus := strings.HasPrefix(s, "+")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '.' || r == '(' || r == ')' || r == '+':
		default:
			return "", errInvalidPhone()
		}
	}
	d := b.String()
	if strings.HasPrefix(d, "00") && !plus {
		d, plus = d[2:], true
	}
	switch {
	case strings.HasPrefix(d, "234") && len(d) == 13 && isNGMobile(d[3:]):
		return "+" + d, nil
	case !plus && len(d) == 11 && d[0] == '0' && isNGMobile(d[1:]):
		return "+234" + d[1:], nil
	case !plus && len(d) == 10 && isNGMobile(d):
		return "+234" + d, nil
	case plus && !strings.HasPrefix(d, "234") && len(d) >= 8 && len(d) <= 15 && d[0] != '0':
		return "+" + d, nil
	}
	return "", errInvalidPhone()
}

func isNGMobile(national string) bool {
	return len(national) == 10 && (national[0] == '7' || national[0] == '8' || national[0] == '9')
}

func errInvalidPhone() error {
	return ledger.Err("invalid_phone", "phone must be a valid Nigerian number such as 08031234567 or +2348031234567")
}

// PhoneSearchDigits reduces user search input to digits without the country or trunk prefix,
// so "0803 123" matches both +2348031234567 and legacy un-normalised "08031234567".
func PhoneSearchDigits(q string) string {
	var b strings.Builder
	for _, r := range q {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	d := b.String()
	switch {
	case strings.HasPrefix(d, "234") && len(d) > 3:
		d = d[3:]
	case strings.HasPrefix(d, "0"):
		d = d[1:]
	}
	return d
}

// MaskPhone hides the middle of a number: +2348031234567 becomes 0803****567.
func MaskPhone(p string) string {
	d := p
	if strings.HasPrefix(d, "+234") && len(d) == 14 {
		d = "0" + d[4:]
	}
	if len(d) >= 9 {
		return d[:4] + "****" + d[len(d)-3:]
	}
	return "****"
}
