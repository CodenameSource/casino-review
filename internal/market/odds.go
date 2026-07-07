package market

import (
	"fmt"
	"strings"

	"casino-review/internal/ledger"
)

// OutcomeOdds is one outcome's current standing in a parimutuel market.
type OutcomeOdds struct {
	Outcome string
	Pool    ledger.USDC
	Prob    float64 // pool share = implied probability (0 if the market is empty)
	PayoutX float64 // $ returned per $1 if this outcome wins (0 = no takers yet)
}

// Odds computes each outcome's pool share and payout multiple from the current
// pools, in the market's declared outcome order.
func Odds(outcomes []string, pools map[string]ledger.USDC) []OutcomeOdds {
	var total ledger.USDC
	for _, o := range outcomes {
		total += pools[o]
	}
	out := make([]OutcomeOdds, 0, len(outcomes))
	for _, o := range outcomes {
		p := pools[o]
		oo := OutcomeOdds{Outcome: o, Pool: p}
		if total > 0 {
			oo.Prob = float64(p) / float64(total)
		}
		if p > 0 {
			oo.PayoutX = float64(total) / float64(p)
		}
		out = append(out, oo)
	}
	return out
}

// Line renders a compact one-liner of the outcomes for the board, e.g.
// "yes $30 (60%) · no $20 (40%)".
func Line(outcomes []string, pools map[string]ledger.USDC) string {
	odds := Odds(outcomes, pools)
	parts := make([]string, len(odds))
	for i, o := range odds {
		parts[i] = fmt.Sprintf("%s %s (%d%%)", o.Outcome, o.Pool, int(o.Prob*100+0.5))
	}
	return strings.Join(parts, " · ")
}
