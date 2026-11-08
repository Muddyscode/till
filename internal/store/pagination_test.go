package store

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCursorRoundTrip(t *testing.T) {
	c := Cursor{CreatedAt: time.Date(2026, 11, 8, 10, 30, 0, 123456000, time.UTC), ID: uuid.Must(uuid.NewV7())}
	got, err := DecodeCursor(c.Encode())
	if err != nil || !got.CreatedAt.Equal(c.CreatedAt) || got.ID != c.ID {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	for _, bad := range []string{"", "!!!", "bm90LWEtY3Vyc29y"} {
		if _, err := DecodeCursor(bad); err == nil {
			t.Errorf("%q should be invalid", bad)
		}
	}
}
