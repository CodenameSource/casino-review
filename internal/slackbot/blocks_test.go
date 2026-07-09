package slackbot

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"casino-review/internal/ledger"
	"casino-review/internal/market"
)

func TestCountdown(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { tt := now.Add(d); return &tt }
	cases := []struct {
		in            *time.Time
		want          string
		overdue, okay bool
	}{
		{nil, "", false, false},
		{at(-time.Hour), "overdue", true, true},
		{at(30 * time.Minute), "in 30m", false, true},
		{at(20 * time.Second), "in 1m", false, true}, // never "in 0m"
		{at(5 * time.Hour), "in 5h", false, true},
		{at(50 * time.Hour), "in 2d", false, true},
	}
	for _, c := range cases {
		got, ov, ok := countdown(c.in, now)
		if got != c.want || ov != c.overdue || ok != c.okay {
			t.Errorf("countdown = (%q,%v,%v), want (%q,%v,%v)", got, ov, ok, c.want, c.overdue, c.okay)
		}
	}
}

func TestConditionLineNoLeak(t *testing.T) {
	// The authored per-kind line must not leak the long ref or a raw timestamp.
	m := ledger.Market{Kind: "merge-by", ContextRef: "pr:mandel-ai/mandel#3664",
		Question: "Will pr:mandel-ai/mandel#3664 be merged by 2026-07-11T09:46:52Z?"}
	got := conditionLine(m)
	if strings.Contains(got, "mandel-ai/mandel") || strings.Contains(got, "2026-07-11T") {
		t.Errorf("conditionLine leaked raw ref/timestamp: %q", got)
	}
	if !strings.Contains(got, "#3664") {
		t.Errorf("conditionLine should reference #3664: %q", got)
	}
	// Unknown kind falls back to a scrubbed stored question.
	u := ledger.Market{Kind: "weird", ContextRef: "pr:o/r#5", Question: "Odd pr:o/r#5 by 2026-07-11T09:46:52Z"}
	if q := conditionLine(u); strings.Contains(q, "o/r#5") || strings.Contains(q, "2026-07-11T") {
		t.Errorf("cleanQuestion fallback leaked: %q", q)
	}
}

func TestPayoutLine(t *testing.T) {
	// One-sided pool: warns, shows payout, never a bare "100%".
	one := sampleDetail("merge-by", ledger.StateOpen, []string{"yes", "no"},
		map[string]ledger.USDC{"yes": 5_000_000}, nil)
	got := payoutLine(one)
	if !strings.Contains(got, "pays") || !strings.Contains(got, "one side") {
		t.Errorf("one-sided payout should show payout + warning: %q", got)
	}
	if strings.Contains(got, "100%") {
		t.Errorf("payout line must not show a bare percentage: %q", got)
	}
	// Two-sided: no warning.
	two := sampleDetail("merge-by", ledger.StateOpen, []string{"yes", "no"},
		map[string]ledger.USDC{"yes": 30_000_000, "no": 20_000_000}, nil)
	if strings.Contains(payoutLine(two), "one side") {
		t.Errorf("two-sided payout should not warn: %q", payoutLine(two))
	}
}

