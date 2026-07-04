package slackbot

import (
	"context"
	"fmt"
	"log"
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
		return fmt.Sprintf("💰 <@%s> staked %s on market #%d (%s) — pool now %s",
			sc.UserID, amt, m.ID, m.ContextRef, pool), false

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
		return fmt.Sprintf("🆕 Market #%d — %s\nOutcomes: `%s` · bet with `/casino bet %d <outcome> <amount>`",
			m.ID, m.Question, strings.Join(m.Outcomes, "` `"), m.ID), false

	case "bet":
		amt, err := ledger.ParseUSDC(cmd.Amount)
		if err != nil {
			return "⚠️ " + err.Error(), true
		}
		if err := b.svc.Bet(ctx, cmd.MarketID, participant, cmd.Outcome, amt); err != nil {
			return "⚠️ " + err.Error(), true
		}
		_, pool, _ := b.svc.Get(ctx, cmd.MarketID)
		return fmt.Sprintf("🎲 <@%s> put %s on *%s* (market #%d) — pool now %s",
			sc.UserID, amt, cmd.Outcome, cmd.MarketID, pool), false

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

func renderBoard(rows []ledger.BoardRow) string {
	if len(rows) == 0 {
		return "🎰 The board is empty — open the bidding with `/casino fund #<pr> <amount>`."
	}
	var sb strings.Builder
	sb.WriteString("🎰 *The Board* — live markets by pool\n")
	for i, r := range rows {
		state := ""
		if r.Market.State == ledger.StateLocked {
			state = " 🔒"
		}
		fmt.Fprintf(&sb, "%d. *#%d* %s — *%s* (%d backer(s))%s\n   _%s_ [%s]\n",
			i+1, r.Market.ID, r.Market.ContextRef, r.Pool, r.Participants, state,
			r.Market.Question, r.Market.Kind)
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
