package slackbot

import (
	"testing"

	"casino-review/internal/ledger"
	"casino-review/internal/market"
)

func sampleDetail(kind, state string, outcomes []string, pools, mine map[string]ledger.USDC) market.Detail {
	var pool ledger.USDC
	for _, v := range pools {
		pool += v
	}
	return market.Detail{
		Market: ledger.Market{
			ID: 1, Kind: kind, ContextRef: "pr:o/r#1",
			Question: "q?", Outcomes: outcomes, State: state,
		},
		Pool: pool, Backers: 2, OutcomePools: pools, MyStake: mine,
	}
}

// TestBlockBuilders exercises every render path — including a market with no
// takers (zero pools) and each lifecycle state — to catch nil-map derefs and
// divide-by-zero in the odds math before it ships to Slack.
func TestBlockBuilders(t *testing.T) {
	cases := []market.Detail{
		sampleDetail("merge-by", ledger.StateOpen, []string{"yes", "no"},
			map[string]ledger.USDC{"yes": 30_000_000, "no": 20_000_000}, map[string]ledger.USDC{"yes": 30_000_000}),
		sampleDetail("merge-by", ledger.StateOpen, []string{"yes", "no"}, map[string]ledger.USDC{}, nil), // no bets yet
		sampleDetail("findings-count", ledger.StateLocked, []string{"0", "1-2", "3-5", "6+"},
			map[string]ledger.USDC{"0": 5_000_000}, nil),
		sampleDetail("bounty", ledger.StateOpen, []string{"merged"},
			map[string]ledger.USDC{"merged": 15_000_000}, map[string]ledger.USDC{"merged": 15_000_000}),
		sampleDetail("merge-by", ledger.StateResolved, []string{"yes", "no"},
			map[string]ledger.USDC{"yes": 10_000_000}, nil),
	}

	if len(boardBlocks(nil)) == 0 {
		t.Fatal("empty board should still render a block")
	}
	if len(boardBlocks(cases)) == 0 {
		t.Fatal("board blocks empty")
	}
	if len(prDashboardBlocks("pr:o/r#1", nil)) == 0 {
		t.Fatal("empty dashboard should still render a block")
	}
	if len(prDashboardBlocks("pr:o/r#1", cases)) == 0 {
		t.Fatal("dashboard blocks empty")
	}
	for _, d := range cases {
		if len(marketDetailBlocks(d)) == 0 {
			t.Fatalf("detail blocks empty for %s/%s", d.Market.Kind, d.Market.State)
		}
		if len(betModal(d.Market).Blocks.BlockSet) == 0 {
			t.Fatalf("bet modal empty for %s", d.Market.Kind)
		}
	}
}
