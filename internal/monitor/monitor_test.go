package monitor

import "testing"

func TestMatchesTrigger(t *testing.T) {
	const trig = "/casino-review"
	cases := []struct {
		body string
		want bool
	}{
		{"/casino-review", true},
		{"/casino-review please go", true},
		{"  /casino-review", true},                   // leading whitespace
		{"some intro\n/casino-review\nthanks", true}, // command on its own line
		{"", false},
		{"/dimoreview", false}, // a different command
		{"please run /casino-review later", false},     // mid-sentence mention, not a command
		{"see the casino-review-assets branch", false}, // prose containing the substring
		// The loop bug: our own GIF comment embeds a URL on casino-review-assets.
		{"![🎰](https://raw.githubusercontent.com/mandel-ai/mandel/casino-review-assets/casino/3606-4808929165.gif?token=X)", false},
		// The "/<winner>" comment we post must never re-trigger.
		{"/gigareview", false},
	}
	for _, c := range cases {
		if got := MatchesTrigger(c.body, trig); got != c.want {
			t.Errorf("MatchesTrigger(%q) = %v, want %v", c.body, got, c.want)
		}
	}
}
