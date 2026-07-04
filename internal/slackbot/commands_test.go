package slackbot

import (
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want Command
		err  bool
	}{
		{in: "", want: Command{Name: "help"}},
		{in: "help", want: Command{Name: "help"}},
		{in: "board", want: Command{Name: "board"}},
		{in: "fund #123 25", want: Command{Name: "fund", Context: "#123", Amount: "25"}},
		{in: "fund ext:PROJ-42 $10.50", want: Command{Name: "fund", Context: "ext:PROJ-42", Amount: "$10.50"}},
		{in: "market #123 merge-by 72h", want: Command{Name: "market", Context: "#123", Kind: "merge-by", Rest: "72h"}},
		{in: "market #123 findings-count", want: Command{Name: "market", Context: "#123", Kind: "findings-count"}},
		{in: "bet 7 yes 10", want: Command{Name: "bet", MarketID: 7, Outcome: "yes", Amount: "10"}},
		{in: "bet #7 yes 10", want: Command{Name: "bet", MarketID: 7, Outcome: "yes", Amount: "10"}},
		{in: "refund 7", want: Command{Name: "refund", MarketID: 7}},
		{in: "lock 12", want: Command{Name: "lock", MarketID: 12}},
		{in: "void 12 dupe market", want: Command{Name: "void", MarketID: 12, Rest: "dupe market"}},
		{in: "link @octocat", want: Command{Name: "link", Rest: "octocat"}},
		{in: "resolve 7 merged solver=octocat", want: Command{Name: "resolve", MarketID: 7, Outcome: "merged"}},
		{in: "fund", err: true},
		{in: "bet 7 yes", err: true},
		{in: "bet seven yes 1", err: true},
		{in: "resolve 7", err: true},
		{in: "jackpot", err: true},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if c.err {
			if err == nil {
				t.Errorf("Parse(%q): expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("Parse(%q): %v", c.in, err)
			continue
		}
		if got.Name != c.want.Name || got.Context != c.want.Context || got.Kind != c.want.Kind ||
			got.MarketID != c.want.MarketID || got.Outcome != c.want.Outcome ||
			got.Amount != c.want.Amount || got.Rest != c.want.Rest {
			t.Errorf("Parse(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}

	// key=value extraction
	cmd, err := Parse("resolve 7 merged solver=octocat")
	if err != nil || cmd.Args["solver"] != "octocat" {
		t.Fatalf("solver arg: %+v %v", cmd, err)
	}

	// A context ref containing '=' must NOT be eaten as a key=value arg.
	cmd, err = Parse("fund ext:KEY=1 25")
	if err != nil || cmd.Context != "ext:KEY=1" || cmd.Amount != "25" {
		t.Fatalf("ext ref with '=': %+v %v", cmd, err)
	}
}

func TestParseDeadline(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if got, err := ParseDeadline("72h", now); err != nil || got != "2026-07-04T12:00:00Z" {
		t.Fatalf("72h → %q, %v", got, err)
	}
	if got, err := ParseDeadline("2026-07-10", now); err != nil || got != "2026-07-10T00:00:00Z" {
		t.Fatalf("date → %q, %v", got, err)
	}
	if _, err := ParseDeadline("-1h", now); err == nil {
		t.Fatal("negative duration should error")
	}
	if _, err := ParseDeadline("soon", now); err == nil {
		t.Fatal("garbage should error")
	}
}
