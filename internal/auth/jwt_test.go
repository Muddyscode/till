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
	tok, exp, err := IssueToken(secret, "cashier@example.com", RoleCashier, staffID, merchantID, 7, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(exp) < 50*time.Minute {
		t.Fatal("expiry too soon")
	}
	id, err := ParseToken(secret, tok)
	if err != nil {
		t.Fatal(err)
	}
	if id.StaffID != staffID || id.MerchantID != merchantID || id.Email != "cashier@example.com" || id.Role != RoleCashier {
		t.Fatalf("got %+v", id)
	}
	if id.TokenVersion != 7 {
		t.Fatalf("token version %d", id.TokenVersion)
	}
	if _, err := uuid.Parse(id.JTI); err != nil {
		t.Fatalf("jti missing: %q", id.JTI)
	}
	tok2, _, _ := IssueToken(secret, "cashier@example.com", RoleCashier, staffID, merchantID, 7, time.Hour)
	id2, _ := ParseToken(secret, tok2)
	if id2.JTI == id.JTI {
		t.Fatal("jti must be unique per token")
	}
	if _, err := ParseToken("other-secret-which-is-32-bytes!!", tok); err == nil {
		t.Fatal("wrong secret accepted")
	}
	expired, _, _ := IssueToken(secret, "a@b.c", RoleCashier, staffID, merchantID, 0, -time.Minute)
	if _, err := ParseToken(secret, expired); err == nil {
		t.Fatal("expired token accepted")
	}
}
