package store

import "testing"

func TestWeightedAverage(t *testing.T) {
	got, err := WeightedAverage(4, 100_000, 4, 300_000)
	if err != nil || got != 200_000 {
		t.Fatalf("mix: %d %v", got, err)
	}
	got, err = WeightedAverage(8, 200_000, 4, 0)
	if err != nil || got != 133_333 {
		t.Fatalf("pull down: %d %v", got, err)
	}
	if _, err := WeightedAverage(0, 0, 0, 100); err == nil {
		t.Fatal("zero units should fail")
	}
}
