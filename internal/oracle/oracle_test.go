package oracle

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"casino-review/internal/config"
	"casino-review/internal/github"
	"casino-review/internal/ledger"
	"casino-review/internal/market"
	"casino-review/internal/store"
	"casino-review/internal/telemetry"
)

// fakePR is an in-memory prSource: a PR not in the map reads as still-open.
type fakePR struct{ m map[int]*github.PullStatus }

func (f fakePR) PullStatus(_ context.Context, n int) (*github.PullStatus, error) {
	if ps, ok := f.m[n]; ok {
		return ps, nil
	}
	return &github.PullStatus{Number: n, State: "open"}, nil
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping DB-backed oracle tests")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

// testOracle builds an oracle over a fresh, uniquely-named repo (so runs don't
// collide via the one-live-market-per-context index) with an injectable PR source.
func testOracle(t *testing.T, prs map[int]*github.PullStatus) (*Oracle, *ledger.Ledger, *store.Store, *config.Config) {
	st := openTestStore(t)
	cfg := &config.Config{Owner: "acme", Repo: fmt.Sprintf("widget-%d", time.Now().UnixNano())}
	led := ledger.New(st)
	tel := telemetry.New()
	t.Cleanup(tel.Close)
	svc := market.NewService(cfg, led, tel)
	return New(cfg, svc, led, fakePR{m: prs}, st), led, st, cfg
}

func ref(cfg *config.Config, pr int) string {
	return fmt.Sprintf("pr:%s/%s#%d", cfg.Owner, cfg.Repo, pr)
}

func assertResolved(t *testing.T, led *ledger.Ledger, id int64, outcome string) {
	t.Helper()
	m, _, err := led.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if m.State != ledger.StateResolved {
		t.Fatalf("market %d state %s, want RESOLVED", id, m.State)
	}
	if got := m.Resolution["outcome"]; got != outcome {
		t.Fatalf("market %d outcome %v, want %s", id, got, outcome)
	}
}

func TestOracleBountyPaysOnMerge(t *testing.T) {
	prs := map[int]*github.PullStatus{}
	o, led, _, cfg := testOracle(t, prs)
	ctx := context.Background()
	pr := 201

	m, err := led.FindOrCreateBounty(ctx, ref(cfg, pr), "bounty q", "cli:test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := led.PlacePosition(ctx, m.ID, "slack:A", "merged", 10_000_000); err != nil {
		t.Fatal(err)
	}
	// Not merged yet → nothing to do.
	if acts, _ := o.Once(ctx, true); len(acts) != 0 {
		t.Fatalf("expected no action before merge, got %+v", acts)
	}
	// Merge it.
	at := time.Now()
	ps := &github.PullStatus{Number: pr, State: "closed", Merged: true, MergedAt: &at}
	ps.User.Login = "octocat"
	prs[pr] = ps

	acts, err := o.Once(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 1 || acts[0].Op != "resolve" || acts[0].Outcome != "merged" || acts[0].Solver != "github:octocat" {
		t.Fatalf("bounty settlement wrong: %+v", acts)
	}
	assertResolved(t, led, m.ID, "merged")
	// Idempotent: a second pass touches nothing.
	if acts, _ := o.Once(ctx, true); len(acts) != 0 {
		t.Fatalf("second pass should be a no-op, got %+v", acts)
	}
}

func TestOracleMergeBy(t *testing.T) {
	prs := map[int]*github.PullStatus{}
	o, led, _, cfg := testOracle(t, prs)
	ctx := context.Background()

	// "no": deadline already passed, never merged.
	past := time.Now().Add(-time.Hour)
	mNo, err := led.CreateMarket(ctx, ledger.Market{Kind: "merge-by", ContextRef: ref(cfg, 211),
		Question: "q", Outcomes: []string{"yes", "no"}, CreatedBy: "cli:test", ResolvesBy: &past})
	if err != nil {
		t.Fatal(err)
	}
	led.PlacePosition(ctx, mNo.ID, "slack:A", "no", 5_000_000)

	// "yes": future deadline, merged before it.
	future := time.Now().Add(time.Hour)
	mYes, err := led.CreateMarket(ctx, ledger.Market{Kind: "merge-by", ContextRef: ref(cfg, 212),
		Question: "q", Outcomes: []string{"yes", "no"}, CreatedBy: "cli:test", ResolvesBy: &future})
	if err != nil {
		t.Fatal(err)
	}
	led.PlacePosition(ctx, mYes.ID, "slack:A", "yes", 5_000_000)
	at := time.Now()
	prs[212] = &github.PullStatus{Number: 212, Merged: true, MergedAt: &at}

	if _, err := o.Once(ctx, true); err != nil {
		t.Fatal(err)
	}
	assertResolved(t, led, mNo.ID, "no")
	assertResolved(t, led, mYes.ID, "yes")
}

func TestOracleFindingsCount(t *testing.T) {
	prs := map[int]*github.PullStatus{}
	o, led, st, cfg := testOracle(t, prs)
	ctx := context.Background()
	pr := 221

	m, err := led.CreateMarket(ctx, ledger.Market{Kind: "findings-count", ContextRef: ref(cfg, pr),
		Question: "q", Outcomes: []string{"0", "1-2", "3-5", "6+"}, CreatedBy: "cli:test"})
	if err != nil {
		t.Fatal(err)
	}
	led.PlacePosition(ctx, m.ID, "slack:A", "3-5", 5_000_000)

	// No review recorded yet → not resolvable.
	if acts, _ := o.Once(ctx, true); len(acts) != 0 {
		t.Fatalf("no review yet should be a no-op, got %+v", acts)
	}
	// Record a review with 4 findings → bucket "3-5".
	four := 4
	if _, err := st.InsertReviewRun(ctx, store.ReviewRun{Repo: cfg.RepoSlug(), PR: pr,
		Engine: "eslint", EngineKind: "analyzer", FindingsCount: &four,
		StartedAt: time.Now(), FinishedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// Dry-run: reports the action but changes nothing.
	acts, err := o.Once(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 1 || acts[0].Outcome != "3-5" {
		t.Fatalf("dry-run should propose 3-5, got %+v", acts)
	}
	if got, _, _ := led.Get(ctx, m.ID); got.State != ledger.StateOpen {
		t.Fatalf("dry-run must not mutate; state is %s", got.State)
	}
	// Apply.
	if _, err := o.Once(ctx, true); err != nil {
		t.Fatal(err)
	}
	assertResolved(t, led, m.ID, "3-5")
}
