package slackbot

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"casino-review/internal/config"
	"casino-review/internal/ledger"
	"casino-review/internal/market"
	"casino-review/internal/store"
	"casino-review/internal/telemetry"
)

type Bot struct {
	cfg       *config.Config
	svc       *market.Service
	st        *store.Store
	tel       *telemetry.T
	api       *slack.Client
	sock      *socketmode.Client
	channelID string
}

func New(cfg *config.Config, svc *market.Service, st *store.Store, tel *telemetry.T) *Bot {
	api := slack.New(cfg.SlackBotToken, slack.OptionAppLevelToken(cfg.SlackAppToken))
	return &Bot{cfg: cfg, svc: svc, st: st, tel: tel, api: api, sock: socketmode.New(api)}
}

// Run connects Socket Mode and serves commands + the notification tailer
// until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) error {
	id, err := b.resolveChannel(ctx, b.cfg.SlackChannel)
	if err != nil {
		return fmt.Errorf("resolve SLACK_CHANNEL %q: %w", b.cfg.SlackChannel, err)
	}
	b.channelID = id
	log.Printf("slackbot: honoring /casino only in channel %s", id)

	// The bot must be IN the channel to reply and to post market activity —
	// chat.postEphemeral/postMessage fail with not_in_channel otherwise. Try to
	// self-join (works for public channels with the channels:join scope); on
	// failure (private channel, or missing scope) tell the operator to invite.
	if _, _, _, err := b.api.JoinConversationContext(ctx, id); err != nil {
		log.Printf("slackbot: could not auto-join %s (%v) — INVITE THE BOT: run `/invite @<your-bot>` in that channel (private channels must be invited; public channels also need the channels:join scope to self-join)", id, err)
	} else {
		log.Printf("slackbot: joined channel %s", id)
	}

	go b.tail(ctx)
	go b.eventLoop(ctx)
	return b.sock.RunContext(ctx)
}

func (b *Bot) eventLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-b.sock.Events:
			if !ok {
				return
			}
			switch evt.Type {
			case socketmode.EventTypeSlashCommand:
				cmd, ok := evt.Data.(slack.SlashCommand)
				if !ok {
					continue
				}
				// Ack immediately (Slack gives 3s); reply async.
				b.sock.Ack(*evt.Request)
				go b.handleSlash(ctx, cmd)
			case socketmode.EventTypeConnectionError:
				log.Printf("slackbot: connection error: %v", evt.Data)
			}
		}
	}
}

func (b *Bot) handleSlash(ctx context.Context, sc slack.SlashCommand) {
	if sc.Command != "/casino" {
		return
	}
	if sc.ChannelID != b.channelID {
		b.ephemeral(sc, fmt.Sprintf("🎰 The casino only operates in <#%s>.", b.channelID))
		return
	}
	participant := "slack:" + sc.UserID
	ctx = ledger.WithVia(ctx, "slack")
	b.tel.Track(participant, "slack_command", map[string]any{"text": firstWord(sc.Text)})

	reply, ephemeral := b.execute(ctx, sc, participant)
	if reply == "" {
		return
	}
	if ephemeral {
		b.ephemeral(sc, reply)
	} else {
		if _, _, err := b.api.PostMessage(b.channelID, slack.MsgOptionText(reply, false)); err != nil {
			log.Printf("slackbot: post: %v", err)
		}
	}
}

