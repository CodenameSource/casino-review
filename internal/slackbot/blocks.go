package slackbot

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/slack-go/slack"

	"casino-review/internal/ledger"
	"casino-review/internal/market"
)

// Interaction identifiers. Button values always carry the internal market id so
// a click is unambiguous without the user ever seeing or typing a number.
const (
	actBet     = "casino_bet"     // button → open the bet modal
	actDetails = "casino_details" // button → ephemeral market detail
	actRefund  = "casino_refund"  // button → refund the clicker's stake

	cbBetModal = "casino_bet_modal" // modal callback_id
	blkOutcome = "casino_outcome"   // modal input block/action for the outcome
	actOutcome = "casino_outcome_a"
	blkAmount  = "casino_amount" // modal input block/action for the amount
	actAmount  = "casino_amount_a"
)

func mrkdwn(s string) *slack.TextBlockObject {
	return slack.NewTextBlockObject(slack.MarkdownType, s, false, false)
}
func plainT(s string) *slack.TextBlockObject {
	return slack.NewTextBlockObject(slack.PlainTextType, s, true, false)
}

// oddsLine renders the money spread for a card: a pool total for a bounty, the
// per-outcome odds otherwise.
func oddsLine(d market.Detail) string {
	m := d.Market
	if m.Kind == "bounty" {
		return fmt.Sprintf("Pool *%s* · %d backer(s)", d.Pool, d.Backers)
	}
	line := market.Line(m.Outcomes, d.OutcomePools)
	if d.Pool == 0 {
		return "_no bets yet_ · " + line
	}
	return fmt.Sprintf("%s · pool *%s*", line, d.Pool)
}

// marketCard is one market's summary + action buttons, used on the board and the
// PR dashboard. Bet is offered only while the market is OPEN.
func marketCard(d market.Detail) []slack.Block {
	m := d.Market
	lock := ""
	if m.State == ledger.StateLocked {
		lock = "  🔒 _locked_"
	} else if m.State == ledger.StateResolved {
		lock = "  🏁 _resolved_"
	}
	head := fmt.Sprintf("*%s %s* on `%s`%s\n_%s_\n%s",
		kindEmoji(m.Kind), m.Kind, m.ContextRef, lock, m.Question, oddsLine(d))
	blocks := []slack.Block{slack.NewSectionBlock(mrkdwn(head), nil, nil)}

	if m.State == ledger.StateOpen {
		id := strconv.FormatInt(m.ID, 10)
		betLabel := "🎲 Bet"
		if m.Kind == "bounty" {
			betLabel = "💰 Fund"
		}
		blocks = append(blocks, slack.NewActionBlock(actBet+"_"+id,
			slack.NewButtonBlockElement(actBet, id, plainT(betLabel)).WithStyle(slack.StylePrimary),
			slack.NewButtonBlockElement(actDetails, id, plainT("📊 Details")),
		))
	}
	return blocks
}

// boardBlocks renders the whole board (ranked open markets) as cards.
func boardBlocks(ds []market.Detail) []slack.Block {
	if len(ds) == 0 {
		return []slack.Block{slack.NewSectionBlock(mrkdwn(
			"🎰 *No markets open yet — be the first.*\n"+
				"• `/casino fund #<pr> 25` — 💰 bounty the author on merge\n"+
				"• `/casino open #<pr> merge-by 72h` — 📅 bet on the merge deadline"), nil, nil)}
	}
	blocks := []slack.Block{slack.NewSectionBlock(mrkdwn("🎰 *The Board* — where the money's at"), nil, nil)}
	for _, d := range ds {
		blocks = append(blocks, slack.NewDividerBlock())
		blocks = append(blocks, marketCard(d)...)
	}
	return blocks
}

// prDashboardBlocks renders every market on one PR — the `/casino show #123` view.
func prDashboardBlocks(contextRef string, ds []market.Detail) []slack.Block {
	if len(ds) == 0 {
		return []slack.Block{slack.NewSectionBlock(mrkdwn(fmt.Sprintf(
			"🎰 *No markets on* `%s` *yet.*\n"+
				"• `/casino fund %s 25` — 💰 bounty the author on merge\n"+
				"• `/casino open %s merge-by 72h` — 📅 will it merge in time?\n"+
				"• `/casino open %s findings-count` — 🔎 how many findings?",
			contextRef, contextRef, contextRef, contextRef)), nil, nil)}
	}
	blocks := []slack.Block{slack.NewSectionBlock(mrkdwn(fmt.Sprintf("🎰 *Markets on* `%s`", contextRef)), nil, nil)}
	for _, d := range ds {
		blocks = append(blocks, slack.NewDividerBlock())
		blocks = append(blocks, marketCard(d)...)
	}
	return blocks
}

