// Package telemetry is the experiment's measurement layer, on three planes:
//
//  1. events — append-only rows in Postgres, the scientific record. Market
//     and money events must be written in the same transaction as the state
//     change they describe, so the experiment log can never drift from the
//     ledger. Spin events log the full assignment (candidate pool, weights,
//     chosen index): the slot machine is a randomizer, so every spin is a
//     random assignment — an RCT — and assignments must be recorded, not
//     just outcomes.
//  2. PostHog — behavioral/product analytics (Slack funnels, bet timing,
//     retention, claude run cost). Fire-and-forget and buffered: analytics
//     must never block or fail a money path. No-ops when unconfigured.
//  3. Prometheus — ops metrics served on METRICS_ADDR.
package telemetry

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/posthog/posthog-go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Execer is the exact Exec signature shared by pgxpool.Pool and pgx.Tx —
// pass the transaction when the event belongs to a state change.
type Execer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// Event is one row of the scientific record.
type Event struct {
	Type       string // e.g. "spin.assigned", "review.completed"
	Actor      string // "slack:U…" | "github:login" | "system"
	ContextRef string // "pr:owner/repo#N" | "ext:KEY" | ""
	Payload    any    // JSON-marshalled
}

// T is the telemetry hub shared by all services.
type T struct {
	posthog posthog.Client
	log     *log.Logger
}

// New builds the hub. posthogKey=="" disables PostHog (no-op).
func New(posthogKey, posthogHost string) *T {
	t := &T{log: log.Default()}
	if posthogKey != "" {
		cfg := posthog.Config{}
		if posthogHost != "" {
			cfg.Endpoint = posthogHost
		}
		client, err := posthog.NewWithConfig(posthogKey, cfg)
		if err != nil {
			log.Printf("telemetry: posthog disabled: %v", err)
		} else {
			t.posthog = client
		}
	}
	return t
}

func (t *T) Close() {
	if t.posthog != nil {
		t.posthog.Close()
	}
}

// Emit writes an event row using the given executor — pass the transaction
// for money/market events so the record commits atomically with the change.
func Emit(ctx context.Context, exec Execer, ev Event) error {
	payload, err := json.Marshal(ev.Payload)
	if err != nil {
		return err
	}
	_, err = exec.Exec(ctx,
		`INSERT INTO events (event_type, actor, context_ref, payload) VALUES ($1,$2,$3,$4)`,
		ev.Type, ev.Actor, ev.ContextRef, payload)
	return err
}

// Track sends a behavioral event to PostHog. Never blocks, never errors the
// caller; silently a no-op when PostHog is not configured.
func (t *T) Track(distinctID, event string, props map[string]any) {
	if t == nil || t.posthog == nil {
		return
	}
	p := posthog.NewProperties()
	for k, v := range props {
		p.Set(k, v)
	}
	if err := t.posthog.Enqueue(posthog.Capture{
		DistinctId: distinctID,
		Event:      event,
		Properties: p,
	}); err != nil {
		t.log.Printf("telemetry: posthog enqueue: %v", err)
	}
}

// --- Prometheus (ops plane) ---

var (
	JobsProcessed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "casino_jobs_processed_total",
		Help: "Jobs processed by the worker, by kind and outcome.",
	}, []string{"kind", "outcome"})

	JobDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "casino_job_duration_seconds",
		Help:    "Wall-clock duration of jobs by kind.",
		Buckets: prometheus.ExponentialBuckets(1, 2, 12), // 1s .. ~1h
	}, []string{"kind"})

	ReviewRuns = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "casino_review_runs_total",
		Help: "Review engine executions, by engine and outcome.",
	}, []string{"engine", "outcome"})

	ReviewFindings = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "casino_review_findings",
		Help:    "Findings per completed review run, by engine.",
		Buckets: []float64{0, 1, 2, 3, 5, 8, 13, 21},
	}, []string{"engine"})

	PollLag = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "casino_poll_lag_seconds",
		Help: "Age of the poller watermark, by poller.",
	}, []string{"poller"})

	GithubRateRemaining = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "casino_github_rate_remaining",
		Help: "Remaining GitHub API rate limit as last observed.",
	})

	ClaudeCostUSD = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "casino_claude_cost_usd_total",
		Help: "Estimated cumulative claude spend, by engine.",
	}, []string{"engine"})
)

// ServeMetrics exposes /metrics until ctx is done. addr=="" disables it.
func ServeMetrics(ctx context.Context, addr string) error {
	if addr == "" {
		<-ctx.Done()
		return nil
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx)
		return nil
	case err := <-errCh:
		return err
	}
}
