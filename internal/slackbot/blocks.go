package slackbot

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/slack-go/slack"

	"casino-review/internal/ledger"
	"casino-review/internal/market"
)

// Interaction identifiers. Button values carry the internal market id (and, for
// the fast bet path, the chosen outcome) so a click is unambiguous without the
// user ever seeing or typing a number.
const (
	actBet     = "casino_bet"      // open the bet modal (bounty Fund / >4-outcome markets)
	actBetPick = "casino_bet_pick" // per-outcome card button (id+outcome in value); action_id gets a _<i> suffix
	actDetails = "casino_details"  // ephemeral market detail
	actRefund  = "casino_refund"   // refund the clicker's stake

	actAmtPreset = "casino_preset"     // preset amount button inside the amount modal; _<amt> suffix, value=amount
	actAmtCustom = "casino_amt_custom" // switch the amount modal to a free-text input
	actBetAgain  = "casino_bet_again"  // confirmation modal → bet again

	actNewMarket = "casino_new_market" // open the create-a-market modal (context-free)
	actBrowse    = "casino_browse"     // post the board ephemerally
	actLink      = "casino_link"       // open the link-GitHub modal
	actRefresh   = "casino_refresh"    // refresh a board/detail message in place (value: "board:0"/"detail:<id>")

	cbBetModal  = "casino_bet_modal"        // amount text submit (classic + custom)
	cbNewMarket = "casino_new_market_modal" // create-market submit
	cbLinkModal = "casino_link_modal"       // link-GitHub submit
	scGlobal    = "casino_open"             // global shortcut callback_id

	blkOutcome     = "casino_outcome"
	actOutcome     = "casino_outcome_a"
	blkAmount      = "casino_amount"
	actAmount      = "casino_amount_a"
	blkNewCtx      = "casino_new_ctx"
	actNewCtx      = "casino_new_ctx_a"
	blkNewKind     = "casino_new_kind"
	actNewKind     = "casino_new_kind_a"
	blkNewDeadline = "casino_new_deadline"
	actNewDeadline = "casino_new_deadline_a"
	blkLinkLogin   = "casino_link_login"
	actLinkLogin   = "casino_link_login_a"
)

// presetAmounts are the one-tap stake buttons (USDC). Each is fed through
// ledger.ParseUSDC like any other amount — never trusted as a raw number.
var presetAmounts = []string{"5", "10", "25", "50"}

func mrkdwn(s string) *slack.TextBlockObject {
	return slack.NewTextBlockObject(slack.MarkdownType, s, false, false)
}
func plainT(s string) *slack.TextBlockObject {
	return slack.NewTextBlockObject(slack.PlainTextType, s, true, false)
}

// encMV/decMV pack a market id + outcome into a button value or a modal's
// PrivateMetadata. A bare id (no '|') decodes to an empty outcome.
func encMV(id int64, outcome string) string {
	return strconv.FormatInt(id, 10) + "|" + outcome
}
func decMV(s string) (int64, string) {
	idStr, outcome, _ := strings.Cut(s, "|")
	id, _ := strconv.ParseInt(idStr, 10, 64)
	return id, outcome
}

// encVal/decVal pack a "kind:id" pair for the refresh button.
func encVal(kind string, id int64) string { return kind + ":" + strconv.FormatInt(id, 10) }
func decVal(s string) (string, int64) {
	kind, idStr, _ := strings.Cut(s, ":")
	id, _ := strconv.ParseInt(idStr, 10, 64)
	return kind, id
}

