// Package telemetry is the experiment's measurement layer, on two planes:
//
//  1. events — append-only rows in Postgres, the scientific record. Market
//     and money events must be written in the same transaction as the state
//     change they describe, so the experiment log can never drift from the
//     ledger. Spin events log the full assignment (candidate pool, weights,
//     chosen index): the slot machine is a randomizer, so every spin is a
//     random assignment — an RCT — and assignments must be recorded, not
//     just outcomes.
//  2. Prometheus — ops metrics served on METRICS_ADDR.
//
// A third plane, PostHog behavioral analytics, was dropped for now: its client
// pulled in a compile-heavy dependency (goccy/go-json) that OOM-killed builds
// on small (1 GB) VMs. The Track seam below remains as a no-op so the call
// sites stay put — restoring PostHog is re-adding the client here plus the dep.
package telemetry

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
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

// T is the telemetry hub shared by all services. With PostHog dropped it holds
// no state, but stays as the seam for behavioral tracking.
type T struct{}

// New builds the hub.
func New() *T { return &T{} }

func (t *T) Close() {}

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

// Track is the behavioral-analytics seam. Currently a no-op (PostHog dropped —
// see the package doc). The Postgres events spine remains the durable record;
// this exists so behavioral tracking can be restored without touching callers.
func (t *T) Track(distinctID, event string, props map[string]any) {}

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
