package main

import (
	"context"
	"fmt"
	"log"

	"casino-review/internal/config"
	"casino-review/internal/github"
	"casino-review/internal/ledger"
	"casino-review/internal/market"
	"casino-review/internal/oracle"
	"casino-review/internal/store"
	"casino-review/internal/telemetry"
)

// runOracle is the settlement dry-run/apply CLI:
//
//	casino oracle once            # report what would settle, change nothing
//	casino oracle once --apply    # actually resolve/void those markets
func runOracle(cfg *config.Config, args []string) {
	if cfg.DatabaseURL == "" {
		log.Fatalf("DATABASE_URL is required")
	}
	apply, sub := false, ""
	for _, a := range args {
		switch a {
		case "--apply":
			apply = true
		default:
			if sub == "" {
				sub = a
			}
		}
	}
	if sub != "once" {
		log.Fatalf("usage: casino oracle once [--apply]")
	}

	ctx := context.Background()
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

	led := ledger.New(st)
	svc := market.NewService(cfg, led, tel)
	gh := github.New(cfg.Token, cfg.Owner, cfg.Repo)
	o := oracle.New(cfg, svc, led, gh, st)

	acts, err := o.Once(ctx, apply)
	if err != nil {
		log.Fatalf("oracle: %v", err)
	}
	mode := "DRY-RUN (no changes)"
	if apply {
		mode = "APPLIED"
	}
	fmt.Printf("oracle once — %s — %d settlement(s)\n", mode, len(acts))
	for _, a := range acts {
		target := a.Outcome
		if a.Op == "void" {
			target = a.Reason
		}
		if a.Solver != "" {
			target += " (" + a.Solver + ")"
		}
		line := fmt.Sprintf("  %-7s #%-4d %-14s %-28s → %s", a.Op, a.MarketID, a.Kind, a.ContextRef, target)
		if a.Err != "" {
			line += "   ⚠️ " + a.Err
		}
		fmt.Println(line)
	}
}