func refundConfirm() *slack.ConfirmationBlockObject {
	return slack.NewConfirmationBlockObject(
		plainT("Withdraw your stake?"), plainT("This pulls your whole stake out of this market."),
		plainT("Refund"), plainT("Keep it"))
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

// outcomeBtnLabel labels a per-outcome bet button: ✅/❌ for a binary market,
// the bucket text otherwise.
func outcomeBtnLabel(outcomes []string, i int) string {
	if len(outcomes) == 2 {
		if i == 0 {
			return "✅ " + outcomes[0]
		}
		return "❌ " + outcomes[1]
	}
	return outcomes[i]
}

// betButtons is the bet-entry element set shared by the card and the detail
// view: Fund for a bounty, one button per outcome for a small parimutuel market
// (the 2-tap fast path), or a single Bet that opens the picker for a market with
// more outcomes than fit on a row.
func betButtons(m ledger.Market) []slack.BlockElement {
	id := strconv.FormatInt(m.ID, 10)
	switch {
	case m.Kind == "bounty":
		return []slack.BlockElement{slack.NewButtonBlockElement(actBet, id, plainT("💰 Fund")).WithStyle(slack.StylePrimary)}
	case len(m.Outcomes) <= 4:
		els := make([]slack.BlockElement, 0, len(m.Outcomes))
		for i, o := range m.Outcomes {
			btn := slack.NewButtonBlockElement(fmt.Sprintf("%s_%d", actBetPick, i), encMV(m.ID, o), plainT(outcomeBtnLabel(m.Outcomes, i)))
			if i == 0 {
				btn = btn.WithStyle(slack.StylePrimary)
			}
			els = append(els, btn)
		}
		return els
	default:
		return []slack.BlockElement{slack.NewButtonBlockElement(actBet, id, plainT("🎲 Bet")).WithStyle(slack.StylePrimary)}
	}
}

// marketCard is one market's summary + bet/details buttons, used on the board,
// the PR dashboard, the home tab, and new-market confirmations. Bet is offered
// only while OPEN.
func marketCard(d market.Detail) []slack.Block {
	m := d.Market
	lock := ""
	switch m.State {
	case ledger.StateLocked:
		lock = "  🔒 _locked_"
	case ledger.StateResolved:
		lock = "  🏁 _resolved_"
	}
	head := fmt.Sprintf("*%s %s* on `%s`%s\n_%s_\n%s",
		kindEmoji(m.Kind), m.Kind, m.ContextRef, lock, m.Question, oddsLine(d))
	blocks := []slack.Block{slack.NewSectionBlock(mrkdwn(head), nil, nil)}
	if m.State != ledger.StateOpen {
		return blocks
	}
	id := strconv.FormatInt(m.ID, 10)
	els := append(betButtons(m), slack.NewButtonBlockElement(actDetails, id, plainT("📊 Details")))
	blocks = append(blocks, slack.NewActionBlock("card_"+id, els...))
	return blocks
}

// boardBlocks renders the whole board (ranked open markets) as cards, with a
// New-market / Refresh action bar on top.
func boardBlocks(ds []market.Detail) []slack.Block {
	blocks := []slack.Block{
		slack.NewSectionBlock(mrkdwn("🎰 *The Board* — where the money's at"), nil, nil),
		slack.NewActionBlock("board_bar",
			slack.NewButtonBlockElement(actNewMarket, "board", plainT("＋ New market")).WithStyle(slack.StylePrimary),
			slack.NewButtonBlockElement(actRefresh, encVal("board", 0), plainT("🔄 Refresh")),
		),
	}
	if len(ds) == 0 {
		blocks = append(blocks, slack.NewSectionBlock(mrkdwn(
			"No markets open yet — be the first: tap *＋ New market*, or\n"+
				"• `/casino fund #<pr> 25` — 💰 bounty the author on merge\n"+
				"• `/casino open #<pr> merge-by 72h` — 📅 bet on the merge deadline"), nil, nil))
		return blocks
	}
	for _, d := range ds {
		blocks = append(blocks, slack.NewDividerBlock())
		blocks = append(blocks, marketCard(d)...)
	}
	return blocks
}

// prDashboardBlocks renders every market on one PR — the `/casino show #123` view.
func prDashboardBlocks(contextRef string, ds []market.Detail) []slack.Block {
	if len(ds) == 0 {
		return []slack.Block{
			slack.NewSectionBlock(mrkdwn(fmt.Sprintf("🎰 *No markets on* `%s` *yet.*", contextRef)), nil, nil),
			slack.NewActionBlock("pr_bar_"+contextRef,
				slack.NewButtonBlockElement(actNewMarket, contextRef, plainT("＋ New market")).WithStyle(slack.StylePrimary)),
		}
	}
	blocks := []slack.Block{slack.NewSectionBlock(mrkdwn(fmt.Sprintf("🎰 *Markets on* `%s`", contextRef)), nil, nil)}
	for _, d := range ds {
		blocks = append(blocks, slack.NewDividerBlock())
		blocks = append(blocks, marketCard(d)...)
	}
	return blocks
}

// marketDetailBlocks is the single-market deep view (odds table, your stake,
// bet + refund actions). No Details button — you're already looking at them.
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

	if m.State == ledger.StateOpen {
		id := strconv.FormatInt(m.ID, 10)
		els := betButtons(m)
		if hasStake(d.MyStake) {
			els = append(els, slack.NewButtonBlockElement(actRefund, id, plainT("↩️ Refund")).WithStyle(slack.StyleDanger).WithConfirm(refundConfirm()))
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

// --- bet modals: card outcome button → amount (presets) → confirmation ---

// betAmountModal is the fast-path amount step: preset buttons that place the bet
// on tap, plus a Custom escape hatch. No Submit — the buttons drive it. The
// market id + chosen outcome ride in PrivateMetadata.
func betAmountModal(m ledger.Market, outcome string) slack.ModalViewRequest {
	lead := fmt.Sprintf("You're betting on *%s*", outcome)
	if m.Kind == "bounty" {
		lead = "You're adding to the *bounty* pool — it pays the PR author when it merges."
	}
	presets := make([]slack.BlockElement, 0, len(presetAmounts)+1)
	for _, a := range presetAmounts {
		presets = append(presets, slack.NewButtonBlockElement(actAmtPreset+"_"+a, a, plainT("$"+a)))
	}
	presets = append(presets, slack.NewButtonBlockElement(actAmtCustom, "custom", plainT("✏️ Custom…")))

	return slack.ModalViewRequest{
		Type:            slack.VTModal,
		CallbackID:      "casino_amount_view",
		PrivateMetadata: encMV(m.ID, outcome),
		Title:           plainT("Place a bet"),
		Close:           plainT("Cancel"),
		Blocks: slack.Blocks{BlockSet: []slack.Block{
			slack.NewSectionBlock(mrkdwn(fmt.Sprintf("*%s %s* on `%s`\n_%s_", kindEmoji(m.Kind), m.Kind, m.ContextRef, m.Question)), nil, nil),
			slack.NewSectionBlock(mrkdwn(lead), nil, nil),
			slack.NewActionBlock("amounts", presets...),
		}},
	}
}

// betCustomModal is the free-text amount step (reached via "Custom…"). Submit
// routes to the shared bet-submit handler; outcome is in PrivateMetadata.
func betCustomModal(m ledger.Market, outcome string) slack.ModalViewRequest {
	amt := slack.NewInputBlock(blkAmount, plainT("Amount (USDC)"), plainT("e.g. 12 or $7.50"),
		slack.NewPlainTextInputBlockElement(plainT("e.g. 12 or $7.50"), actAmount))
	return slack.ModalViewRequest{
		Type:            slack.VTModal,
		CallbackID:      cbBetModal,
		PrivateMetadata: encMV(m.ID, outcome),
		Title:           plainT("Custom amount"),
		Submit:          plainT("Place bet"),
		Close:           plainT("Back"),
		Blocks: slack.Blocks{BlockSet: []slack.Block{
			slack.NewSectionBlock(mrkdwn(fmt.Sprintf("Betting on *%s* · *%s %s* on `%s`", outcome, kindEmoji(m.Kind), m.Kind, m.ContextRef)), nil, nil),
			amt,
		}},
	}
}

// betModal is the classic radio-outcome + amount modal, kept for markets with
// more outcomes than fit as buttons. PrivateMetadata is the bare id; the outcome
// comes from the radio, read by handleBetSubmit when PrivateMetadata has none.
func betModal(m ledger.Market) slack.ModalViewRequest {
	blocks := []slack.Block{slack.NewSectionBlock(mrkdwn(fmt.Sprintf(
		"*%s %s* on `%s`\n_%s_", kindEmoji(m.Kind), m.Kind, m.ContextRef, m.Question)), nil, nil)}
	if m.Kind != "bounty" {
		opts := make([]*slack.OptionBlockObject, 0, len(m.Outcomes))
		for _, o := range m.Outcomes {
			opts = append(opts, slack.NewOptionBlockObject(o, plainT(o), nil))
		}
		// radio_buttons caps at 10 options; a static select handles up to 100.
		// Both submit their choice under .SelectedOption, so handleBetSubmit
		// reads either the same way.
		var el slack.BlockElement = slack.NewRadioButtonsBlockElement(actOutcome, opts...)
		if len(opts) > 10 {
			el = slack.NewOptionsSelectBlockElement(slack.OptTypeStatic, plainT("Pick an outcome"), actOutcome, opts...)
		}
		blocks = append(blocks, slack.NewInputBlock(blkOutcome, plainT("Outcome"), nil, el))
	}
	blocks = append(blocks, slack.NewInputBlock(blkAmount, plainT("Amount (USDC)"), nil,
		slack.NewPlainTextInputBlockElement(plainT("e.g. 10 or $10.50"), actAmount)))
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

// betDoneModal confirms a placed bet and offers a one-tap Bet-again.
func betDoneModal(m ledger.Market, outcome string, amt, pool ledger.USDC) slack.ModalViewRequest {
	return slack.ModalViewRequest{
		Type:            slack.VTModal,
		CallbackID:      "casino_done_view",
		PrivateMetadata: encMV(m.ID, outcome),
		Title:           plainT("Bet placed 🎲"),
		Close:           plainT("Done"),
		Blocks: slack.Blocks{BlockSet: []slack.Block{
			slack.NewSectionBlock(mrkdwn(fmt.Sprintf("✅ *%s* on *%s* · *%s %s* on `%s`\nPool now *%s*.",
				amt, outcome, kindEmoji(m.Kind), m.Kind, m.ContextRef, pool)), nil, nil),
			slack.NewActionBlock("done_bar",
				slack.NewButtonBlockElement(actBetAgain, "again", plainT("🎲 Bet again")).WithStyle(slack.StylePrimary)),
		}},
	}
}

// --- create-market + link modals ---

func newMarketModal(prefillCtx string) slack.ModalViewRequest {
	ctxInput := slack.NewPlainTextInputBlockElement(plainT("#123  or  ext:PROJ-42"), actNewCtx)
	if prefillCtx != "" && prefillCtx != "board" {
		ctxInput.InitialValue = prefillCtx
	}
	kindSelect := slack.NewOptionsSelectBlockElement(slack.OptTypeStatic, plainT("Pick a market type"), actNewKind,
		slack.NewOptionBlockObject("bounty", plainT("💰 Bounty — pays the PR author on merge"), nil),
		slack.NewOptionBlockObject("merge-by", plainT("📅 Merge-by — will it merge by a date?"), nil),
		slack.NewOptionBlockObject("findings-count", plainT("🔎 Findings-count — how many findings?"), nil),
	)
	deadline := slack.NewInputBlock(blkNewDeadline,
		plainT("Deadline (merge-by only)"), plainT("Ignored for other kinds."),
		slack.NewDatePickerBlockElement(actNewDeadline))
	deadline.Optional = true

	return slack.ModalViewRequest{
		Type:       slack.VTModal,
		CallbackID: cbNewMarket,
		Title:      plainT("New market"),
		Submit:     plainT("Open market"),
		Close:      plainT("Cancel"),
		Blocks: slack.Blocks{BlockSet: []slack.Block{
			slack.NewInputBlock(blkNewCtx, plainT("Which PR (or tracker key)?"), plainT("A GitHub PR like #123, or ext:KEY before a PR exists."), ctxInput),
			slack.NewInputBlock(blkNewKind, plainT("Market type"), nil, kindSelect),
			deadline,
		}},
	}
}

func linkModal() slack.ModalViewRequest {
	return slack.ModalViewRequest{
		Type:       slack.VTModal,
		CallbackID: cbLinkModal,
		Title:      plainT("Link GitHub"),
		Submit:     plainT("Link"),
		Close:      plainT("Cancel"),
		Blocks: slack.Blocks{BlockSet: []slack.Block{
			slack.NewSectionBlock(mrkdwn("Link your GitHub login so bounties can pay you."), nil, nil),
			slack.NewInputBlock(blkLinkLogin, plainT("GitHub username"), nil,
				slack.NewPlainTextInputBlockElement(plainT("octocat"), actLinkLogin)),
		}},
	}
}

// welcomeBlocks is the friendly `/casino help` panel: buttons first, command
// reference below.
func welcomeBlocks(linked bool) []slack.Block {
	bar := []slack.BlockElement{
		slack.NewButtonBlockElement(actBrowse, "help", plainT("📋 Board")),
		slack.NewButtonBlockElement(actNewMarket, "help", plainT("＋ New market")).WithStyle(slack.StylePrimary),
	}
	if !linked {
		bar = append(bar, slack.NewButtonBlockElement(actLink, "help", plainT("🔗 Link GitHub")))
	}
	return []slack.Block{
		slack.NewSectionBlock(mrkdwn("🎰 *Welcome to the Casino* — stake USDC on what happens to pull requests.\nTap a button, or open the app's *Home* tab for your dashboard."), nil, nil),
		slack.NewActionBlock("welcome_bar", bar...),
		slack.NewDividerBlock(),
		slack.NewSectionBlock(mrkdwn(helpText), nil, nil),
	}
}

// --- App Home ---

// homeBlocks is the per-user Home dashboard: link banner, action bar, your open
// bets (tappable), and the live board.
func homeBlocks(githubLogin string, positions []ledger.PositionView, board []market.Detail) []slack.Block {
	blocks := []slack.Block{
		slack.NewSectionBlock(mrkdwn("🎰 *Your Casino*"), nil, nil),
	}
	bar := []slack.BlockElement{
		slack.NewButtonBlockElement(actNewMarket, "home", plainT("＋ New market")).WithStyle(slack.StylePrimary),
		slack.NewButtonBlockElement(actRefresh, encVal("board", 0), plainT("🔄 Refresh")),
	}
	if githubLogin == "" {
		bar = append(bar, slack.NewButtonBlockElement(actLink, "home", plainT("🔗 Link GitHub")))
		blocks = append(blocks, slack.NewContextBlock("linkctx", mrkdwn("⚠️ GitHub not linked — bounties can't pay you yet.")))
	} else {
		blocks = append(blocks, slack.NewContextBlock("linkctx", mrkdwn("🔗 Linked to `github:"+githubLogin+"`")))
	}
	blocks = append(blocks, slack.NewActionBlock("home_bar", bar...), slack.NewDividerBlock())

	blocks = append(blocks, slack.NewSectionBlock(mrkdwn("*Your open bets*"), nil, nil))
	if len(positions) == 0 {
		blocks = append(blocks, slack.NewContextBlock("nobets", mrkdwn("No open bets yet — tap a market below to get in.")))
	} else {
		var total ledger.USDC
		for _, p := range positions {
			blocks = append(blocks, positionRow(p)...)
			total += p.Amount
		}
		blocks = append(blocks, slack.NewContextBlock("total", mrkdwn(fmt.Sprintf("Total staked: *%s*", total))))
	}

	blocks = append(blocks, slack.NewDividerBlock(), slack.NewSectionBlock(mrkdwn("*The Board*"), nil, nil))
	if len(board) == 0 {
		blocks = append(blocks, slack.NewContextBlock("noboard", mrkdwn("No markets open. Be the first — *＋ New market*.")))
	} else {
		for _, d := range board {
			blocks = append(blocks, slack.NewDividerBlock())
			blocks = append(blocks, marketCard(d)...)
		}
	}
	return blocks
}

// positionRow is one of a user's stakes with Details/Refund buttons — shared by
// the Home tab and `/casino me`.
func positionRow(p ledger.PositionView) []slack.Block {
	outcome := ""
	if p.Kind != "bounty" {
		outcome = "on *" + p.Outcome + "* "
	}
	lock := ""
	switch p.MarketState {
	case ledger.StateLocked:
		lock = " 🔒"
	case ledger.StateResolved:
		lock = " 🏁"
	}
	txt := fmt.Sprintf("%s *%s* %s· *%s* · `%s`%s", kindEmoji(p.Kind), p.Kind, outcome, p.Amount, p.ContextRef, lock)
	id := strconv.FormatInt(p.MarketID, 10)
	els := []slack.BlockElement{slack.NewButtonBlockElement(actDetails, id, plainT("📊 Details"))}
	if p.MarketState == ledger.StateOpen && p.Amount > 0 {
		els = append(els, slack.NewButtonBlockElement(actRefund, id, plainT("↩️ Refund")).WithStyle(slack.StyleDanger).WithConfirm(refundConfirm()))
	}
	return []slack.Block{
		slack.NewSectionBlock(mrkdwn(txt), nil, nil),
		slack.NewActionBlock("pos_"+id+"_"+p.Outcome, els...),
	}
}

// meBlocks is the `/casino me` view: your open bets as tappable rows.
func meBlocks(ps []ledger.PositionView, githubLogin string) []slack.Block {
	id := "not linked — tap *🔗 Link GitHub* so bounties can pay you"
	if githubLogin != "" {
		id = "github:" + githubLogin
	}
	if len(ps) == 0 {
		return []slack.Block{
			slack.NewSectionBlock(mrkdwn("🎰 *Your bets* — "+id+"\nNo open bets yet."), nil, nil),
			slack.NewActionBlock("me_bar",
				slack.NewButtonBlockElement(actBrowse, "me", plainT("📋 Board")).WithStyle(slack.StylePrimary),
				slack.NewButtonBlockElement(actNewMarket, "me", plainT("＋ New market"))),
		}
	}
	blocks := []slack.Block{slack.NewSectionBlock(mrkdwn("🎰 *Your bets* — "+id), nil, nil)}
	var total ledger.USDC
	for _, p := range ps {
		blocks = append(blocks, positionRow(p)...)
		total += p.Amount
	}
	blocks = append(blocks, slack.NewContextBlock("me_total", mrkdwn(fmt.Sprintf("Total staked: *%s*", total))))
	return blocks
}
