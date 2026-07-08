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
			case socketmode.EventTypeInteractive:
				cb, ok := evt.Data.(slack.InteractionCallback)
				if !ok {
					continue
				}
				switch cb.Type {
				case slack.InteractionTypeBlockActions:
					// Button clicks: ack now, act async (opening a modal still
					// beats the 3s window off the ack'd trigger_id).
					b.sock.Ack(*evt.Request)
					go b.handleBlockAction(ctx, cb)
				case slack.InteractionTypeViewSubmission:
					// Modal submit: the ack payload carries validation errors or
					// closes the dialog, so it must run before the ack.
					b.handleViewSubmission(ctx, cb, evt.Request)
				}
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

	r := b.execute(ctx, sc, participant)
	if r.text == "" && len(r.blocks) == 0 {
		return
	}
	b.send(sc, r)
}

// reply is a command result: either text or Block Kit blocks, posted in-channel
// (public market activity) or ephemerally (help, errors, personal views).
type reply struct {
	text      string
	blocks    []slack.Block
	ephemeral bool
}

func (b *Bot) send(sc slack.SlashCommand, r reply) {
	opt := slack.MsgOptionText(r.text, false)
	if len(r.blocks) > 0 {
		opt = slack.MsgOptionBlocks(r.blocks...)
	}
	if r.ephemeral {
		if _, err := b.api.PostEphemeral(sc.ChannelID, sc.UserID, opt); err != nil {
			log.Printf("slackbot: ephemeral: %v", err)
		}
		return
	}
	if _, _, err := b.api.PostMessage(b.channelID, opt); err != nil {
		log.Printf("slackbot: post: %v", err)
	}
}

