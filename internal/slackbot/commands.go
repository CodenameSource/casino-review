// Package slackbot is the market's human surface: a Socket-Mode Slack app
// honored in exactly one channel, driving market.Service, plus a tailer that
// posts notifications for market events that happened on other surfaces.
package slackbot

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Command is a parsed /casino invocation.
type Command struct {
	Name     string // fund|market|bet|board|refund|link|lock|resolve|void|help
	Context  string // raw context input (#123, pr:o/r#1, ext:KEY)
	Kind     string
	MarketID int64
	Outcome  string
	Amount   string            // raw, parsed by ledger.ParseUSDC
	Args     map[string]string // key=value extras (solver=login, reason=?)
	Rest     string
}

// Parse turns the slash-command text into a Command. Pure — unit tested.
func Parse(text string) (Command, error) {
	f := strings.Fields(text)
	if len(f) == 0 {
		return Command{Name: "help"}, nil
	}
	cmd := Command{Name: strings.ToLower(f[0]), Args: map[string]string{}}
	rest := f[1:]

	// Collect key=value pairs anywhere in the tail. Keys must be purely
	// alphabetic (solver=…, reason=…) so tokens like "ext:KEY=1" — a
	// legitimate context ref — are never eaten as arguments.
	var plain []string
	for _, tok := range rest {
		if k, v, ok := strings.Cut(tok, "="); ok && isAlphaKey(k) && v != "" {
			cmd.Args[strings.ToLower(k)] = v
			continue
		}
		plain = append(plain, tok)
	}

	need := func(n int, usage string) error {
		if len(plain) < n {
			return fmt.Errorf("usage: `/casino %s`", usage)
		}
		return nil
	}
	parseID := func(s string) (int64, error) {
		id, err := strconv.ParseInt(strings.TrimPrefix(s, "#"), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not a market id", s)
		}
		return id, nil
	}

	switch cmd.Name {
	case "help", "board":
		return cmd, nil
	case "fund":
		if err := need(2, "fund <#pr|ext:KEY> <amount>"); err != nil {
			return cmd, err
		}
		cmd.Context, cmd.Amount = plain[0], plain[1]
	case "market":
		if err := need(2, "market <#pr|ext:KEY> <bounty|merge-by|findings-count> [deadline]"); err != nil {
			return cmd, err
		}
		cmd.Context, cmd.Kind = plain[0], strings.ToLower(plain[1])
		if len(plain) > 2 {
			cmd.Rest = plain[2]
		}
	case "bet":
		if err := need(3, "bet <market-id> <outcome> <amount>"); err != nil {
			return cmd, err
		}
		id, err := parseID(plain[0])
		if err != nil {
			return cmd, err
		}
		cmd.MarketID, cmd.Outcome, cmd.Amount = id, plain[1], plain[2]
	case "refund", "lock", "void":
		if err := need(1, cmd.Name+" <market-id>"); err != nil {
			return cmd, err
		}
		id, err := parseID(plain[0])
		if err != nil {
			return cmd, err
		}
		cmd.MarketID = id
		if len(plain) > 1 {
			cmd.Rest = strings.Join(plain[1:], " ")
		}
	case "resolve":
		if err := need(2, "resolve <market-id> <outcome> [solver=<github-login>]"); err != nil {
			return cmd, err
		}
		id, err := parseID(plain[0])
		if err != nil {
			return cmd, err
		}
		cmd.MarketID, cmd.Outcome = id, plain[1]
	case "link":
		if err := need(1, "link <github-login>"); err != nil {
			return cmd, err
		}
		cmd.Rest = strings.TrimPrefix(plain[0], "@")
	default:
		return cmd, fmt.Errorf("unknown command %q — try `/casino help`", cmd.Name)
	}
	return cmd, nil
}

func isAlphaKey(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}

// ParseDeadline accepts a duration ("72h", "3d" not supported — go durations)
// or RFC3339, returning RFC3339 (the stored form).
func ParseDeadline(s string, now time.Time) (string, error) {
	if s == "" {
		return "", fmt.Errorf("merge-by needs a deadline, e.g. 72h or 2026-07-10T00:00:00Z")
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return "", fmt.Errorf("deadline must be in the future")
		}
		return now.Add(d).UTC().Format(time.RFC3339), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Format(time.RFC3339), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC().Format(time.RFC3339), nil
	}
	return "", fmt.Errorf("can't parse deadline %q (use 72h, 2026-07-10, or RFC3339)", s)
}

const helpText = "🎰 *casino market* — stake USDC on questions about PRs\n" +
	"`/casino fund #123 25` — bounty: pool pays the author on merge\n" +
	"`/casino market #123 merge-by 72h` — open a merge-deadline market\n" +
	"`/casino market #123 findings-count` — bet on the review's findings count\n" +
	"`/casino bet 7 yes 10` — stake $10 on outcome *yes* of market 7\n" +
	"`/casino board` — the ranked board\n" +
	"`/casino refund 7` — withdraw your stake (while the market is open)\n" +
	"`/casino link <github-login>` — link your GitHub identity\n" +
	"admin: `/casino lock 7` · `/casino resolve 7 merged solver=<login>` · `/casino void 7`"
