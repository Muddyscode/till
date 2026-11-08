package auth

import (
	"context"
	"sync"
)

// Verifier runs argon2 work behind a semaphore so a burst of logins cannot
// exhaust memory (each hash needs 64 MiB).
type Verifier struct {
	sem chan struct{}
}

func NewVerifier(maxConcurrent int) *Verifier {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &Verifier{sem: make(chan struct{}, maxConcurrent)}
}

func (v *Verifier) acquire(ctx context.Context) error {
	select {
	case v.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (v *Verifier) release() { <-v.sem }

// Verify checks password against encoded. An empty or invalid encoded hash is
// verified against a dummy hash so the time taken does not reveal that.
func (v *Verifier) Verify(ctx context.Context, password, encoded string) (bool, error) {
	if err := v.acquire(ctx); err != nil {
		return false, err
	}
	defer v.release()
	if _, _, ok := parseArgon(encoded); !ok {
		VerifyPassword(password, dummyHash())
		return false, nil
	}
	return VerifyPassword(password, encoded), nil
}

// Hash hashes a password under the same concurrency cap.
func (v *Verifier) Hash(ctx context.Context, password string) (string, error) {
	if err := v.acquire(ctx); err != nil {
		return "", err
	}
	defer v.release()
	return HashPassword(password)
}

var (
	dummyOnce sync.Once
	dummyVal  string
)

func dummyHash() string {
	dummyOnce.Do(func() {
		h, err := HashPassword("till-dummy-password-for-timing")
		if err != nil {
			panic(err)
		}
		dummyVal = h
	})
	return dummyVal
}
