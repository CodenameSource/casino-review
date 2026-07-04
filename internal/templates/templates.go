// Package templates is the single place bot-authored comment bodies are
// composed. Centralizing them exists for one hard-won reason: every template
// must be provably unable to re-trigger the bot (the GIF-URL substring loop).
// A test runs every template against MatchesTrigger with every configured
// trigger and engine name — add a template here and the test covers it.
package templates

import (
	"fmt"
	"strings"
)

// SpinGIF is the comment containing only the slot-machine GIF.
func SpinGIF(gifURL string) string {
	return fmt.Sprintf("![🎰](%s)", gifURL)
}

// DispatchTrigger invokes an external review bot. It is the one template that
// IS a command on purpose — for the dispatched bot, never for ourselves; the
// safety test asserts it matches no name in OUR registry other than intended.
func DispatchTrigger(name string) string {
	return "/" + name
}

// Finding is the rendered form of one review finding.
type Finding struct {
	Path     string
	Line     int
	Severity string
	Title    string
	Body     string
}

// quote renders untrusted text (model output, analyzer output) as a markdown
// blockquote. This is trigger-neutralization, not decoration: every line's
// first token becomes ">", so no line of model-controlled text can ever be a
// command (MatchesTrigger works on first tokens of lines).
func quote(s string) string {
	return "> " + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n> ")
}

// ReviewFindings renders an engine's results as a PR comment. Summary, titles,
// and bodies are engine/model-controlled and therefore quoted or kept inline —
// never allowed to start a line raw.
func ReviewFindings(engine string, findings []Finding, summary string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## 🎰 Casino Review — `%s`\n\n", engine)
	if summary != "" {
		b.WriteString(quote(summary) + "\n\n")
	}
	if len(findings) == 0 {
		b.WriteString("No findings. The house pays out a clean bill of health. 🟢\n")
		return b.String()
	}
	fmt.Fprintf(&b, "**%d finding(s):**\n\n", len(findings))
	for i, f := range findings {
		loc := f.Path
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", f.Path, f.Line)
		}
		sev := f.Severity
		if sev == "" {
			sev = "note"
		}
		// Title is inline after our own tokens; newlines are stripped so a
		// multi-line title can't smuggle a fresh line start.
		title := strings.Join(strings.Fields(f.Title), " ")
		fmt.Fprintf(&b, "%d. **[%s]** `%s` — %s\n", i+1, sev, loc, title)
		if f.Body != "" {
			b.WriteString(quote(f.Body) + "\n")
		}
	}
	return b.String()
}

// ReviewError reports a failed engine run on the PR. The error text may embed
// subprocess output — quoted for the same trigger-neutralization reason.
func ReviewError(engine string, err error) string {
	return fmt.Sprintf("## 🎰 Casino Review — `%s`\n\n⚠️ The reel jammed:\n\n%s", engine, quote(err.Error()))
}

// All returns every template rendered with adversarial sample data — model
// text that actively tries to start lines with commands — for the
// trigger-safety test. Update when adding a template.
func All(sampleTrigger, sampleGifURL string, engineNames []string) map[string]string {
	hostileBody := sampleTrigger + " on a fresh line\nand /" + strings.TrimPrefix(engineNames[0], "/") + " too\nsee " + sampleGifURL
	out := map[string]string{
		"SpinGIF": SpinGIF(sampleGifURL),
		"ReviewFindingsNone": ReviewFindings("some-engine", nil,
			sampleTrigger+" as the very first token of the summary"),
		"ReviewFindingsSome": ReviewFindings("some-engine", []Finding{{
			Path: "src/casino-review.ts", Line: 3, Severity: "high",
			Title: "multi\nline title with\n" + sampleTrigger + " inside",
			Body:  hostileBody,
		}}, "summary mentioning "+sampleTrigger+"-assets paths"),
		"ReviewError": ReviewError("some-engine", fmt.Errorf("analyzer said:\n%s\nexit status 2", sampleTrigger)),
	}
	for _, n := range engineNames {
		out["DispatchTrigger:"+n] = DispatchTrigger(n)
	}
	return out
}
