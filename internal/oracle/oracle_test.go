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

// fakePR is an in-memory prSource: a PR in err returns an error, a PR in m
// returns its status, anything else reads as still-open.
type fakePR struct {
	m   map[int]*github.PullStatus
	err map[int]bool
}

func newFakePR() *fakePR {
	return &fakePR{m: map[int]*github.PullStatus{}, err: map[int]bool{}}
}

func (f *fakePR) PullStatus(_ context.Context, n int) (*github.PullStatus, error) {
	if f.err[n] {
		return nil, fmt.Errorf("github unavailable")
	}
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
func testOracle(t *testing.T, gh *fakePR) (*Oracle, *ledger.Ledger, *store.Store, *config.Config) {
	st := openTestStore(t)
	cfg := &config.Config{Owner: "acme", Repo: fmt.Sprintf("widget-%d", time.Now().UnixNano())}
	led := ledger.New(st)
	tel := telemetry.New()
	t.Cleanup(tel.Close)
	svc := market.NewService(cfg, led, tel)
	return New(cfg, svc, led, gh, st), led, st, cfg
}

func ref(cfg *config.Config, pr int) string {
	return fmt.Sprintf("pr:%s/%s#%d", cfg.Owner, cfg.Repo, pr)
}

func state(t *testing.T, led *ledger.Ledger, id int64) string {
	t.Helper()
	m, _, err := led.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return m.State
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

// untouched fails if the oracle settled market id in acts.
func untouched(t *testing.T, acts []Action, id int64, why string) {
	t.Helper()
	for _, a := range acts {
		if a.MarketID == id {
			t.Fatalf("%s: market %d should be untouched, got %+v", why, id, a)
		}
	}
}

func TestOracleBountyPaysOnMerge(t *testing.T) {
	gh := newFakePR()
	o, led, _, cfg := testOracle(t, gh)
	ctx := context.Background()
	pr := 201

	m, err := led.FindOrCreateBounty(ctx, ref(cfg, pr), "bounty q", "cli:test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := led.PlacePosition(ctx, m.ID, "slack:A", "merged", 10_000_000); err != nil {
		t.Fatal(err)
	}
	if acts, _ := o.Once(ctx, true); len(acts) != 0 {
		t.Fatalf("expected no action before merge, got %+v", acts)
	}
	at := time.Now()
	ps := &github.PullStatus{Number: pr, State: "closed", Merged: true, MergedAt: &at}
	ps.User.Login = "octocat"
	gh.m[pr] = ps

	acts, err := o.Once(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 1 || acts[0].Op != "resolve" || acts[0].Outcome != "merged" || acts[0].Solver != "github:octocat" {
		t.Fatalf("bounty settlement wrong: %+v", acts)
	}
	assertResolved(t, led, m.ID, "merged")
	if acts, _ := o.Once(ctx, true); len(acts) != 0 {
		t.Fatalf("second pass should be a no-op, got %+v", acts)
	}
}

// A merged PR with no resolvable author must be left for manual settling, not
// paid to an unclaimable "github:" payee.
func TestOracleBountyEmptyAuthorSkips(t *testing.T) {
	gh := newFakePR()
	o, led, _, cfg := testOracle(t, gh)
	ctx := context.Background()
	pr := 241
	m, err := led.FindOrCreateBounty(ctx, ref(cfg, pr), "q", "cli:test")
	if err != nil {
		t.Fatal(err)
	}
	led.PlacePosition(ctx, m.ID, "slack:A", "merged", 10_000_000)
	at := time.Now()
	gh.m[pr] = &github.PullStatus{Number: pr, Merged: true, MergedAt: &at} // User.Login == ""
	acts, _ := o.Once(ctx, true)
	untouched(t, acts, m.ID, "empty author")
	if s := state(t, led, m.ID); s != ledger.StateOpen {
		t.Fatalf("empty-author bounty should stay OPEN, got %s", s)
	}
}

func TestOracleMergeBy(t *testing.T) {
	gh := newFakePR()
	o, led, _, cfg := testOracle(t, gh)
	ctx := context.Background()

	past := time.Now().Add(-time.Hour)
	mNo, err := led.CreateMarket(ctx, ledger.Market{Kind: "merge-by", ContextRef: ref(cfg, 211),
		Question: "q", Outcomes: []string{"yes", "no"}, CreatedBy: "cli:test", ResolvesBy: &past})
	if err != nil {
		t.Fatal(err)
	}
	led.PlacePosition(ctx, mNo.ID, "slack:A", "no", 5_000_000)

	future := time.Now().Add(time.Hour)
	mYes, err := led.CreateMarket(ctx, ledger.Market{Kind: "merge-by", ContextRef: ref(cfg, 212),
		Question: "q", Outcomes: []string{"yes", "no"}, CreatedBy: "cli:test", ResolvesBy: &future})
	if err != nil {
		t.Fatal(err)
	}
	led.PlacePosition(ctx, mYes.ID, "slack:A", "yes", 5_000_000)
	at := time.Now()
	gh.m[212] = &github.PullStatus{Number: 212, Merged: true, MergedAt: &at}

	if _, err := o.Once(ctx, true); err != nil {
		t.Fatal(err)
	}
	assertResolved(t, led, mNo.ID, "no") // 211 unknown → reads open → confirmed not merged
	assertResolved(t, led, mYes.ID, "yes")
}

// BLOCKER regression: "no" must never be settled on an unconfirmed status. A
// GitHub error leaves the market OPEN (retried); it settles only once confirmed.
func TestOracleMergeByNoNeedsConfirmation(t *testing.T) {
	gh := newFakePR()
	o, led, _, cfg := testOracle(t, gh)
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)
	m, err := led.CreateMarket(ctx, ledger.Market{Kind: "merge-by", ContextRef: ref(cfg, 231),
		Question: "q", Outcomes: []string{"yes", "no"}, CreatedBy: "cli:test", ResolvesBy: &past})
	if err != nil {
		t.Fatal(err)
	}
	led.PlacePosition(ctx, m.ID, "slack:A", "no", 5_000_000)

	gh.err[231] = true // GitHub unreachable this pass
	acts, err := o.Once(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	untouched(t, acts, m.ID, "github error")
	if s := state(t, led, m.ID); s != ledger.StateOpen {
		t.Fatalf("must stay OPEN on GitHub error, got %s", s)
	}
	// GitHub recovers; PR confirmed not merged → now "no".
	delete(gh.err, 231)
	if _, err := o.Once(ctx, true); err != nil {
		t.Fatal(err)
	}
	assertResolved(t, led, m.ID, "no")
}

// A merge-by on a repo this oracle can't observe (foreign/ext) is voided at its
// deadline — never blindly resolved "no".
func TestOracleMergeByUnobservableVoids(t *testing.T) {
	gh := newFakePR()
	o, led, _, _ := testOracle(t, gh)
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)
	m, err := led.CreateMarket(ctx, ledger.Market{Kind: "merge-by", ContextRef: "pr:other/repo#9",
		Question: "q", Outcomes: []string{"yes", "no"}, CreatedBy: "cli:test", ResolvesBy: &past})
	if err != nil {
		t.Fatal(err)
	}
	led.PlacePosition(ctx, m.ID, "slack:A", "yes", 5_000_000)
	if _, err := o.Once(ctx, true); err != nil {
		t.Fatal(err)
	}
	if s := state(t, led, m.ID); s != ledger.StateVoided {
		t.Fatalf("unobservable merge-by should VOID, got %s", s)
	}
}

func TestOracleFindingsCount(t *testing.T) {
	gh := newFakePR()
	o, led, st, cfg := testOracle(t, gh)
	ctx := context.Background()
	pr := 221

	m, err := led.CreateMarket(ctx, ledger.Market{Kind: "findings-count", ContextRef: ref(cfg, pr),
		Question: "q", Outcomes: []string{"0", "1-2", "3-5", "6+"}, CreatedBy: "cli:test"})
	if err != nil {
		t.Fatal(err)
	}
	led.PlacePosition(ctx, m.ID, "slack:A", "3-5", 5_000_000)

	if acts, _ := o.Once(ctx, true); len(acts) != 0 {
		t.Fatalf("no review yet should be a no-op, got %+v", acts)
	}
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
	if s := state(t, led, m.ID); s != ledger.StateOpen {
		t.Fatalf("dry-run must not mutate; state is %s", s)
	}
	if _, err := o.Once(ctx, true); err != nil {
		t.Fatal(err)
	}
	assertResolved(t, led, m.ID, "3-5")
}
