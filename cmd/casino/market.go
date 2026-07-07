package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"casino-review/internal/config"
	"casino-review/internal/ledger"
	"casino-review/internal/market"
	"casino-review/internal/slackbot"
	"casino-review/internal/store"
	"casino-review/internal/telemetry"
)

// runMarket is the CLI mirror of the Slack surface — same Service, same
// ledger, useful for admin and for testing without a workspace.
//
// Markets are addressed by <#pr> <kind> (context form) or a bare <market#>:
//
//	casino market fund <ctx> <amount> [--as id]
//	casino market create <ctx> <kind> [deadline] [--as id]
//	casino market bet <#pr> <kind> <outcome> <amount> [--as id]   (or: bet <market#> <outcome> <amount>)
//	casino market board
//	casino market show <#pr>            # the PR's whole dashboard
//	casino market show <market#>        # one market
//	casino market me [--as id]
//	casino market refund <#pr> <kind> [--as id]   (or: refund <market#>)
//	casino market lock <#pr> <kind> [--as id]
//	casino market resolve <#pr> <kind> <outcome> [--solver login] [--as id]
//	casino market void <#pr> <kind> [reason] [--as id]
func runMarket(cfg *config.Config, args []string) {
	if cfg.DatabaseURL == "" {
		log.Fatalf("DATABASE_URL is required")
	}
	actor, solver, plain := "cli:admin", "", []string{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--as":
			if i+1 < len(args) {
				actor = args[i+1]
				i++
			}
		case "--solver":
			if i+1 < len(args) {
				solver = "github:" + strings.TrimPrefix(args[i+1], "github:")
				i++
			}
		default:
			plain = append(plain, args[i])
		}
	}
	if len(plain) == 0 {
		usage()
	}

	ctx := ledger.WithVia(context.Background(), "cli")
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	tel := telemetry.New()
	defer tel.Close()
	svc := market.NewService(cfg, ledger.New(st), tel)

	sub, rest := plain[0], plain[1:]
	// address resolves the leading market address in rest: context form
	// "<#pr> <kind> …" (via MarketFor) or id form "<market#> …". Returns the id
	// and the args following the address.
	address := func() (int64, []string) {
		if len(rest) == 0 {
			usage()
		}
		if isCtxRef(rest[0]) {
			if len(rest) < 2 {
				log.Fatalf("address a market by `<#pr> <kind>` (e.g. `#123 merge-by`)")
			}
			m, err := svc.MarketFor(ctx, rest[0], strings.ToLower(rest[1]))
			if err != nil {
				log.Fatalf("%v", err)
			}
			return m.ID, rest[2:]
		}
		id, err := strconv.ParseInt(rest[0], 10, 64)
		if err != nil {
			log.Fatalf("%q is not a market id (use `<#pr> <kind>` to address by PR)", rest[0])
		}
		return id, rest[1:]
	}
	mustAmt := func(i int) ledger.USDC {
		if len(rest) <= i {
			usage()
		}
		amt, err := ledger.ParseUSDC(rest[i])
		if err != nil {
			log.Fatalf("%v", err)
		}
		return amt
	}

	switch sub {
	case "fund":
		if len(rest) < 2 {
			usage()
		}
		m, err := svc.Fund(ctx, rest[0], actor, mustAmt(1))
		if err != nil {
			log.Fatalf("%v", err)
		}
		_, pool, _ := svc.Get(ctx, m.ID)
		fmt.Printf("market #%d (%s) pool now %s\n", m.ID, m.ContextRef, pool)

	case "create":
		if len(rest) < 2 {
			usage()
		}
		spec := map[string]any{}
		if rest[1] == "merge-by" {
			if len(rest) < 3 {
				log.Fatalf("merge-by needs a deadline (e.g. 72h)")
			}
			d, err := slackbot.ParseDeadline(rest[2], time.Now())
			if err != nil {
				log.Fatalf("%v", err)
			}
			spec["deadline"] = d
		}
		m, err := svc.Create(ctx, rest[1], rest[0], actor, spec)
		if err != nil {
			log.Fatalf("%v", err)
		}
		fmt.Printf("market #%d — %s\noutcomes: %s\n", m.ID, m.Question, strings.Join(m.Outcomes, ", "))

	case "bet":
		id, after := address()
		if len(after) < 2 {
			log.Fatalf("usage: bet <#pr> <kind> <outcome> <amount>  (or bet <market#> <outcome> <amount>)")
		}
		amt, err := ledger.ParseUSDC(after[1])
		if err != nil {
			log.Fatalf("%v", err)
		}
		if err := svc.Bet(ctx, id, actor, after[0], amt); err != nil {
			log.Fatalf("%v", err)
		}
		fmt.Println("placed")

	case "board":
		rows, err := svc.Board(ctx, 20)
		if err != nil {
			log.Fatalf("%v", err)
		}
		if len(rows) == 0 {
			fmt.Println("board is empty")
			return
		}
		for i, r := range rows {
			fmt.Printf("%2d. #%-4d %-28s %-10s %2d backer(s) [%s/%s] %s\n",
				i+1, r.Market.ID, r.Market.ContextRef, r.Pool, r.Participants,
				r.Market.Kind, r.Market.State, r.Market.Question)
		}

	case "show":
		if len(rest) == 0 {
			usage()
		}
		// context ref → the PR's whole dashboard; a bare number → one market.
		if isCtxRef(rest[0]) {
			ref, ds, err := svc.PRMarkets(ctx, rest[0], actor)
			if err != nil {
				log.Fatalf("%v", err)
			}
			if len(ds) == 0 {
				fmt.Printf("no markets on %s\n", ref)
				return
			}
			fmt.Printf("markets on %s:\n", ref)
			for _, d := range ds {
				printDetail(d)
			}
			return
		}
		id, err := strconv.ParseInt(rest[0], 10, 64)
		if err != nil {
			log.Fatalf("%q is not a market id", rest[0])
		}
		d, err := svc.Detail(ctx, id, actor)
		if err != nil {
			log.Fatalf("%v", err)
		}
		printDetail(d)

	case "me":
		ps, err := svc.MyPositions(ctx, actor)
		if err != nil {
			log.Fatalf("%v", err)
		}
		if len(ps) == 0 {
			fmt.Println("no open positions")
			return
		}
		var total ledger.USDC
		for _, p := range ps {
			fmt.Printf("#%-4d %-14s %-10s %-9s %s\n", p.MarketID, p.Kind, p.Outcome, p.Amount, p.ContextRef)
			total += p.Amount
		}
		fmt.Printf("total staked: %s\n", total)

	case "refund":
		id, _ := address()
		amt, err := svc.Refund(ctx, id, actor)
		if err != nil {
			log.Fatalf("%v", err)
		}
		fmt.Printf("refunded %s\n", amt)

	case "lock":
		id, _ := address()
		if err := svc.Lock(ctx, id, actor); err != nil {
			log.Fatalf("%v", err)
		}
		fmt.Println("locked")

	case "resolve":
		id, after := address()
		if len(after) < 1 {
			log.Fatalf("usage: resolve <#pr> <kind> <outcome>  (or resolve <market#> <outcome>)")
		}
		payouts, err := svc.Resolve(ctx, id, after[0], solver, actor, map[string]any{"resolved_via": "cli-admin"})
		if err != nil {
			log.Fatalf("%v", err)
		}
		for _, p := range payouts {
			fmt.Printf("%s -> %s (%s)\n", p.Payee, p.Amount, p.Reason)
		}

	case "void":
		id, after := address()
		refunds, err := svc.Void(ctx, id, actor, strings.Join(after, " "))
		if err != nil {
			log.Fatalf("%v", err)
		}
		fmt.Printf("voided; %d stake(s) refunded\n", len(refunds))

	default:
		usage()
	}
}