// execute runs a parsed command and returns a reply. Money verbs address their
// market context-first (`bet #123 merge-by …`), resolved to an id via MarketFor;
// a bare market number still works as a fallback.
func (b *Bot) execute(ctx context.Context, sc slack.SlashCommand, participant string) reply {
	cmd, err := Parse(sc.Text)
	if err != nil {
		return reply{text: "⚠️ " + err.Error(), ephemeral: true}
	}
	emsg := func(s string) reply { return reply{text: s, ephemeral: true} }
	errf := func(err error) reply { return emsg("⚠️ " + err.Error()) }
	pub := func(s string) reply { return reply{text: s} }

	// resolveID turns a context+kind command into a market id, or uses the
	// explicit id fallback. Every money verb goes through this.
	resolveID := func() (int64, error) {
		if cmd.Context != "" {
			m, err := b.svc.MarketFor(ctx, cmd.Context, cmd.Kind)
			if err != nil {
				return 0, err
			}
			return m.ID, nil
		}
		return cmd.MarketID, nil
	}

	switch cmd.Name {
	case "help":
		return emsg(helpText)

	case "board":
		ds, err := b.svc.BoardDetails(ctx, 15)
		if err != nil {
			return errf(err)
		}
		return reply{blocks: boardBlocks(ds)}

	case "show":
		if cmd.Context != "" {
			ref, ds, err := b.svc.PRMarkets(ctx, cmd.Context, participant)
			if err != nil {
				return errf(err)
			}
			return reply{blocks: prDashboardBlocks(ref, ds), ephemeral: true}
		}
		d, err := b.svc.Detail(ctx, cmd.MarketID, participant)
		if err != nil {
			return errf(err)
		}
		return reply{blocks: marketDetailBlocks(d), ephemeral: true}

	case "me":
		positions, err := b.svc.MyPositions(ctx, participant)
		if err != nil {
			return errf(err)
		}
		login, _ := b.st.GithubLogin(ctx, sc.UserID)
		return emsg(renderMyPositions(positions, login))

	case "prs":
		prs, err := b.st.TrackedPRs(ctx, b.cfg.RepoSlug(), 15)
		if err != nil {
			return errf(err)
		}
		pending, _ := b.st.PendingSpins(ctx)
		return emsg(renderPRs(b.cfg.RepoSlug(), prs, pending))

	case "fund":
		amt, err := ledger.ParseUSDC(cmd.Amount)
		if err != nil {
			return errf(err)
		}
		m, err := b.svc.Fund(ctx, cmd.Context, participant, amt)
		if err != nil {
			return errf(err)
		}
		return b.cardReply(ctx, m.ID, participant,
			fmt.Sprintf("💰 <@%s> funded %s into the bounty on `%s`", sc.UserID, amt, m.ContextRef))

	case "market":
		spec := map[string]any{}
		if cmd.Kind == "merge-by" {
			deadline, err := ParseDeadline(cmd.Rest, time.Now())
			if err != nil {
				return errf(err)
			}
			spec["deadline"] = deadline
		}
		m, err := b.svc.Create(ctx, cmd.Kind, cmd.Context, participant, spec)
		if err != nil {
			return errf(err)
		}
		return b.cardReply(ctx, m.ID, participant,
			fmt.Sprintf("🆕 <@%s> opened a market — tap 🎲 *Bet* to get in:", sc.UserID))

	case "bet":
		amt, err := ledger.ParseUSDC(cmd.Amount)
		if err != nil {
			return errf(err)
		}
		id, err := resolveID()
		if err != nil {
			return errf(err)
		}
		if err := b.svc.Bet(ctx, id, participant, cmd.Outcome, amt); err != nil {
			return errf(err)
		}
		return b.cardReply(ctx, id, participant,
			fmt.Sprintf("🎲 <@%s> put %s on *%s*", sc.UserID, amt, cmd.Outcome))

	case "refund":
		id, err := resolveID()
		if err != nil {
			return errf(err)
		}
		m, _, _ := b.svc.Get(ctx, id)
		amt, err := b.svc.Refund(ctx, id, participant)
		if err != nil {
			return errf(err)
		}
		return pub(fmt.Sprintf("↩️ <@%s> withdrew %s from %s market on `%s`", sc.UserID, amt, m.Kind, m.ContextRef))

	case "link":
		if err := b.st.LinkIdentity(ctx, sc.UserID, cmd.Rest); err != nil {
			return errf(err)
		}
		// Public on purpose: identity claims route payouts, so the channel
		// should see them happen.
		return pub(fmt.Sprintf("🔗 Linked <@%s> ↔ github:%s", sc.UserID, cmd.Rest))

	case "lock", "resolve", "void":
		// MONEY AUTHORIZATION: settling verbs move other people's stakes, so
		// they are restricted to the configured admin allowlist. With no
		// admins configured the verbs are disabled in Slack entirely (the CLI
		// on the host remains the admin path). Without this gate, any channel
		// member could resolve a market to an outcome that pays themselves.
		if !b.isAdmin(sc.UserID) {
			return emsg("⛔ Only casino admins can lock/resolve/void markets (set SLACK_ADMINS).")
		}
		id, err := resolveID()
		if err != nil {
			return errf(err)
		}
		m, _, _ := b.svc.Get(ctx, id)
		switch cmd.Name {
		case "lock":
			if err := b.svc.Lock(ctx, id, participant); err != nil {
				return errf(err)
			}
			return pub(fmt.Sprintf("🔒 %s market on `%s` locked — no more bets.", m.Kind, m.ContextRef))

		case "resolve":
			solver := strings.TrimPrefix(cmd.Args["solver"], "@")
			if solver != "" {
				solver = "github:" + strings.TrimPrefix(solver, "github:")
			}
			payouts, err := b.svc.Resolve(ctx, id, cmd.Outcome, solver, participant,
				map[string]any{"resolved_via": "slack-admin"})
			if err != nil {
				return errf(err)
			}
			return pub(fmt.Sprintf("🏁 %s market on `%s` resolved: *%s*\n%s", m.Kind, m.ContextRef, cmd.Outcome, renderPayouts(payouts)))

		default: // void
			refunds, err := b.svc.Void(ctx, id, participant, cmd.Rest)
			if err != nil {
				return errf(err)
			}
			return pub(fmt.Sprintf("🚫 %s market on `%s` voided — %d stake(s) refunded.", m.Kind, m.ContextRef, len(refunds)))
		}
	}
	return emsg("⚠️ unhandled command")
}

// handleBlockAction routes a board/detail button click. Button values carry the
// market id, so a click is unambiguous without the user typing anything.
func (b *Bot) handleBlockAction(ctx context.Context, cb slack.InteractionCallback) {
	if len(cb.ActionCallback.BlockActions) == 0 {
		return
	}
	if cb.Channel.ID != "" && cb.Channel.ID != b.channelID {
		return
	}
	ba := cb.ActionCallback.BlockActions[0]
	participant := "slack:" + cb.User.ID
	ctx = ledger.WithVia(ctx, "slack")
	id, err := strconv.ParseInt(ba.Value, 10, 64)
	if err != nil {
		return
	}
	switch ba.ActionID {
	case actBet:
		m, _, err := b.svc.Get(ctx, id)
		if err != nil {
			b.ephemUser(cb.User.ID, "⚠️ "+err.Error())
			return
		}
		if m.State != ledger.StateOpen {
			b.ephemUser(cb.User.ID, fmt.Sprintf("🔒 That market is %s — no more bets.", strings.ToLower(m.State)))
			return
		}
		if _, err := b.api.OpenView(cb.TriggerID, betModal(m)); err != nil {
			log.Printf("slackbot: open view: %v", err)
			b.ephemUser(cb.User.ID, "⚠️ couldn't open the bet dialog — try `/casino bet` instead.")
		}
	case actDetails:
		d, err := b.svc.Detail(ctx, id, participant)
		if err != nil {
			b.ephemUser(cb.User.ID, "⚠️ "+err.Error())
			return
		}
		if _, err := b.api.PostEphemeral(b.channelID, cb.User.ID, slack.MsgOptionBlocks(marketDetailBlocks(d)...)); err != nil {
			log.Printf("slackbot: ephemeral: %v", err)
		}
	case actRefund:
		m, _, _ := b.svc.Get(ctx, id)
		amt, err := b.svc.Refund(ctx, id, participant)
		if err != nil {
			b.ephemUser(cb.User.ID, "⚠️ "+err.Error())
			return
		}
		b.tel.Track(participant, "slack_button", map[string]any{"action": "refund"})
		b.postChannel(fmt.Sprintf("↩️ <@%s> withdrew %s from %s market on `%s`", cb.User.ID, amt, m.Kind, m.ContextRef))
	}
}

