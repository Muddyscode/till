package checkout

import "testing"

func TestApportionSumsAndRemainderGoesLast(t *testing.T) {
	w := []int64{100, 100, 100}
	got := apportion(100, w, w)
	if got[0] != 33 || got[1] != 33 || got[2] != 34 {
		t.Fatalf("got %v", got)
	}
	w = []int64{300_000, 100_000}
	got = apportion(30_000, w, w)
	if got[0] != 22_500 || got[1] != 7_500 {
		t.Fatalf("got %v", got)
	}
}

func TestApportionRespectsCaps(t *testing.T) {
	// A tiny last line cannot take more than it costs; the excess moves back.
	w := []int64{1000, 2}
	got := apportion(1001, w, w)
	if got[0]+got[1] != 1001 || got[1] > 2 || got[0] > 1000 {
		t.Fatalf("got %v", got)
	}
}

func TestMulDivNoOverflow(t *testing.T) {
	if got := mulDiv(1<<62, 3, 4); got != 3*(1<<60) {
		t.Fatalf("got %d", got)
	}
}