// execute runs a parsed command; returns the reply and whether it should be
// ephemeral (errors/help) instead of in-channel (market activity is public).
func (b *Bot) execute(ctx context.Context, sc slack.SlashCommand, participant string) (string, bool) {
	cmd, err := Parse(sc.Text)
	if err != nil {
		return "⚠️ " + err.Error(), true
	}

	switch cmd.Name {
	case "help":
		return helpText, true

	case "board":
		rows, err := b.svc.Board(ctx, 15)
		if err != nil {
			return "⚠️ " + err.Error(), true
		}
		return renderBoard(rows), false

	case "show":
		d, err := b.svc.Detail(ctx, cmd.MarketID, participant)
		if err != nil {
			return "⚠️ " + err.Error(), true
		}
		return renderMarketDetail(d), true

	case "me":
		positions, err := b.svc.MyPositions(ctx, participant)
		if err != nil {
			return "⚠️ " + err.Error(), true
		}
		login, _ := b.st.GithubLogin(ctx, sc.UserID)
		return renderMyPositions(positions, login), true

	case "prs":
		prs, err := b.st.TrackedPRs(ctx, b.cfg.RepoSlug(), 15)
		if err != nil {
			return "⚠️ " + err.Error(), true
		}
		pending, _ := b.st.PendingSpins(ctx)
		return renderPRs(b.cfg.RepoSlug(), prs, pending), true // ephemeral: a status query, not channel activity

	case "fund":
		amt, err := ledger.ParseUSDC(cmd.Amount)
		if err != nil {
			return "⚠️ " + err.Error(), true
		}
		m, err := b.svc.Fund(ctx, cmd.Context, participant, amt)
		if err != nil {
			return "⚠️ " + err.Error(), true
		}
		_, pool, _ := b.svc.Get(ctx, m.ID)
		return fmt.Sprintf("💰 <@%s> funded %s → *#%d* `%s` · pool now *%s*  ·  `/casino show %d`",
			sc.UserID, amt, m.ID, m.ContextRef, pool, m.ID), false

	case "market":
		spec := map[string]any{}
		if cmd.Kind == "merge-by" {
			deadline, err := ParseDeadline(cmd.Rest, time.Now())
			if err != nil {
				return "⚠️ " + err.Error(), true
			}
			spec["deadline"] = deadline
		}
		m, err := b.svc.Create(ctx, cmd.Kind, cmd.Context, participant, spec)
		if err != nil {
			return "⚠️ " + err.Error(), true
		}
		return fmt.Sprintf("🆕 Market *#%d* — %s %s\n_%s_\nOutcomes: `%s`\n🎲 `/casino bet %d <outcome> <amount>`",
			m.ID, kindEmoji(m.Kind), m.Kind, m.Question, strings.Join(m.Outcomes, "` `"), m.ID), false

	case "bet":
		amt, err := ledger.ParseUSDC(cmd.Amount)
		if err != nil {
			return "⚠️ " + err.Error(), true
		}
		if err := b.svc.Bet(ctx, cmd.MarketID, participant, cmd.Outcome, amt); err != nil {
			return "⚠️ " + err.Error(), true
		}
		_, pool, _ := b.svc.Get(ctx, cmd.MarketID)
		return fmt.Sprintf("🎲 <@%s> put %s on *%s* (*#%d*) · pool now *%s*  ·  `/casino show %d`",
			sc.UserID, amt, cmd.Outcome, cmd.MarketID, pool, cmd.MarketID), false

	case "refund":
		amt, err := b.svc.Refund(ctx, cmd.MarketID, participant)
		if err != nil {
			return "⚠️ " + err.Error(), true
		}
		return fmt.Sprintf("↩️ <@%s> withdrew %s from market #%d", sc.UserID, amt, cmd.MarketID), false

	case "link":
		if err := b.st.LinkIdentity(ctx, sc.UserID, cmd.Rest); err != nil {
			return "⚠️ " + err.Error(), true
		}
		// Public on purpose: identity claims route payouts, so the channel
		// should see them happen.
		return fmt.Sprintf("🔗 Linked <@%s> ↔ github:%s", sc.UserID, cmd.Rest), false

	case "lock", "resolve", "void":
		// MONEY AUTHORIZATION: settling verbs move other people's stakes, so
		// they are restricted to the configured admin allowlist. With no
		// admins configured the verbs are disabled in Slack entirely (the CLI
		// on the host remains the admin path). Without this gate, any channel
		// member could `/casino resolve <id> merged solver=<their-login>` and
		// pay themselves the pool.
		if !b.isAdmin(sc.UserID) {
			return "⛔ Only casino admins can lock/resolve/void markets (set SLACK_ADMINS).", true
		}
		switch cmd.Name {
		case "lock":
			if err := b.svc.Lock(ctx, cmd.MarketID, participant); err != nil {
				return "⚠️ " + err.Error(), true
			}
			return fmt.Sprintf("🔒 Market #%d locked — no more bets.", cmd.MarketID), false

		case "resolve":
			solver := strings.TrimPrefix(cmd.Args["solver"], "@")
			if solver != "" {
				solver = "github:" + strings.TrimPrefix(solver, "github:")
			}
			payouts, err := b.svc.Resolve(ctx, cmd.MarketID, cmd.Outcome, solver, participant,
				map[string]any{"resolved_via": "slack-admin"})
			if err != nil {
				return "⚠️ " + err.Error(), true
			}
			return fmt.Sprintf("🏁 Market #%d resolved: *%s*\n%s", cmd.MarketID, cmd.Outcome, renderPayouts(payouts)), false

		default: // void
			refunds, err := b.svc.Void(ctx, cmd.MarketID, participant, cmd.Rest)
			if err != nil {
				return "⚠️ " + err.Error(), true
			}
			return fmt.Sprintf("🚫 Market #%d voided — %d stake(s) refunded.", cmd.MarketID, len(refunds)), false
		}
	}
	return "⚠️ unhandled command", true
}

