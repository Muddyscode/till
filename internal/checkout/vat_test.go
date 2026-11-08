package checkout

import "testing"

func TestInclusiveVATSerumPrice(t *testing.T) {
	vat, net := InclusiveVAT(450_000, 750)
	if vat != 31395 || net != 418605 || vat+net != 450_000 {
		t.Fatalf("vat=%d net=%d", vat, net)
	}
}

func TestInclusiveVATZeroRate(t *testing.T) {
	vat, net := InclusiveVAT(450_000, 0)
	if vat != 0 || net != 450_000 {
		t.Fatalf("vat=%d net=%d", vat, net)
	}
}

func TestInclusiveVATTinyGrossDoesNotEatTheKobo(t *testing.T) {
	vat, net := InclusiveVAT(1, 750)
	if vat != 0 || net != 1 {
		t.Fatalf("vat=%d net=%d", vat, net)
	}
}
