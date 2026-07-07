package ledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"casino-review/internal/store"
)

// openTestStore connects to TEST_DATABASE_URL (skips otherwise) and migrates.
// Each test uses fresh markets, so tests don't interfere with each other.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping DB-backed ledger tests")
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

func testMarket(t *testing.T, l *Ledger, kind string, outcomes []string) Market {
	t.Helper()
	m, err := l.CreateMarket(context.Background(), Market{
		Kind: kind, ContextRef: fmt.Sprintf("ext:TEST-%d", time.Now().UnixNano()),
		Question: "test market", Outcomes: outcomes, CreatedBy: "cli:test",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return m
}

func TestLedgerLifecycle(t *testing.T) {
	st := openTestStore(t)
	l := New(st)
	ctx := context.Background()

	m := testMarket(t, l, "merge-by", []string{"yes", "no"})

	if _, err := l.PlacePosition(ctx, m.ID, "slack:A", "yes", 10_000_000); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PlacePosition(ctx, m.ID, "slack:B", "no", 30_000_000); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PlacePosition(ctx, m.ID, "slack:A", "maybe", 1); !errors.Is(err, ErrBadOutcome) {
		t.Fatalf("expected ErrBadOutcome, got %v", err)
	}

	// Refund own stake while OPEN.
	if amt, err := l.Refund(ctx, m.ID, "slack:A"); err != nil || amt != 10_000_000 {
		t.Fatalf("refund = %v, %v", amt, err)
	}
	if _, err := l.Refund(ctx, m.ID, "slack:A"); !errors.Is(err, ErrNoPosition) {
		t.Fatalf("double refund: expected ErrNoPosition, got %v", err)
	}

	// Re-stake, lock, then betting and refunds must be rejected.
	if _, err := l.PlacePosition(ctx, m.ID, "slack:A", "yes", 10_000_000); err != nil {
		t.Fatal(err)
	}
	if err := l.Lock(ctx, m.ID, "cli:test"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PlacePosition(ctx, m.ID, "slack:C", "yes", 1); !errors.Is(err, ErrBadState) {
		t.Fatalf("bet after lock: expected ErrBadState, got %v", err)
	}
	if _, err := l.Refund(ctx, m.ID, "slack:A"); !errors.Is(err, ErrBadState) {
		t.Fatalf("refund after lock: expected ErrBadState, got %v", err)
	}

	// Resolve parimutuel: pool 40 → winner A (10 on yes) gets all 40.
	payouts, err := l.Resolve(ctx, m.ID, "yes", "parimutuel", "", "cli:test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(payouts) != 1 || payouts[0].Payee != "slack:A" || payouts[0].Amount != 40_000_000 {
		t.Fatalf("payouts = %+v", payouts)
	}

	// Terminal: everything must be rejected now.
	if _, err := l.Resolve(ctx, m.ID, "no", "parimutuel", "", "cli:test", nil); !errors.Is(err, ErrBadState) {
		t.Fatalf("double resolve: expected ErrBadState, got %v", err)
	}
	if _, err := l.Void(ctx, m.ID, "cli:test", ""); !errors.Is(err, ErrBadState) {
		t.Fatalf("void after resolve: expected ErrBadState, got %v", err)
	}
}

func TestLedgerBountyResolve(t *testing.T) {
	st := openTestStore(t)
	l := New(st)
	ctx := context.Background()

	ref := fmt.Sprintf("ext:BNTY-%d", time.Now().UnixNano())
	m, err := l.FindOrCreateBounty(ctx, ref, "q", "cli:test")
	if err != nil {
		t.Fatal(err)
	}
	m2, err := l.FindOrCreateBounty(ctx, ref, "q", "cli:other")
	if err != nil || m2.ID != m.ID {
		t.Fatalf("find-or-create not idempotent: %v %v", m2.ID, err)
	}

	l.PlacePosition(ctx, m.ID, "slack:A", "merged", 25_000_000)
	l.PlacePosition(ctx, m.ID, "slack:B", "merged", 75_000_000)

	payouts, err := l.Resolve(ctx, m.ID, "merged", "solver", "github:author", "cli:test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(payouts) != 1 || payouts[0].Payee != "github:author" || payouts[0].Amount != 100_000_000 {
		t.Fatalf("payouts = %+v", payouts)
	}

	// The context is free for a new bounty after resolution.
	m3, err := l.FindOrCreateBounty(ctx, ref, "q", "cli:test")
	if err != nil || m3.ID == m.ID {
		t.Fatalf("expected fresh bounty after resolve, got %v %v", m3.ID, err)
	}
}

func TestLedgerVoidRefundsAll(t *testing.T) {
	st := openTestStore(t)
	l := New(st)
	ctx := context.Background()

	m := testMarket(t, l, "findings-count", []string{"0", "1-2", "3-5", "6+"})
	l.PlacePosition(ctx, m.ID, "slack:A", "0", 5_000_000)
	l.PlacePosition(ctx, m.ID, "slack:B", "6+", 7_000_000)

	refunds, err := l.Void(ctx, m.ID, "cli:test", "test void")
	if err != nil {
		t.Fatal(err)
	}
	var total USDC
	for _, r := range refunds {
		total += r.Amount
	}
	if len(refunds) != 2 || total != 12_000_000 {
		t.Fatalf("refunds = %+v", refunds)
	}
}

// The concurrency tripwire: N goroutines race to resolve one market; exactly
// one may succeed. Same for concurrent double-refund of one participant.
func TestLedgerConcurrentResolve(t *testing.T) {
	st := openTestStore(t)
	l := New(st)
	ctx := context.Background()

	m := testMarket(t, l, "merge-by", []string{"yes", "no"})
	l.PlacePosition(ctx, m.ID, "slack:A", "yes", 10_000_000)

	const n = 8
	var wg sync.WaitGroup
	succ := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.Resolve(ctx, m.ID, "yes", "parimutuel", "", "cli:test", nil); err == nil {
				succ <- struct{}{}
			}
		}()
	}
	wg.Wait()
	close(succ)
	if got := len(succ); got != 1 {
		t.Fatalf("concurrent resolve: %d succeeded, want exactly 1", got)
	}
}

func TestLedgerConcurrentRefund(t *testing.T) {
	st := openTestStore(t)
	l := New(st)
	ctx := context.Background()

	m := testMarket(t, l, "merge-by", []string{"yes", "no"})
	l.PlacePosition(ctx, m.ID, "slack:A", "yes", 10_000_000)

	const n = 8
	var wg sync.WaitGroup
	succ := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.Refund(ctx, m.ID, "slack:A"); err == nil {
				succ <- struct{}{}
			}
		}()
	}
	wg.Wait()
	close(succ)
	if got := len(succ); got != 1 {
		t.Fatalf("concurrent refund: %d succeeded, want exactly 1", got)
	}
}

