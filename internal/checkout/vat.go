package checkout

// InclusiveVAT splits a shelf price that already includes VAT.
// vat = gross * bps / (10000 + bps), rounded down. The leftover kobo stays in the net
// so net + vat = gross and the journal still balances.
func InclusiveVAT(gross int64, bps int) (vat, net int64) {
	if bps <= 0 || gross <= 0 {
		return 0, gross
	}
	vat = gross * int64(bps) / int64(10000+bps)
	return vat, gross - vat
}
