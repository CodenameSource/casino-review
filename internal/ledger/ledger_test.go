package ledger

import (
	"strings"
	"testing"
)

func TestParseUSDC(t *testing.T) {
	good := map[string]USDC{
		"25":       25_000_000,
		"$25":      25_000_000,
		"25.5":     25_500_000,
		"$25.50":   25_500_000,
		"0.000001": 1,
		" $3 ":     3_000_000,
		".5":       500_000,
	}
	for in, want := range good {
		got, err := ParseUSDC(in)
		if err != nil || got != want {
			t.Errorf("ParseUSDC(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "$", "-5", "0", "0.0000001", "1e6", "12,50", "abc", "99999999999999999999",
		// int64-overflow regression: these used to WRAP and be accepted as
		// arbitrary small amounts ("18446744073710" parsed as $0.448384).
		"18446744073710", "9300000000000", "9223372036855", "1000000001"} {
		if _, err := ParseUSDC(bad); err == nil {
			t.Errorf("ParseUSDC(%q): expected error", bad)
		}
	}
	// The cap itself is fine.
	if v, err := ParseUSDC("1000000000"); err != nil || v != maxParse {
		t.Errorf("ParseUSDC(1e9 USDC) = %v, %v", v, err)
	}
}

func TestValidOutcomes(t *testing.T) {
	if err := validOutcomes([]string{"yes", "no"}); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string][]string{
		"empty-set":    {},
		"empty-string": {"yes", ""},
		"dup":          {"yes", "yes"},
		"case-dup":     {"yes", "Yes"},
		"too-long":     {strings.Repeat("x", 33)},
	} {
		if err := validOutcomes(bad); err == nil {
			t.Errorf("%s: expected error for %v", name, bad)
		}
	}
}

func TestUSDCString(t *testing.T) {
	cases := map[USDC]string{
		25_000_000: "$25",
		25_500_000: "$25.5",
		1:          "$0.000001",
		-2_500_000: "-$2.5",
	}
	for in, want := range cases {
		if got := in.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", in, got, want)
		}
	}
}

func TestComputeSolverPayout(t *testing.T) {
	stakes := []stake{
		{ID: 1, Participant: "slack:A", Outcome: "merged", Amount: 10_000_000},
		{ID: 2, Participant: "slack:B", Outcome: "merged", Amount: 5_000_000},
	}
	ps := computeSolverPayout(stakes, "github:solver")
	if len(ps) != 1 || ps[0].Amount != 15_000_000 || ps[0].Payee != "github:solver" {
		t.Fatalf("payouts = %+v", ps)
	}
	if ps := computeSolverPayout(nil, "github:solver"); ps != nil {
		t.Fatalf("empty pool should pay nothing, got %+v", ps)
	}
}

func TestComputeParimutuelPayouts(t *testing.T) {
	stakes := []stake{
		{ID: 1, Participant: "A", Outcome: "yes", Amount: 10_000_000},
		{ID: 2, Participant: "B", Outcome: "yes", Amount: 20_000_000},
		{ID: 3, Participant: "C", Outcome: "no", Amount: 30_000_000},
	}
	ps := computeParimutuelPayouts(stakes, "yes")
	// pool 60, winners 30 → A gets 20, B gets 40, no dust.
	if len(ps) != 2 {
		t.Fatalf("payouts = %+v", ps)
	}
	if ps[0].Payee != "A" || ps[0].Amount != 20_000_000 || ps[1].Payee != "B" || ps[1].Amount != 40_000_000 {
		t.Fatalf("payouts = %+v", ps)
	}

	// Conservation with dust: pool 100, winners 3 uneven stakes.
	stakes = []stake{
		{ID: 1, Participant: "A", Outcome: "yes", Amount: 1},
		{ID: 2, Participant: "B", Outcome: "yes", Amount: 1},
		{ID: 3, Participant: "C", Outcome: "yes", Amount: 1},
		{ID: 4, Participant: "D", Outcome: "no", Amount: 97},
	}
	ps = computeParimutuelPayouts(stakes, "yes")
	var total USDC
	for _, p := range ps {
		total += p.Amount
	}
	if total != 100 {
		t.Fatalf("conservation violated: distributed %d of 100 (%+v)", total, ps)
	}
	last := ps[len(ps)-1]
	if last.Payee != "house" || last.Reason != "dust" || last.Amount != 1 {
		t.Fatalf("expected 1 micro dust to house, got %+v", ps)
	}

	// No winners → nil (caller refunds).
	if ps := computeParimutuelPayouts(stakes, "maybe"); ps != nil {
		t.Fatalf("expected nil for no winners, got %+v", ps)
	}
}

// Conservation across many random-ish configurations: money in == money out.
func TestParimutuelConservation(t *testing.T) {
	outcomes := []string{"yes", "no", "0", "1-2"}
	for n := 1; n <= 40; n++ {
		var stakes []stake
		var pool USDC
		for i := 0; i < n; i++ {
			amt := USDC((i*7919+13)%997 + 1) // deterministic pseudo-random 1..997
			stakes = append(stakes, stake{
				ID: int64(i), Participant: string(rune('A' + i%26)),
				Outcome: outcomes[i%len(outcomes)], Amount: amt,
			})
			pool += amt
		}
		ps := computeParimutuelPayouts(stakes, "yes")
		if ps == nil {
			continue
		}
		var total USDC
		for _, p := range ps {
			total += p.Amount
		}
		if total != pool {
			t.Fatalf("n=%d: distributed %d of pool %d", n, total, pool)
		}
	}
}
