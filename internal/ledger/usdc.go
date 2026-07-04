// Package ledger is the money source of truth: markets, positions, payouts,
// and the guarded state machine that moves value between them. Amounts are
// int64 micro-USDC (6 decimals) — the exact scaling of on-chain USDC, so the
// future Base escrow swap is a unit-preserving move.
package ledger

import (
	"fmt"
	"strings"
)

// USDC is an amount in micro-USDC (1 USDC = 1_000_000).
type USDC int64

const microPerUSDC = 1_000_000

// maxParse caps a single parsed amount at 1B USDC — far above any sane stake,
// far below int64 overflow territory even summed across a market.
const maxParse = USDC(1_000_000_000) * microPerUSDC

// ParseUSDC accepts "25", "25.5", "$25.50", "0.000001" (min one micro-USDC).
func ParseUSDC(s string) (USDC, error) {
	orig := s
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "$"))
	if s == "" {
		return 0, fmt.Errorf("empty amount")
	}
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	if len(frac) > 6 {
		return 0, fmt.Errorf("%q: more than 6 decimal places", orig)
	}
	frac += strings.Repeat("0", 6-len(frac))

	// Each part is bounded BEFORE scaling: the whole part in whole USDC, the
	// fraction in micro. Bounding only the digit accumulator against maxParse
	// (micro units) would let a 13+-digit whole part overflow int64 in the
	// `*1e6` scale step and silently wrap to an arbitrary accepted amount.
	var total USDC
	for _, part := range []struct {
		digits string
		scale  USDC
		limit  USDC
	}{{whole, microPerUSDC, maxParse / microPerUSDC}, {frac, 1, maxParse}} {
		var v USDC
		for _, ch := range part.digits {
			if ch < '0' || ch > '9' {
				return 0, fmt.Errorf("%q: not a number", orig)
			}
			v = v*10 + USDC(ch-'0')
			if v > part.limit {
				return 0, fmt.Errorf("%q: amount too large", orig)
			}
		}
		total += v * part.scale
	}
	if total > maxParse {
		return 0, fmt.Errorf("%q: amount too large", orig)
	}
	if total <= 0 {
		return 0, fmt.Errorf("%q: amount must be positive", orig)
	}
	return total, nil
}

// String renders "$12.50" style, trimming trailing fractional zeros.
func (u USDC) String() string {
	sign := ""
	if u < 0 {
		sign, u = "-", -u
	}
	whole, frac := u/microPerUSDC, u%microPerUSDC
	if frac == 0 {
		return fmt.Sprintf("%s$%d", sign, whole)
	}
	f := strings.TrimRight(fmt.Sprintf("%06d", frac), "0")
	return fmt.Sprintf("%s$%d.%s", sign, whole, f)
}
