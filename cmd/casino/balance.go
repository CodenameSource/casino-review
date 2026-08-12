package main

import (
	"context"
	"fmt"
	"log"

	"casino-review/internal/config"
	"casino-review/internal/ledger"
	"casino-review/internal/store"
	"casino-review/internal/telemetry"
)

// runBalance is the balance admin/inspection CLI:
//
//	casino balance <participant>                  # show a balance
//	casino balance credit <participant> <amount>  # seed/deposit funds (admin)
func runBalance(cfg *config.Config, args []string) {
	if cfg.DatabaseURL == "" {
		log.Fatalf("DATABASE_URL is required")
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
	led := ledger.New(st, ledger.WithBalances(cfg.BalancesEnabled))

	switch {
	case len(args) >= 3 && args[0] == "credit":
		participant := args[1]
		amt, err := ledger.ParseUSDC(args[2])
		if err != nil {
			log.Fatalf("%v", err)
		}
		if err := led.Credit(ctx, participant, amt, "admin-credit"); err != nil {
			log.Fatalf("%v", err)
		}
		bal, _ := led.Balance(ctx, participant)
		fmt.Printf("credited %s → %s · balance now %s\n", amt, participant, bal)
	case len(args) >= 1:
		bal, err := led.Balance(ctx, args[0])
		if err != nil {
			log.Fatalf("%v", err)
		}
		fmt.Printf("%s: %s\n", args[0], bal)
	default:
		log.Fatalf("usage: casino balance <participant> | casino balance credit <participant> <amount>")
	}
}