func (b *Bot) isAdmin(userID string) bool {
	for _, id := range b.cfg.SlackAdmins {
		if id == userID {
			return true
		}
	}
	return false
}

func (b *Bot) ephemeral(sc slack.SlashCommand, text string) {
	if _, err := b.api.PostEphemeral(sc.ChannelID, sc.UserID, slack.MsgOptionText(text, false)); err != nil {
		log.Printf("slackbot: ephemeral: %v", err)
	}
}

// resolveChannel accepts a channel ID (C…/G…) or a #name to look up.
func (b *Bot) resolveChannel(ctx context.Context, ch string) (string, error) {
	ch = strings.TrimSpace(ch)
	if ch == "" {
		return "", fmt.Errorf("SLACK_CHANNEL is required")
	}
	if !strings.HasPrefix(ch, "#") {
		return ch, nil
	}
	name := strings.TrimPrefix(ch, "#")
	cursor := ""
	for {
		chans, next, err := b.api.GetConversationsContext(ctx, &slack.GetConversationsParameters{
			Cursor: cursor, Limit: 200, Types: []string{"public_channel", "private_channel"},
		})
		if err != nil {
			if strings.Contains(err.Error(), "missing_scope") {
				return "", fmt.Errorf("looking up channel by #name needs the bot token scope channels:read (and groups:read for private channels) — add them and reinstall, OR set SLACK_CHANNEL to the channel ID (C0…) to skip the lookup: %w", err)
			}
			return "", err
		}
		for _, c := range chans {
			if c.Name == name {
				return c.ID, nil
			}
		}
		if next == "" {
			return "", fmt.Errorf("channel #%s not found (is the bot invited?)", name)
		}
		cursor = next
	}
}

func firstWord(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return ""
}

func kindEmoji(kind string) string {
	switch kind {
	case "bounty":
		return "💰"
	case "merge-by":
		return "📅"
	case "findings-count":
		return "🔎"
	}
	return "🎯"
}

func renderBoard(rows []ledger.BoardRow) string {
	if len(rows) == 0 {
		return "🎰 *No markets open yet — be the first.*\n" +
			"• `/casino fund #<pr> 25` — 💰 bounty the author on merge\n" +
			"• `/casino open #<pr> merge-by 72h` — 📅 bet on the merge deadline\n" +
			"`/casino help` for the full table."
	}
	var sb strings.Builder
	sb.WriteString("🎰 *The Board* — where the money's at")
	for _, r := range rows {
		lock := ""
		if r.Market.State == ledger.StateLocked {
			lock = " 🔒"
		}
		fmt.Fprintf(&sb, "\n\n*#%d* `%s` · %s %s · *%s* · %d backer(s)%s\n_%s_  →  `/casino show %d`",
			r.Market.ID, r.Market.ContextRef, kindEmoji(r.Market.Kind), r.Market.Kind,
			r.Pool, r.Participants, lock, r.Market.Question, r.Market.ID)
	}
	return sb.String()
}

