package market

import (
	"math"
	"testing"

	"casino-review/internal/ledger"
)

func TestOdds(t *testing.T) {
	outcomes := []string{"yes", "no"}
	pools := map[string]ledger.USDC{"yes": 30_000_000, "no": 20_000_000} // $30 / $20

	got := Odds(outcomes, pools)
	if len(got) != 2 {
		t.Fatalf("want 2 outcomes, got %d", len(got))
	}
	// order is preserved from the declared outcomes
	if got[0].Outcome != "yes" || got[1].Outcome != "no" {
		t.Fatalf("outcome order not preserved: %+v", got)
	}
	// yes: 30/50 share, 50/30 payout
	if !approx(got[0].Prob, 0.6) || !approx(got[0].PayoutX, 50.0/30.0) {
		t.Errorf("yes: prob=%v payoutX=%v", got[0].Prob, got[0].PayoutX)
	}
	if !approx(got[1].Prob, 0.4) || !approx(got[1].PayoutX, 50.0/20.0) {
		t.Errorf("no: prob=%v payoutX=%v", got[1].Prob, got[1].PayoutX)
	}
}

func TestOddsEmpty(t *testing.T) {
	// No stake anywhere: probabilities and payouts stay zero, never NaN/Inf.
	got := Odds([]string{"yes", "no"}, map[string]ledger.USDC{})
	for _, o := range got {
		if o.Prob != 0 || o.PayoutX != 0 {
			t.Errorf("%s: want zeroed odds on empty market, got prob=%v payoutX=%v", o.Outcome, o.Prob, o.PayoutX)
		}
	}
}

func TestOddsOneSided(t *testing.T) {
	// All money on "yes": yes is certain (prob 1, payout 1x), no has no takers.
	got := Odds([]string{"yes", "no"}, map[string]ledger.USDC{"yes": 10_000_000})
	if !approx(got[0].Prob, 1) || !approx(got[0].PayoutX, 1) {
		t.Errorf("yes should be prob 1 payout 1x, got %+v", got[0])
	}
	if got[1].Prob != 0 || got[1].PayoutX != 0 {
		t.Errorf("no should have zeroed odds (no takers), got %+v", got[1])
	}
}

func TestLine(t *testing.T) {
	pools := map[string]ledger.USDC{"yes": 30_000_000, "no": 20_000_000}
	if got, want := Line([]string{"yes", "no"}, pools), "yes $30 (60%) · no $20 (40%)"; got != want {
		t.Errorf("Line = %q, want %q", got, want)
	}
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
