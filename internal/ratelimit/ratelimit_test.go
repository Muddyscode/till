package ratelimit

import (
	"testing"
	"time"
)

func TestTokenBucket(t *testing.T) {
	l := New(3, 60) // 1 token per second
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("request %d should pass", i)
		}
	}
	ok, wait := l.Allow("a")
	if ok || wait < time.Second {
		t.Fatalf("4th should be limited, wait=%v", wait)
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("keys are independent")
	}
	now = now.Add(1500 * time.Millisecond)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("token should have refilled")
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("only one token refilled")
	}
}

func TestSweepBoundsMemory(t *testing.T) {
	l := New(2, 120)
	l.maxKeys = 10
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }
	for i := 0; i < 10; i++ {
		l.Allow(string(rune('a' + i)))
	}
	now = now.Add(time.Minute)
	l.Allow("fresh")
	if len(l.buckets) > 2 {
		t.Fatalf("refilled buckets should be swept, have %d", len(l.buckets))
	}
}
