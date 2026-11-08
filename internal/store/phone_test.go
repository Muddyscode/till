package store

import "testing"

func TestNormalizePhone(t *testing.T) {
	ok := map[string]string{
		"08031234567":       "+2348031234567",
		"0803 123 4567":     "+2348031234567",
		"8031234567":        "+2348031234567",
		"2348031234567":     "+2348031234567",
		"+234 803 123 4567": "+2348031234567",
		"002348031234567":   "+2348031234567",
		"+234-703-123-4567": "+2347031234567",
		"09012345678":       "+2349012345678",
		"+14155550123":      "+14155550123",
	}
	for in, want := range ok {
		got, err := NormalizePhone(in)
		if err != nil || got != want {
			t.Errorf("%q: got %q err %v want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0803", "0803abcd567", "12345678901234567", "+0803123456", "01234567890", "080312345678"} {
		if got, err := NormalizePhone(in); err == nil {
			t.Errorf("%q should be rejected, got %q", in, got)
		}
	}
}

func TestMaskAndSearchDigits(t *testing.T) {
	if got := MaskPhone("+2348031234567"); got != "0803****567" {
		t.Fatalf("mask: %s", got)
	}
	if got := MaskPhone("08031234567"); got != "0803****567" {
		t.Fatalf("mask legacy: %s", got)
	}
	if PhoneSearchDigits("+234 803 12") != "80312" || PhoneSearchDigits("0803 12") != "80312" || PhoneSearchDigits("803") != "803" {
		t.Fatal("search digits")
	}
}