// renderMarketDetail is the `/casino show <id>` view: odds, your stake, and how
// to act on it.
func renderMarketDetail(d market.Detail) string {
	m := d.Market
	var sb strings.Builder
	fmt.Fprintf(&sb, "*Market #%d* — %s %s · [%s]\n_%s_\nPool *%s* · %d backer(s)\n",
		m.ID, kindEmoji(m.Kind), m.Kind, m.State, m.Question, d.Pool, d.Backers)

	if m.Kind == "bounty" {
		if mine := d.MyStake["merged"]; mine > 0 {
			fmt.Fprintf(&sb, "Your stake: *%s*\n", mine)
		}
		if m.State == ledger.StateOpen {
			fmt.Fprintf(&sb, "\n💰 Add to the bounty: `/casino fund %s <amount>`", m.ContextRef)
		}
		return sb.String()
	}

	sb.WriteString("\n")
	for _, o := range market.Odds(m.Outcomes, d.OutcomePools) {
		payout := "—"
		if o.PayoutX > 0 {
			payout = fmt.Sprintf("~%.2f×", o.PayoutX)
		}
		fmt.Fprintf(&sb, "`%-10s` %s  (%d%%) · win pays %s\n", o.Outcome, o.Pool, int(o.Prob*100+0.5), payout)
	}
	var mine []string
	for _, o := range m.Outcomes {
		if v := d.MyStake[o]; v > 0 {
			mine = append(mine, fmt.Sprintf("*%s* %s", o, v))
		}
	}
	if len(mine) > 0 {
		fmt.Fprintf(&sb, "\nYour stake: %s\n", strings.Join(mine, ", "))
	}
	if m.State == ledger.StateOpen {
		fmt.Fprintf(&sb, "\n🎲 `/casino bet %d <outcome> <amount>`  ·  ↩️ `/casino refund %d`", m.ID, m.ID)
	}
	return sb.String()
}

// renderMyPositions is the `/casino me` view.
func renderMyPositions(ps []ledger.PositionView, githubLogin string) string {
	id := "not linked — `/casino link <github-login>` so bounties can pay you"
	if githubLogin != "" {
		id = "github:" + githubLogin
	}
	if len(ps) == 0 {
		return fmt.Sprintf("🎰 *Your bets* — %s\nNo open bets yet. See `/casino board`.", id)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "🎰 *Your bets* — %s", id)
	var total ledger.USDC
	for _, p := range ps {
		outcome := ""
		if p.Kind != "bounty" {
			outcome = "*" + p.Outcome + "* "
		}
		lock := ""
		if p.MarketState == ledger.StateLocked {
			lock = " 🔒"
		}
		fmt.Fprintf(&sb, "\n• *#%d* %s %s · %s%s · `%s`%s",
			p.MarketID, kindEmoji(p.Kind), p.Kind, outcome, p.Amount, p.ContextRef, lock)
		total += p.Amount
	}
	fmt.Fprintf(&sb, "\nTotal staked: *%s*", total)
	return sb.String()
}

func renderPRs(repo string, prs []store.TrackedPR, pending int) string {
	if len(prs) == 0 {
		msg := fmt.Sprintf("🎰 No PRs tracked yet for `%s`.", repo)
		if pending > 0 {
			msg += fmt.Sprintf(" (%d spin in flight)", pending)
		}
		return msg
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "🎰 *Tracked PRs* — `%s`\n```\n", repo)
	fmt.Fprintf(&sb, "%-6s %-5s %-16s %-8s %s\n", "PR", "spins", "last-engine", "findings", "last-run")
	for _, p := range prs {
		findings := "?"
		if p.LastFindings != nil {
			findings = strconv.Itoa(*p.LastFindings)
		}
		engine := p.LastEngine
		if p.LastKind == "addon" {
			engine += "(bonus)"
		}
		flag := ""
		if p.LastError != "" {
			flag = " ⚠️"
		}
		fmt.Fprintf(&sb, "#%-5d %-5d %-16s %-8s %s%s\n",
			p.PR, p.Runs, engine, findings, p.LastAt.UTC().Format("01-02 15:04"), flag)
	}
	sb.WriteString("```")
	if pending > 0 {
		fmt.Fprintf(&sb, "\n_%d spin(s) in flight (review not posted yet)_", pending)
	}
	return sb.String()
}

func renderPayouts(ps []ledger.Payout) string {
	if len(ps) == 0 {
		return "_(empty pool — nothing to pay)_"
	}
	var sb strings.Builder
	for _, p := range ps {
		fmt.Fprintf(&sb, "• %s → %s (%s)\n", renderPayee(p.Payee), p.Amount, p.Reason)
	}
	return sb.String()
}

func renderPayee(p string) string {
	if id, ok := strings.CutPrefix(p, "slack:"); ok {
		return "<@" + id + ">"
	}
	return p
}
