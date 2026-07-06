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
//	casino market fund <ctx> <amount> [--as id]
//	casino market create <ctx> <kind> [deadline] [--as id]
//	casino market bet <id> <outcome> <amount> [--as id]
//	casino market board
//	casino market refund <id> [--as id]
//	casino market lock <id> [--as id]
//	casino market resolve <id> <outcome> [--solver login] [--as id]
//	casino market void <id> [reason] [--as id]
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
	mustID := func(i int) int64 {
		if len(rest) <= i {
			usage()
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(rest[i], "#"), 10, 64)
		if err != nil {
			log.Fatalf("%q is not a market id", rest[i])
		}
		return id
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
		if len(rest) < 3 {
			usage()
		}
		if err := svc.Bet(ctx, mustID(0), actor, rest[1], mustAmt(2)); err != nil {
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

	case "refund":
		amt, err := svc.Refund(ctx, mustID(0), actor)
		if err != nil {
			log.Fatalf("%v", err)
		}
		fmt.Printf("refunded %s\n", amt)

	case "lock":
		if err := svc.Lock(ctx, mustID(0), actor); err != nil {
			log.Fatalf("%v", err)
		}
		fmt.Println("locked")

	case "resolve":
		if len(rest) < 2 {
			usage()
		}
		payouts, err := svc.Resolve(ctx, mustID(0), rest[1], solver, actor, map[string]any{"resolved_via": "cli-admin"})
		if err != nil {
			log.Fatalf("%v", err)
		}
		for _, p := range payouts {
			fmt.Printf("%s -> %s (%s)\n", p.Payee, p.Amount, p.Reason)
		}

	case "void":
		reason := strings.Join(rest[1:], " ")
		refunds, err := svc.Void(ctx, mustID(0), actor, reason)
		if err != nil {
			log.Fatalf("%v", err)
		}
		fmt.Printf("voided; %d stake(s) refunded\n", len(refunds))

	default:
		usage()
	}
}