func TestSlackDate(t *testing.T) {
	got := slackDate(time.Date(2026, 7, 11, 9, 46, 0, 0, time.UTC))
	if !strings.HasPrefix(got, "<!date^") || !strings.Contains(got, "{date_short_pretty}") || !strings.Contains(got, "|") {
		t.Errorf("slackDate malformed: %q", got)
	}
}

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
		m := d.Market
		if len(marketCard(d)) == 0 {
			t.Fatalf("card empty for %s/%s", m.Kind, m.State)
		}
		if len(marketDetailBlocks(d)) == 0 {
			t.Fatalf("detail blocks empty for %s/%s", m.Kind, m.State)
		}
		outcome := "merged"
		if len(m.Outcomes) > 0 {
			outcome = m.Outcomes[0]
		}
		if len(betModal(m).Blocks.BlockSet) == 0 {
			t.Fatalf("bet modal empty for %s", m.Kind)
		}
		if len(betAmountModal(m, outcome).Blocks.BlockSet) == 0 {
			t.Fatalf("amount modal empty for %s", m.Kind)
		}
		if len(betCustomModal(m, outcome).Blocks.BlockSet) == 0 {
			t.Fatalf("custom modal empty for %s", m.Kind)
		}
		if len(betDoneModal(m, outcome, 5_000_000, d.Pool).Blocks.BlockSet) == 0 {
			t.Fatalf("done modal empty for %s", m.Kind)
		}
		// action_ids must be unique within each actions block, and the fast-path
		// row must not exceed Slack's 5-element limit.
		assertActionBlocksValid(t, marketCard(d))
		assertActionBlocksValid(t, marketDetailBlocks(d))
	}

	// A market with >10 outcomes must not use radio buttons (Slack caps them at
	// 10) — betModal switches to a static select above that.
	big := make([]string, 12)
	for i := range big {
		big[i] = strconv.Itoa(i)
	}
	bigModal := betModal(ledger.Market{ID: 9, Kind: "findings-count", ContextRef: "pr:o/r#9", Outcomes: big, State: ledger.StateOpen})
	if len(bigModal.Blocks.BlockSet) == 0 {
		t.Fatal("big-outcome bet modal empty")
	}
	for _, blk := range bigModal.Blocks.BlockSet {
		if in, ok := blk.(*slack.InputBlock); ok {
			if _, isRadio := in.Element.(*slack.RadioButtonsBlockElement); isRadio && len(big) > 10 {
				t.Fatalf("betModal used radio buttons for %d outcomes (Slack caps at 10)", len(big))
			}
		}
	}

	// creation / onboarding / home / me builders. Cover both the free-text
	// fallback (no PRs) and the PR-dropdown path, and a very long PR title that
	// must be truncated to Slack's option limits.
	prs := []prChoice{
		{Ref: "#3664", Title: "Add the thing", Desc: "by @octocat"},
		{Ref: "#42", Title: strings.Repeat("very long title ", 20), Desc: strings.Repeat("desc ", 40)},
		{Ref: "#7", Title: "Draft work", Desc: ""},
	}
	if len(newMarketModal("", nil).Blocks.BlockSet) == 0 || len(newMarketModal("ext:X", nil).Blocks.BlockSet) == 0 {
		t.Fatal("new-market modal (text) empty")
	}
	if len(newMarketModal("", prs).Blocks.BlockSet) == 0 || len(newMarketModal("#3664", prs).Blocks.BlockSet) == 0 {
		t.Fatal("new-market modal (picker) empty")
	}
	if got := truncate(strings.Repeat("x", 200), 75); len([]rune(got)) > 75 {
		t.Fatalf("truncate produced %d runes, want ≤75", len([]rune(got)))
	}
	if got := prRef("pr:o/r#3664"); got != "#3664" {
		t.Fatalf("prRef(pr:o/r#3664) = %q, want #3664", got)
	}
	// A PR-dashboard prefill (normalized ref) preselects the matching option.
	mdl := newMarketModal("pr:o/r#3664", prs)
	var prSel *slack.SelectBlockElement
	for _, blk := range mdl.Blocks.BlockSet {
		if in, ok := blk.(*slack.InputBlock); ok && in.BlockID == blkNewPR {
			prSel, _ = in.Element.(*slack.SelectBlockElement)
		}
	}
	if prSel == nil || prSel.InitialOption == nil || prSel.InitialOption.Value != "#3664" {
		t.Fatalf("prefill should preselect #3664 in the picker, got %+v", prSel)
	}
	if len(linkModal().Blocks.BlockSet) == 0 {
		t.Fatal("link modal empty")
	}
	if len(welcomeBlocks(true)) == 0 || len(welcomeBlocks(false)) == 0 {
		t.Fatal("welcome blocks empty")
	}
	if len(meBlocks(nil, "")) == 0 {
		t.Fatal("empty me blocks should still render")
	}

	positions := []ledger.PositionView{
		{MarketID: 1, Kind: "merge-by", ContextRef: "pr:o/r#1", MarketState: ledger.StateOpen, Outcome: "yes", Amount: 30_000_000},
		{MarketID: 2, Kind: "bounty", ContextRef: "pr:o/r#2", MarketState: ledger.StateLocked, Outcome: "merged", Amount: 10_000_000},
	}
	if len(meBlocks(positions, "octocat")) == 0 {
		t.Fatal("me blocks empty")
	}
	if len(homeBlocks("", positions, cases)) == 0 || len(homeBlocks("octocat", nil, nil)) == 0 {
		t.Fatal("home blocks empty")
	}
	assertActionBlocksValid(t, homeBlocks("", positions, cases))
}

// assertActionBlocksValid checks Slack's per-actions-block rules: ≤5 elements
// and unique action_ids within the block.
func assertActionBlocksValid(t *testing.T, blocks []slack.Block) {
	t.Helper()
	for _, blk := range blocks {
		ab, ok := blk.(*slack.ActionBlock)
		if !ok || ab.Elements == nil {
			continue
		}
		els := ab.Elements.ElementSet
		if len(els) > 5 {
			t.Fatalf("actions block %q has %d elements (>5)", ab.BlockID, len(els))
		}
		seen := map[string]bool{}
		for _, e := range els {
			if btn, ok := e.(*slack.ButtonBlockElement); ok {
				if seen[btn.ActionID] {
					t.Fatalf("duplicate action_id %q in block %q", btn.ActionID, ab.BlockID)
				}
				seen[btn.ActionID] = true
			}
		}
	}
}
