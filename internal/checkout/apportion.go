package checkout

import (
	"math/big"
	"strings"

	"github.com/Muddyscode/till/internal/ledger"
	"github.com/Muddyscode/till/internal/store"
)

const (
	CodeDiscountLimit       = "discount_limit"
	CodeDiscountReason      = "discount_reason_required"
	CodeTenderMismatch      = "tender_mismatch"
	CodeRefundExceedsOrder  = "refund_exceeds_order"
	maxDiscountReasonLength = 200
	taxStandard             = "standard"
)

// mulDiv is floor(a*b/c) without overflow. a, b >= 0 and c > 0.
func mulDiv(a, b, c int64) int64 {
	x := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	x.Quo(x, big.NewInt(c))
	return x.Int64()
}

// apportion splits total across weights: every line but the last gets the
// floor of its weighted share and the last line takes the remainder. A share
// above its cap is clamped and the excess moves to earlier lines with room.
// total must not exceed the sum of caps.
func apportion(total int64, weights, caps []int64) []int64 {
	n := len(weights)
	out := make([]int64, n)
	if n == 0 || total <= 0 {
		return out
	}
	var sum int64
	for _, w := range weights {
		sum += w
	}
	var given int64
	for i := 0; i < n-1; i++ {
		out[i] = mulDiv(total, weights[i], sum)
		given += out[i]
	}
	out[n-1] = total - given
	var excess int64
	for i := n - 1; i >= 0; i-- {
		if out[i] > caps[i] {
			excess += out[i] - caps[i]
			out[i] = caps[i]
		}
	}
	for i := n - 1; i >= 0 && excess > 0; i-- {
		room := caps[i] - out[i]
		take := min(room, excess)
		out[i] += take
		excess -= take
	}
	return out
}

// enforceDiscount applies the reason rule and the per-role limit. Admin has no
// limit (the discount must still be below the shelf total, checked by totals).
func enforceDiscount(m store.Merchant, role string, shelf, discount int64, reason string) error {
	if discount <= 0 {
		return nil
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ledger.Err(CodeDiscountReason, "discount_reason is required when a discount is given")
	}
	if len(reason) > maxDiscountReasonLength {
		return ledger.Err(ledger.CodeInvalidOrder, "discount_reason is too long")
	}
	var limit int
	switch role {
	case "admin":
		return nil
	case "manager":
		limit = m.MaxDiscountBpsManager
	default:
		limit = m.MaxDiscountBpsCashier
	}
	lhs := new(big.Int).Mul(big.NewInt(discount), big.NewInt(10000))
	rhs := new(big.Int).Mul(big.NewInt(int64(limit)), big.NewInt(shelf))
	if lhs.Cmp(rhs) > 0 {
		return ledger.Err(CodeDiscountLimit, "discount is above the limit for your role")
	}
	return nil
}