// TestLiveMarketUniqueness verifies the "one live market per (kind, context)"
// rule that context-first addressing ("#123 merge-by") depends on.
func TestLiveMarketUniqueness(t *testing.T) {
	st := openTestStore(t)
	l := New(st)
	ctx := context.Background()
	ref := fmt.Sprintf("ext:UNIQ-%d", time.Now().UnixNano())

	m1, err := l.CreateMarket(ctx, Market{
		Kind: "merge-by", ContextRef: ref, Question: "q1",
		Outcomes: []string{"yes", "no"}, CreatedBy: "cli:test",
	})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}

	// A second live market of the same kind on the same context is rejected.
	if _, err := l.CreateMarket(ctx, Market{
		Kind: "merge-by", ContextRef: ref, Question: "q2",
		Outcomes: []string{"yes", "no"}, CreatedBy: "cli:test",
	}); err == nil {
		t.Fatal("expected a duplicate live merge-by market to be rejected")
	}

	// LiveMarket resolves (context, kind) to that one market.
	got, err := l.LiveMarket(ctx, ref, "merge-by")
	if err != nil {
		t.Fatalf("LiveMarket: %v", err)
	}
	if got.ID != m1.ID {
		t.Fatalf("LiveMarket returned #%d, want #%d", got.ID, m1.ID)
	}

	// A different kind on the same context is independent — allowed.
	if _, err := l.CreateMarket(ctx, Market{
		Kind: "findings-count", ContextRef: ref, Question: "q3",
		Outcomes: []string{"0", "1-2"}, CreatedBy: "cli:test",
	}); err != nil {
		t.Fatalf("different kind on same context should be allowed: %v", err)
	}

	// Voiding frees the slot: a fresh live market of that kind can be created.
	if _, err := l.Void(ctx, m1.ID, "cli:test", "cleanup"); err != nil {
		t.Fatalf("void: %v", err)
	}
	if _, err := l.CreateMarket(ctx, Market{
		Kind: "merge-by", ContextRef: ref, Question: "q4",
		Outcomes: []string{"yes", "no"}, CreatedBy: "cli:test",
	}); err != nil {
		t.Fatalf("recreate after void should be allowed: %v", err)
	}

	// MarketsForContext returns the non-voided markets on the context.
	ms, err := l.MarketsForContext(ctx, ref)
	if err != nil {
		t.Fatalf("MarketsForContext: %v", err)
	}
	if len(ms) != 2 { // the findings-count + the recreated merge-by; the voided one is excluded
		t.Fatalf("MarketsForContext returned %d markets, want 2", len(ms))
	}
}
