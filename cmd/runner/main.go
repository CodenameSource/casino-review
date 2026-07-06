// runner executes spin jobs: renders/posts the GIF, waits out the display
// window, runs the winning review engine (claude persona / static analyzer /
// external-bot dispatch), and records the run. Its image carries the
// toolchain: git, node (eslint/tsc), and the claude CLI.
package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"golang.org/x/sync/errgroup"

	"casino-review/internal/config"
	"casino-review/internal/github"
	"casino-review/internal/review"
	"casino-review/internal/selector"
	"casino-review/internal/spin"
	"casino-review/internal/store"
	"casino-review/internal/telemetry"
	"casino-review/internal/worker"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[runner] ")

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.DatabaseURL == "" {
		log.Fatalf("DATABASE_URL is required")
	}

	registry, err := review.LoadRegistry(cfg.ReviewsFile, cfg.Reviews, cfg.Trigger)
	if err != nil {
		log.Fatalf("reviews registry: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil { // advisory-locked; safe alongside core
		log.Fatalf("migrate: %v", err)
	}

	tel := telemetry.New()
	defer tel.Close()

	gh := github.New(cfg.Token, cfg.Owner, cfg.Repo)
	ghAssets := gh
	if cfg.AssetsOwner != cfg.Owner || cfg.AssetsRepo != cfg.Repo {
		ghAssets = github.New(cfg.Token, cfg.AssetsOwner, cfg.AssetsRepo)
	}

	deps := review.Deps{
		GH: gh, Token: cfg.Token,
		Checkouts: review.NewCheckouts(cfg.Workdir, cfg.Token),
		ClaudeBin: cfg.ClaudeBin,
	}
	engines, err := review.BuildAll(registry, deps)
	if err != nil {
		log.Fatalf("build engines: %v", err)
	}
	addon, err := review.BuildAddon(registry.Addon, deps)
	if err != nil {
		log.Fatalf("build addon: %v", err)
	}
	if addon != nil {
		log.Printf("bonus addon %q armed at %.0f%% chance", addon.Engine.Name(), addon.Chance*100)
	}

	spinner := &spin.Spinner{GH: gh, Assets: ghAssets, Branch: cfg.AssetsBranch}
	w := worker.New(cfg, st, tel, gh, spinner, engines, registry.Names(), selector.Random{}, addon)

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return w.Run(ctx) })
	g.Go(func() error { return telemetry.ServeMetrics(ctx, cfg.MetricsAddr) })

	if err := g.Wait(); err != nil && err != context.Canceled {
		log.Fatalf("runner: %v", err)
	}
}