// marketDetailBlocks is the single-market deep view (odds table, your stake,
// actions) shown by `/casino show <id>` and the Details button.
func marketDetailBlocks(d market.Detail) []slack.Block {
	m := d.Market
	head := fmt.Sprintf("*%s %s* on `%s` · [%s]\n_%s_\nPool *%s* · %d backer(s)",
		kindEmoji(m.Kind), m.Kind, m.ContextRef, m.State, m.Question, d.Pool, d.Backers)
	blocks := []slack.Block{slack.NewSectionBlock(mrkdwn(head), nil, nil)}

	if m.Kind == "bounty" {
		if mine := d.MyStake["merged"]; mine > 0 {
			blocks = append(blocks, slack.NewSectionBlock(mrkdwn(fmt.Sprintf("Your stake: *%s*", mine)), nil, nil))
		}
	} else {
		var sb strings.Builder
		for _, o := range market.Odds(m.Outcomes, d.OutcomePools) {
			payout := "—"
			if o.PayoutX > 0 {
				payout = fmt.Sprintf("~%.2f×", o.PayoutX)
			}
			mine := ""
			if v := d.MyStake[o.Outcome]; v > 0 {
				mine = fmt.Sprintf("  ← you: *%s*", v)
			}
			fmt.Fprintf(&sb, "`%-10s` %s  (%d%%) · win pays %s%s\n", o.Outcome, o.Pool, int(o.Prob*100+0.5), payout, mine)
		}
		blocks = append(blocks, slack.NewSectionBlock(mrkdwn(sb.String()), nil, nil))
	}

	// Actions: Bet while open; Refund only if the caller actually has a stake.
	if m.State == ledger.StateOpen {
		id := strconv.FormatInt(m.ID, 10)
		betLabel := "🎲 Bet"
		if m.Kind == "bounty" {
			betLabel = "💰 Fund"
		}
		els := []slack.BlockElement{slack.NewButtonBlockElement(actBet, id, plainT(betLabel)).WithStyle(slack.StylePrimary)}
		if hasStake(d.MyStake) {
			els = append(els, slack.NewButtonBlockElement(actRefund, id, plainT("↩️ Refund")).WithStyle(slack.StyleDanger).
				WithConfirm(slack.NewConfirmationBlockObject(
					plainT("Withdraw your stake?"), plainT("This pulls your whole stake out of this market."),
					plainT("Refund"), plainT("Keep it"))))
		}
		blocks = append(blocks, slack.NewActionBlock("detail_"+id, els...))
	}
	return blocks
}

func hasStake(m map[string]ledger.USDC) bool {
	for _, v := range m {
		if v > 0 {
			return true
		}
	}
	return false
}

// betModal builds the Place-a-bet modal for a market. The market id rides in
// PrivateMetadata; a bounty skips outcome selection (single implicit outcome).
func betModal(m ledger.Market) slack.ModalViewRequest {
	blocks := []slack.Block{slack.NewSectionBlock(mrkdwn(fmt.Sprintf(
		"*%s %s* on `%s`\n_%s_", kindEmoji(m.Kind), m.Kind, m.ContextRef, m.Question)), nil, nil)}

	if m.Kind == "bounty" {
		blocks = append(blocks, slack.NewSectionBlock(mrkdwn(
			"You're adding to the *bounty* pool — it pays the PR author when it merges."), nil, nil))
	} else {
		opts := make([]*slack.OptionBlockObject, 0, len(m.Outcomes))
		for _, o := range m.Outcomes {
			opts = append(opts, slack.NewOptionBlockObject(o, plainT(o), nil))
		}
		radio := slack.NewRadioButtonsBlockElement(actOutcome, opts...)
		blocks = append(blocks, slack.NewInputBlock(blkOutcome, plainT("Outcome"), nil, radio))
	}

	amt := slack.NewPlainTextInputBlockElement(plainT("e.g. 10 or $10.50"), actAmount)
	blocks = append(blocks, slack.NewInputBlock(blkAmount, plainT("Amount (USDC)"), nil, amt))

	return slack.ModalViewRequest{
		Type:            slack.VTModal,
		CallbackID:      cbBetModal,
		PrivateMetadata: strconv.FormatInt(m.ID, 10),
		Title:           plainT("Place a bet"),
		Submit:          plainT("Place bet"),
		Close:           plainT("Cancel"),
		Blocks:          slack.Blocks{BlockSet: blocks},
	}
}
