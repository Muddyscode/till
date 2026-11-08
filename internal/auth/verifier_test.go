package auth

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestVerifierCapsConcurrency(t *testing.T) {
	v := NewVerifier(1)
	v.sem <- struct{}{} // occupy the only slot
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := v.Verify(ctx, "x", "y"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline while saturated, got %v", err)
	}
	<-v.sem
	ok, err := v.Verify(context.Background(), "x", "not-a-hash")
	if err != nil || ok {
		t.Fatalf("invalid hash must fail cleanly: %v %v", ok, err)
	}
}

func TestVerifierRoundTrip(t *testing.T) {
	v := NewVerifier(2)
	h, err := v.Hash(context.Background(), "change-me-now-12")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := v.Verify(context.Background(), "change-me-now-12", h); !ok {
		t.Fatal("correct password rejected")
	}
	if ok, _ := v.Verify(context.Background(), "nope-nope-nope", h); ok {
		t.Fatal("wrong password accepted")
	}
}