// handleViewSubmission places a bet from the modal. Validation problems are
// returned as inline modal errors (response_action: errors) so the dialog stays
// open; success acks (closes the modal) and posts the bet to the channel.
func (b *Bot) handleViewSubmission(ctx context.Context, cb slack.InteractionCallback, req *socketmode.Request) {
	if cb.View.CallbackID != cbBetModal {
		b.sock.Ack(*req)
		return
	}
	participant := "slack:" + cb.User.ID
	ctx = ledger.WithVia(ctx, "slack")
	id, err := strconv.ParseInt(cb.View.PrivateMetadata, 10, 64)
	if err != nil {
		b.sock.Ack(*req)
		return
	}
	vals := cb.View.State.Values
	amt, err := ledger.ParseUSDC(vals[blkAmount][actAmount].Value)
	if err != nil {
		b.ackViewErr(req, blkAmount, err.Error())
		return
	}
	m, _, err := b.svc.Get(ctx, id)
	if err != nil {
		b.ackViewErr(req, blkAmount, err.Error())
		return
	}
	outcome := "merged"
	if m.Kind != "bounty" {
		outcome = vals[blkOutcome][actOutcome].SelectedOption.Value
		if outcome == "" {
			b.ackViewErr(req, blkOutcome, "pick an outcome")
			return
		}
	}
	if err := b.svc.Bet(ctx, id, participant, outcome, amt); err != nil {
		b.ackViewErr(req, blkAmount, err.Error())
		return
	}
	b.sock.Ack(*req) // close the modal
	b.tel.Track(participant, "slack_button", map[string]any{"action": "bet"})
	_, pool, _ := b.svc.Get(ctx, id)
	b.postChannel(fmt.Sprintf("🎲 <@%s> put %s on *%s* (%s %s on `%s`) · pool now *%s*",
		cb.User.ID, amt, outcome, kindEmoji(m.Kind), m.Kind, m.ContextRef, pool))
}

func (b *Bot) ackViewErr(req *socketmode.Request, block, msg string) {
	if err := b.sock.Ack(*req, map[string]any{
		"response_action": "errors",
		"errors":          map[string]string{block: msg},
	}); err != nil {
		log.Printf("slackbot: ack view: %v", err)
	}
}

// cardReply builds an in-channel reply: a one-line lead + the market's card
// (odds + Bet/Details buttons), so a fresh/updated market is immediately
// tappable right where the action happened — not one command away.
func (b *Bot) cardReply(ctx context.Context, marketID int64, participant, lead string) reply {
	d, err := b.svc.Detail(ctx, marketID, participant)
	if err != nil {
		return reply{text: lead} // still confirm, just without the card
	}
	blocks := append([]slack.Block{slack.NewSectionBlock(mrkdwn(lead), nil, nil)}, marketCard(d)...)
	return reply{blocks: blocks}
}

func (b *Bot) postChannel(text string) {
	if _, _, err := b.api.PostMessage(b.channelID, slack.MsgOptionText(text, false)); err != nil {
		log.Printf("slackbot: post: %v", err)
	}
}

func (b *Bot) ephemUser(userID, text string) {
	if _, err := b.api.PostEphemeral(b.channelID, userID, slack.MsgOptionText(text, false)); err != nil {
		log.Printf("slackbot: ephemeral: %v", err)
	}
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
		fmt.Fprintf(&sb, "\n• %s *%s* on `%s` · %s%s%s",
			kindEmoji(p.Kind), p.Kind, p.ContextRef, outcome, p.Amount, lock)
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
