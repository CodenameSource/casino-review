// slackbot is the market's Slack surface: a Socket-Mode app (no public URL)
// honored in a single channel, plus a notification tailer over the events
// spine so actions from any surface reach the channel.
package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"golang.org/x/sync/errgroup"

	"casino-review/internal/config"
	"casino-review/internal/ledger"
	"casino-review/internal/market"
	"casino-review/internal/slackbot"
	"casino-review/internal/store"
	"casino-review/internal/telemetry"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[slackbot] ")

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	switch {
	case cfg.DatabaseURL == "":
		log.Fatalf("DATABASE_URL is required")
	case cfg.SlackBotToken == "" || cfg.SlackAppToken == "":
		log.Fatalf("SLACK_BOT_TOKEN (xoxb-…) and SLACK_APP_TOKEN (xapp-…) are required")
	case cfg.SlackChannel == "":
		log.Fatalf("SLACK_CHANNEL is required (channel ID or #name)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil { // advisory-locked; safe alongside core/runner
		log.Fatalf("migrate: %v", err)
	}

	tel := telemetry.New()
	defer tel.Close()

	svc := market.NewService(cfg, ledger.New(st), tel)
	bot := slackbot.New(cfg, svc, st, tel)

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return bot.Run(ctx) })
	g.Go(func() error { return telemetry.ServeMetrics(ctx, cfg.MetricsAddr) })

	if err := g.Wait(); err != nil && err != context.Canceled {
		log.Fatalf("slackbot: %v", err)
	}
}
