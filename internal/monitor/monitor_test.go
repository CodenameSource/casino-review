package monitor

import (
	"strings"
	"testing"

	"casino-review/internal/templates"
)

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

// TestTemplatesNeverRetrigger is the permanent codification of the GIF-URL
// self-trigger loop: EVERY comment template the bot can post, rendered with
// worst-case sample data, must not match the trigger. Dispatch triggers are
// commands by design — but only for names that are NOT our own trigger, which
// the registry can't contain (asserted here too).
func TestTemplatesNeverRetrigger(t *testing.T) {
	const trig = "/casino-review"
	engineNames := []string{"tsetso-review", "dimoreview", "gigareview", "paranoid-sec", "eslint", "tsc"}
	gifURL := "https://raw.githubusercontent.com/owner/repo/casino-review-assets/casino/123-456-789.gif"

	for name, body := range templates.All(trig, gifURL, engineNames) {
		isDispatch := strings.HasPrefix(name, "DispatchTrigger:")
		if isDispatch {
			// A dispatch trigger is allowed to be a command — for its own bot.
			// It must still never be OUR trigger.
			if MatchesTrigger(body, trig) {
				t.Errorf("template %s (%q) matches our own trigger — self-trigger loop", name, body)
			}
			continue
		}
		if MatchesTrigger(body, trig) {
			t.Errorf("template %s matches the trigger — self-trigger loop:\n%s", name, body)
		}
		// Also must not accidentally invoke any dispatch bot.
		for _, en := range engineNames {
			if MatchesTrigger(body, "/"+en) {
				t.Errorf("template %s matches dispatch command /%s:\n%s", name, en, body)
			}
		}
	}
}
