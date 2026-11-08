package auth

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestJWTRoundTrip(t *testing.T) {
	secret := "dev-only-change-me-use-32chars-min!!"
	staffID := uuid.Must(uuid.NewV7())
	merchantID := uuid.Must(uuid.NewV7())
	tok, exp, err := IssueToken(secret, "cashier@example.com", RoleCashier, staffID, merchantID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(exp) < 50*time.Minute {
		t.Fatal("expiry too soon")
	}
	gotStaff, gotMerchant, email, role, err := ParseToken(secret, tok)
	if err != nil {
		t.Fatal(err)
	}
	if gotStaff != staffID || gotMerchant != merchantID || email != "cashier@example.com" || role != RoleCashier {
		t.Fatalf("got %s %s %s %s", gotStaff, gotMerchant, email, role)
	}
	if _, _, _, _, err := ParseToken("other-secret-which-is-32-bytes!!", tok); err == nil {
		t.Fatal("wrong secret accepted")
	}
}
