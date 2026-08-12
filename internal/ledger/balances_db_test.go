package ledger

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// balanceLedger is a balances-enforcing ledger over the test store.
func balanceLedger(t *testing.T) (*Ledger, context.Context) {
	return New(openTestStore(t), WithBalances(true)), context.Background()
}

func uniq(prefix string) string { return fmt.Sprintf("slack:%s%d", prefix, time.Now().UnixNano()) }

func TestBalanceCreditDebit(t *testing.T) {
	l, ctx := balanceLedger(t)
	p := uniq("A")
	if b, _ := l.Balance(ctx, p); b != 0 {
		t.Fatalf("new balance = %s, want 0", b)
	}
	if err := l.Credit(ctx, p, 50_000_000, "deposit"); err != nil {
		t.Fatal(err)
	}
	if b, _ := l.Balance(ctx, p); b != 50_000_000 {
		t.Fatalf("after credit = %s, want $50", b)
	}
}

func TestBalanceBetDebitsThenRefundCredits(t *testing.T) {
	l, ctx := balanceLedger(t)
	p := uniq("B")
	l.Credit(ctx, p, 30_000_000, "deposit") // $30
	m := testMarket(t, l, "merge-by", []string{"yes", "no"})

	if _, err := l.PlacePosition(ctx, m.ID, p, "yes", 10_000_000); err != nil {
		t.Fatal(err)
	}
	if b, _ := l.Balance(ctx, p); b != 20_000_000 {
		t.Fatalf("after $10 bet = %s, want $20", b)
	}
	if _, err := l.Refund(ctx, m.ID, p); err != nil {
		t.Fatal(err)
	}
	if b, _ := l.Balance(ctx, p); b != 30_000_000 {
		t.Fatalf("after refund = %s, want $30", b)
	}
}

func TestBalanceInsufficientRejectedAtomically(t *testing.T) {
	l, ctx := balanceLedger(t)
	p := uniq("C")
	l.Credit(ctx, p, 5_000_000, "deposit") // $5
	m := testMarket(t, l, "merge-by", []string{"yes", "no"})

	if _, err := l.PlacePosition(ctx, m.ID, p, "yes", 10_000_000); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("expected ErrInsufficientFunds, got %v", err)
	}
	// The whole tx rolled back: balance untouched, no position placed.
	if b, _ := l.Balance(ctx, p); b != 5_000_000 {
		t.Fatalf("balance moved on a failed bet: %s", b)
	}
	if pool, err := poolTotal(ctx, l, m.ID); err != nil || pool != 0 {
		t.Fatalf("a position was placed despite insufficient funds (pool %s, %v)", pool, err)
	}
}

func TestBalanceWinCredits(t *testing.T) {
	l, ctx := balanceLedger(t)
	winner, loser := uniq("W"), uniq("L")
	l.Credit(ctx, winner, 10_000_000, "deposit")
	l.Credit(ctx, loser, 10_000_000, "deposit")
	m := testMarket(t, l, "merge-by", []string{"yes", "no"})
	l.PlacePosition(ctx, m.ID, winner, "yes", 10_000_000)
	l.PlacePosition(ctx, m.ID, loser, "no", 10_000_000)

	if _, err := l.Resolve(ctx, m.ID, "yes", "parimutuel", "", "test", nil); err != nil {
		t.Fatal(err)
	}
	// Winner takes the whole $20 pool; loser gets nothing. Money conserved.
	if b, _ := l.Balance(ctx, winner); b != 20_000_000 {
		t.Fatalf("winner balance = %s, want $20", b)
	}
	if b, _ := l.Balance(ctx, loser); b != 0 {
		t.Fatalf("loser balance = %s, want $0", b)
	}
}

func TestBalanceConcurrentNoDoubleSpend(t *testing.T) {
	l, ctx := balanceLedger(t)
	p := uniq("X")
	l.Credit(ctx, p, 10_000_000, "deposit") // $10 — affords exactly one $10 bet
	m := testMarket(t, l, "merge-by", []string{"yes", "no"})

	const n = 8
	var wg sync.WaitGroup
	succ := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.PlacePosition(ctx, m.ID, p, "yes", 10_000_000); err == nil {
				succ <- struct{}{}
			}
		}()
	}
	wg.Wait()
	close(succ)
	if got := len(succ); got != 1 {
		t.Fatalf("concurrent bets: %d succeeded, want exactly 1", got)
	}
	if b, _ := l.Balance(ctx, p); b != 0 {
		t.Fatalf("balance after = %s, want $0", b)
	}
}

func poolTotal(ctx context.Context, l *Ledger, marketID int64) (USDC, error) {
	var v int64
	err := l.st.Pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(amount_usdc),0) FROM positions WHERE market_id=$1 AND status='ACTIVE'`, marketID).Scan(&v)
	return USDC(v), err
}
