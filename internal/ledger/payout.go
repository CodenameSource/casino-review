package ledger

import (
	"math/big"
	"sort"
)

// Payout is one computed award. Pure data — persistence happens in the
// resolve transaction.
type Payout struct {
	Payee  string
	Amount USDC
	Reason string // "solver" | "parimutuel-win" | "dust" | "refund"
}

// stake is the slice of a position the payout math needs.
type stake struct {
	ID          int64
	Participant string
	Outcome     string
	Amount      USDC
}

// computeSolverPayout: the whole pool goes to the solver (bounty rule).
func computeSolverPayout(stakes []stake, solver string) []Payout {
	var pool USDC
	for _, s := range stakes {
		pool += s.Amount
	}
	if pool == 0 {
		return nil
	}
	return []Payout{{Payee: solver, Amount: pool, Reason: "solver"}}
}

// computeParimutuelPayouts splits the WHOLE pool among the winning outcome's
// stakes pro-rata. Integer math floors each share; the leftover micro-USDC
// dust goes to 'house' so every unit in equals every unit out — auditable to
// the micro. A winner with multiple positions is paid per position.
//
// No winners ⇒ nil (the caller refunds everyone instead).
func computeParimutuelPayouts(stakes []stake, winning string) []Payout {
	var pool, winPool USDC
	var winners []stake
	for _, s := range stakes {
		pool += s.Amount
		if s.Outcome == winning {
			winPool += s.Amount
			winners = append(winners, s)
		}
	}
	if winPool == 0 || pool == 0 {
		return nil
	}
	// Deterministic order: by position ID.
	sort.Slice(winners, func(i, j int) bool { return winners[i].ID < winners[j].ID })

	var out []Payout
	var distributed USDC
	poolBig, winBig := big.NewInt(int64(pool)), big.NewInt(int64(winPool))
	for _, w := range winners {
		// share = floor(pool * stake / winPool) — exact via big.Int (the naive
		// int64 multiply overflows for large pools).
		share := new(big.Int).Mul(poolBig, big.NewInt(int64(w.Amount)))
		share.Quo(share, winBig)
		s := USDC(share.Int64())
		if s <= 0 {
			continue
		}
		out = append(out, Payout{Payee: w.Participant, Amount: s, Reason: "parimutuel-win"})
		distributed += s
	}
	if dust := pool - distributed; dust > 0 {
		out = append(out, Payout{Payee: "house", Amount: dust, Reason: "dust"})
	}
	return out
}
