package slackbot

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/slack-go/slack"

	"casino-review/internal/ledger"
)

const tailCursorKey = "slackbot.events.cursor"

// tail posts channel notifications for market events that did NOT originate
// from a Slack command (those already got an in-channel reply). This is how
// CLI actions today — and oracle resolutions in P3 — reach the channel without
// any extra wiring: everything money-shaped already lands on the events spine.
func (b *Bot) tail(ctx context.Context) {
	// Establish the cursor before consuming anything. A transient failure here
	// must NOT silently fall back to 0 — that would replay the entire events
	// history into the channel. Retry until we know where to start.
	var after int64
	for {
		cursor, ok, err := b.st.GetKV(ctx, tailCursorKey)
		if err == nil && ok && cursor != "" {
			after, _ = strconv.ParseInt(cursor, 10, 64)
			break
		}
		if err == nil {
			// First run: start at the tail — don't replay history.
			if after, err = b.st.MaxEventID(ctx); err == nil {
				break
			}
		}
		log.Printf("slackbot tail: init cursor: %v (retrying)", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}

	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			events, err := b.st.EventsAfter(ctx, after, []string{"market.", "position."}, 50)
			if err != nil {
				log.Printf("slackbot tail: %v", err)
				continue
			}
			for _, e := range events {
				after = e.ID
				if opts, ok := b.eventMessage(ctx, e.Type, e.Actor, e.ContextRef, e.Payload); ok {
					if _, _, err := b.api.PostMessage(b.channelID, opts...); err != nil {
						log.Printf("slackbot tail: post: %v", err)
					}
				}
			}
			if len(events) > 0 {
				if err := b.st.SetKV(ctx, tailCursorKey, strconv.FormatInt(after, 10)); err != nil {
					log.Printf("slackbot tail: cursor: %v", err)
				}
			}
		}
	}
}

// eventMessage turns a spine event into the message options to post, or ok=false
// to skip (slack-originated events were already answered in-channel). New markets
// arrive as a tappable card; resolutions/locks/voids get a 📊 View button;
// everything else is plain text.
func (b *Bot) eventMessage(ctx context.Context, evType, actor, ctxRef string, payload json.RawMessage) ([]slack.MsgOption, bool) {
	var p struct {
		Via      string `json:"via"`
		MarketID int64  `json:"market_id"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, false
	}
	if p.Via == "slack" {
		return nil, false
	}

	switch evType {
	case "market.created":
		// Post the live card so people can bet straight from the notification.
		if d, err := b.svc.Detail(ctx, p.MarketID, ""); err == nil && d.Market.State == ledger.StateOpen {
			lead := slack.NewSectionBlock(mrkdwn(fmt.Sprintf("🆕 New market on `%s` — tap to get in:", ctxRef)), nil, nil)
			return []slack.MsgOption{slack.MsgOptionBlocks(append([]slack.Block{lead}, marketCard(d)...)...)}, true
		}
	case "market.resolved", "market.locked", "market.voided":
		text := formatEvent(evType, actor, ctxRef, payload)
		if text == "" {
			return nil, false
		}
		id := strconv.FormatInt(p.MarketID, 10)
		blocks := []slack.Block{
			slack.NewSectionBlock(mrkdwn(text), nil, nil),
			slack.NewActionBlock("notif_"+id, slack.NewButtonBlockElement(actDetails, id, plainT("📊 View"))),
		}
		return []slack.MsgOption{slack.MsgOptionBlocks(blocks...)}, true
	}

	if text := formatEvent(evType, actor, ctxRef, payload); text != "" {
		return []slack.MsgOption{slack.MsgOptionText(text, false)}, true
	}
	return nil, false
}

// formatEvent renders a notification's text, or "" to skip (slack-originated
// events were already answered in-channel by the command handler).
func formatEvent(evType, actor, ctxRef string, payload json.RawMessage) string {
	var p struct {
		Via        string           `json:"via"`
		MarketID   int64            `json:"market_id"`
		Kind       string           `json:"kind"`
		Question   string           `json:"question"`
		Outcome    string           `json:"outcome"`
		AmountUSDC int64            `json:"amount_usdc"`
		Reason     string           `json:"reason"`
		Payouts    []map[string]any `json:"payouts"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	if p.Via == "slack" {
		return ""
	}
	who := renderPayee(actor)
	switch evType {
	case "market.created":
		return fmt.Sprintf("🆕 Market #%d on %s — _%s_ (by %s)", p.MarketID, ctxRef, p.Question, who)
	case "position.placed":
		return fmt.Sprintf("🎲 %s put %s on *%s* (market #%d, %s)", who, ledger.USDC(p.AmountUSDC), p.Outcome, p.MarketID, ctxRef)
	case "position.refunded":
		return fmt.Sprintf("↩️ %s withdrew %s from market #%d", who, ledger.USDC(p.AmountUSDC), p.MarketID)
	case "market.locked":
		return fmt.Sprintf("🔒 Market #%d locked — no more bets (%s)", p.MarketID, ctxRef)
	case "market.resolved":
		msg := fmt.Sprintf("🏁 Market #%d resolved: *%s* (%s)", p.MarketID, p.Outcome, ctxRef)
		for _, po := range p.Payouts {
			amt, _ := po["amount_usdc"].(float64)
			payee, _ := po["payee"].(string)
			reason, _ := po["reason"].(string)
			msg += fmt.Sprintf("\n• %s → %s (%s)", renderPayee(payee), ledger.USDC(int64(amt)), reason)
		}
		return msg
	case "market.voided":
		return fmt.Sprintf("🚫 Market #%d voided (%s) — stakes refunded", p.MarketID, ctxRef)
	}
	return ""
}
