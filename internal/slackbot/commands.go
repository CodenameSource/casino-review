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
	Name     string // help|board|show|me|fund|market|bet|refund|link|prs|lock|resolve|void
	Context  string // raw context input (#123, pr:o/r#1, ext:KEY)
	Kind     string
	MarketID int64
	Outcome  string
	Amount   string            // raw, parsed by ledger.ParseUSDC
	Args     map[string]string // key=value extras (solver=login)
	Rest     string
}

// aliases map friendlier / casino-flavored verbs onto the canonical command.
var aliases = map[string]string{
	"markets": "board",
	"open":    "market", // "open a market" reads better than "market" as a verb
	"cashout": "refund", // casino-native for pulling your stake
	"cash":    "refund",
	"mine":    "me",
	"status":  "prs",
}

// Parse turns the slash-command text into a Command. Pure — unit tested.
func Parse(text string) (Command, error) {
	f := strings.Fields(text)
	if len(f) == 0 {
		return Command{Name: "help"}, nil
	}
	cmd := Command{Name: strings.ToLower(f[0]), Args: map[string]string{}}
	if canon, ok := aliases[cmd.Name]; ok {
		cmd.Name = canon
	}
	rest := f[1:]

	// key=value pairs anywhere in the tail. Keys must be purely alphabetic
	// (solver=…), so a context ref like "ext:KEY=1" is never eaten as an arg.
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
			return 0, fmt.Errorf("%q isn't a market number — see `/casino board`", s)
		}
		return id, nil
	}

	switch cmd.Name {
	case "help", "board", "me", "prs":
		return cmd, nil

	case "show":
		// show <#pr|ext:KEY> → the PR's market dashboard;  show <market#> → one market.
		if err := need(1, "show <#pr|market#>"); err != nil {
			return cmd, err
		}
		if isContextRef(plain[0]) {
			cmd.Context = plain[0]
		} else {
			id, err := parseID(plain[0])
			if err != nil {
				return cmd, err
			}
			cmd.MarketID = id
		}

	case "fund":
		if err := need(2, "fund <#pr|ext:KEY> <amount>"); err != nil {
			return cmd, err
		}
		cmd.Context, cmd.Amount = plain[0], plain[1]

	case "market":
		if err := need(2, "open <#pr|ext:KEY> <bounty|merge-by|findings-count> [deadline]"); err != nil {
			return cmd, err
		}
		cmd.Context, cmd.Kind = plain[0], strings.ToLower(plain[1])
		if len(plain) > 2 {
			cmd.Rest = plain[2]
		}

	case "bet":
		// context form: bet <#pr> <kind> <outcome> <amount>
		// id form:      bet <market#> <outcome> <amount>
		if len(plain) > 0 && isContextRef(plain[0]) {
			if err := need(4, "bet <#pr> <kind> <outcome> <amount>"); err != nil {
				return cmd, err
			}
			cmd.Context, cmd.Kind, cmd.Outcome, cmd.Amount = plain[0], strings.ToLower(plain[1]), plain[2], plain[3]
		} else {
			if err := need(3, "bet <#pr> <kind> <outcome> <amount>"); err != nil {
				return cmd, err
			}
			id, err := parseID(plain[0])
			if err != nil {
				return cmd, err
			}
			cmd.MarketID, cmd.Outcome, cmd.Amount = id, plain[1], plain[2]
		}

	case "refund", "lock", "void":
		// context form: <verb> <#pr> <kind> [reason]
		// id form:      <verb> <market#> [reason]
		if len(plain) > 0 && isContextRef(plain[0]) {
			if err := need(2, cmd.Name+" <#pr> <kind>"); err != nil {
				return cmd, err
			}
			cmd.Context, cmd.Kind = plain[0], strings.ToLower(plain[1])
			if len(plain) > 2 {
				cmd.Rest = strings.Join(plain[2:], " ")
			}
		} else {
			if err := need(1, cmd.Name+" <#pr> <kind>"); err != nil {
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
		}

	case "resolve":
		// context form: resolve <#pr> <kind> <outcome>
		// id form:      resolve <market#> <outcome>
		if len(plain) > 0 && isContextRef(plain[0]) {
			if err := need(3, "resolve <#pr> <kind> <outcome> [solver=<github-login>]"); err != nil {
				return cmd, err
			}
			cmd.Context, cmd.Kind, cmd.Outcome = plain[0], strings.ToLower(plain[1]), plain[2]
		} else {
			if err := need(2, "resolve <#pr> <kind> <outcome> [solver=<github-login>]"); err != nil {
				return cmd, err
			}
			id, err := parseID(plain[0])
			if err != nil {
				return cmd, err
			}
			cmd.MarketID, cmd.Outcome = id, plain[1]
		}

	case "link":
		if err := need(1, "link <github-login>"); err != nil {
			return cmd, err
		}
		cmd.Rest = strings.TrimPrefix(plain[0], "@")

	default:
		return cmd, fmt.Errorf("no such command `%s` — try `/casino help`", cmd.Name)
	}
	return cmd, nil
}

// isContextRef reports whether a token names a context (a PR or ext: key) rather
// than a bare market serial. "#123", "ext:KEY", "pr:o/r#1", "o/r#5" are context
// refs; a plain number ("7") is a market id (the hidden fallback address).
func isContextRef(s string) bool {
	return strings.HasPrefix(s, "#") || strings.ContainsAny(s, ":/")
}

// ParseDeadline accepts a Go duration ("72h") or a date/RFC3339, returning the
// stored RFC3339 form.
func ParseDeadline(s string, now time.Time) (string, error) {
	if s == "" {
		return "", fmt.Errorf("merge-by needs a deadline, e.g. `72h` or `2026-07-10`")
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return "", fmt.Errorf("the deadline must be in the future")
		}
		return now.Add(d).UTC().Format(time.RFC3339), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Format(time.RFC3339), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC().Format(time.RFC3339), nil
	}
	return "", fmt.Errorf("can't read deadline %q — use `72h`, `2026-07-10`, or RFC3339", s)
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

const helpText = "🎰 *Welcome to the Casino* — stake USDC on what happens to pull requests.\n" +
	"Everything is addressed by the *PR* — you never need a market number.\n\n" +
	"*See what's live*\n" +
	"• `/casino board` — open markets & where the money is (tap 🎲 to bet)\n" +
	"• `/casino show #123` — every market on PR #123: odds + your position\n" +
	"• `/casino me` — your open bets\n\n" +
	"*Put money down*\n" +
	"• `/casino fund #123 25` — 💰 *bounty*: the whole pool pays the PR author when it merges\n" +
	"• `/casino open #123 merge-by 72h` — 📅 will it merge in time? (yes / no)\n" +
	"• `/casino open #123 findings-count` — 🔎 how many findings will the review post?\n" +
	"• `/casino bet #123 merge-by yes 10` — 🎲 stake $10 on *yes* of #123's merge-by\n" +
	"• `/casino refund #123 merge-by` — ↩️ pull your stake back (while it's open)\n\n" +
	"_Tip: tap 🎲 *Bet* / 📊 *Details* on the board instead of typing._\n\n" +
	"*You*\n" +
	"• `/casino link octocat` — link your GitHub login so bounties can pay you\n" +
	"• `/casino prs` — PRs the casino has reviewed\n\n" +
	"_Amounts accept `$` and decimals (`$10.50`). Admins settle markets with `lock` · `resolve` · `void`._"