// isCtxRef mirrors the Slack parser: a token names a context (PR/ext key) rather
// than a bare market serial if it starts with '#' or contains ':' or '/'.
func isCtxRef(s string) bool {
	return strings.HasPrefix(s, "#") || strings.ContainsAny(s, ":/")
}

func printDetail(d market.Detail) {
	m := d.Market
	fmt.Printf("#%d %s on %s [%s] pool %s · %d backer(s)\n  %s\n",
		m.ID, m.Kind, m.ContextRef, m.State, d.Pool, d.Backers, m.Question)
	if m.Kind == "bounty" {
		if mine := d.MyStake["merged"]; mine > 0 {
			fmt.Printf("  your stake: %s\n", mine)
		}
		return
	}
	for _, o := range market.Odds(m.Outcomes, d.OutcomePools) {
		payout := "-"
		if o.PayoutX > 0 {
			payout = fmt.Sprintf("%.2fx", o.PayoutX)
		}
		mine := ""
		if v := d.MyStake[o.Outcome]; v > 0 {
			mine = fmt.Sprintf(" (you: %s)", v)
		}
		fmt.Printf("    %-12s %-9s %3d%% pays %s%s\n", o.Outcome, o.Pool, int(o.Prob*100+0.5), payout, mine)
	}
}
