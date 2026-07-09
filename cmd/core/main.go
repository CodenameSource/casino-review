// core is the trigger/coordination service: it polls the monitored repo for
// /casino-review comments, dedups, and enqueues spin jobs for the runner.
// It also owns schema migrations and the GIF asset TTL cleanup.
package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"golang.org/x/sync/errgroup"

	"casino-review/internal/config"
	"casino-review/internal/github"
	"casino-review/internal/ledger"
	"casino-review/internal/market"
	"casino-review/internal/monitor"
	"casino-review/internal/oracle"
	"casino-review/internal/store"
	"casino-review/internal/telemetry"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[core] ")

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.DatabaseURL == "" {
		log.Fatalf("DATABASE_URL is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return monitor.New(cfg, st, tel).Run(ctx) })
	g.Go(func() error { return telemetry.ServeMetrics(ctx, cfg.MetricsAddr) })

	// Resolution oracle: auto-settles markets on merge/findings/expiry. Its
	// events flow to Slack via the bot's tailer (via != "slack" is posted).
	if cfg.OracleEnabled {
		svc := market.NewService(cfg, ledger.New(st), tel)
		o := oracle.New(cfg, svc, ledger.New(st), github.New(cfg.Token, cfg.Owner, cfg.Repo), st)
		g.Go(func() error { return o.Run(ctx) })
	} else {
		log.Printf("core: resolution oracle disabled (ORACLE_ENABLED=false)")
	}

	if err := g.Wait(); err != nil && err != context.Canceled {
		log.Fatalf("core: %v", err)
	}
}
