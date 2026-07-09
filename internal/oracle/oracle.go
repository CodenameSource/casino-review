// Package oracle auto-settles markets. Each pass it reads the ledger's open
// markets and, for those whose outcome is now determined, resolves or voids
// them: bounty/merge-by from GitHub merge status, findings-count from the
// recorded review findings, and any expired deadline-bearing market by voiding.
// Resolution is state-guarded in the ledger, so a pass is idempotent — only
// still-open markets are ever touched, and no watermark is needed.
package oracle

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"casino-review/internal/config"
	"casino-review/internal/github"
	"casino-review/internal/ledger"
	"casino-review/internal/market"
	"casino-review/internal/store"
)

// prSource is the merge-status dependency, satisfied by *github.Client and
// narrowed so the oracle's decision logic is testable without a live GitHub.
type prSource interface {
	PullStatus(ctx context.Context, number int) (*github.PullStatus, error)
}

type Oracle struct {
	cfg *config.Config
	svc *market.Service
	led *ledger.Ledger
	gh  prSource
	st  *store.Store
}

func New(cfg *config.Config, svc *market.Service, led *ledger.Ledger, gh prSource, st *store.Store) *Oracle {
	return &Oracle{cfg: cfg, svc: svc, led: led, gh: gh, st: st}
}

// Action is a settlement the oracle took (or, in dry-run, would take).
type Action struct {
	MarketID   int64
	Kind       string
	ContextRef string
	Op         string // "resolve" | "void"
	Outcome    string // resolve target
	Solver     string // bounty payee (github:login)
	Reason     string // void reason
	Err        string // apply error, if any (dry-run leaves this empty)
}

// Run scans on a ticker until ctx is cancelled.
func (o *Oracle) Run(ctx context.Context) error {
	log.Printf("oracle: auto-resolving markets every %s", o.cfg.OraclePollInterval)
	tick := time.NewTicker(o.cfg.OraclePollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			acts, err := o.Once(ctx, true)
			if err != nil {
				log.Printf("oracle: %v", err)
				continue
			}
			for _, a := range acts {
				if a.Err != "" {
					log.Printf("oracle: %s #%d failed: %s", a.Op, a.MarketID, a.Err)
				} else {
					log.Printf("oracle: %s #%d (%s) → %s%s", a.Op, a.MarketID, a.ContextRef, a.Outcome+a.Reason, solverNote(a))
				}
			}
		}
	}
}

// Once runs one settlement pass. With apply=false it only reports the actions it
// would take (the `casino oracle once` dry-run) and never mutates.
func (o *Oracle) Once(ctx context.Context, apply bool) ([]Action, error) {
	markets, err := o.led.ResolvableMarkets(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	// Per-pass PR-status cache: markets sharing a PR (a bounty + a merge-by)
	// need only one GET, and a PR that errored once isn't retried this pass.
	cache := map[int]*github.PullStatus{}
	failed := map[int]bool{}
	var actions []Action
	for _, m := range markets {
		a, ok := o.decide(ctx, m, now, cache, failed)
		if !ok {
			continue
		}
		if apply {
			if err := o.applyAction(ctx, a); err != nil {
				a.Err = err.Error()
			}
		}
		actions = append(actions, a)
	}
	return actions, nil
}

// decide determines the settlement for one market, or ok=false to leave it.
func (o *Oracle) decide(ctx context.Context, m ledger.Market, now time.Time, cache map[int]*github.PullStatus, failed map[int]bool) (Action, bool) {
	base := Action{MarketID: m.ID, Kind: m.Kind, ContextRef: m.ContextRef}
	switch m.Kind {
	case "bounty":
		// Pays the PR author immediately on merge (challenge window 0). A merged
		// PR with no resolvable author (deleted account) is left for manual
		// settling rather than paid to an unclaimable "github:" payee.
		if ps, ok := o.prStatus(ctx, m, cache, failed); ok && ps.Merged && ps.User.Login != "" {
			base.Op, base.Outcome, base.Solver = "resolve", "merged", "github:"+ps.User.Login
			return base, true
		}

	case "merge-by":
		ps, ok := o.prStatus(ctx, m, cache, failed)
		if ok && ps.Merged && ps.MergedAt != nil && m.ResolvesBy != nil && !ps.MergedAt.After(*m.ResolvesBy) {
			base.Op, base.Outcome = "resolve", "yes" // merged in time
			return base, true
		}
		if m.ResolvesBy != nil && now.After(*m.ResolvesBy) {
			// "no" requires a SUCCESSFUL, scoped status confirming not-merged-in-
			// time — the "yes" branch already returned if it merged in time, so
			// ok here means confirmed-not. Never settle "no" on a GitHub error
			// (leave OPEN, retry next pass) — that would blindly pay the wrong
			// side if the PR actually merged just before the deadline.
			switch {
			case ok:
				base.Op, base.Outcome = "resolve", "no"
				return base, true
			case !o.observable(m):
				// A merge-by on a repo/ext ref this oracle can never observe
				// can't be settled by merge status → void so stakes refund.
				base.Op, base.Reason = "void", "deadline passed on an unobservable market"
				return base, true
			}
		}

	case "findings-count":
		if n, ok := o.prNumber(m); ok {
			count, has, err := o.st.FindingsForPR(ctx, o.cfg.RepoSlug(), n)
			if err != nil {
				log.Printf("oracle: findings for %s: %v", m.ContextRef, err)
			} else if has {
				if bucket, ok := market.FindingsBucket(count, m.Outcomes); ok {
					base.Op, base.Outcome = "resolve", bucket
					return base, true
				}
			}
		}

	default:
		// An unknown deadline-bearing kind past its deadline can't self-resolve;
		// void it so stakes refund rather than lock forever.
		if m.ResolvesBy != nil && now.After(*m.ResolvesBy) {
			base.Op, base.Reason = "void", "expired without resolution"
			return base, true
		}
	}
	return Action{}, false
}

func (o *Oracle) applyAction(ctx context.Context, a Action) error {
	ctx = ledger.WithVia(ctx, "oracle")
	switch a.Op {
	case "resolve":
		_, err := o.svc.Resolve(ctx, a.MarketID, a.Outcome, a.Solver, "oracle",
			map[string]any{"resolved_via": "oracle"})
		if errors.Is(err, ledger.ErrBadState) {
			return nil // already settled concurrently — fine
		}
		return err
	case "void":
		_, err := o.svc.Void(ctx, a.MarketID, "oracle", a.Reason)
		if errors.Is(err, ledger.ErrBadState) {
			return nil
		}
		return err
	}
	return nil
}

// prStatus fetches merge state for a PR-context market in the configured repo
// (ok=false for ext: markets, markets on another repo, or a GitHub error). The
// per-pass cache de-dupes shared PRs; a PR that errored once is not retried.
func (o *Oracle) prStatus(ctx context.Context, m ledger.Market, cache map[int]*github.PullStatus, failed map[int]bool) (*github.PullStatus, bool) {
	n, ok := o.prNumber(m)
	if !ok {
		return nil, false
	}
	if failed[n] {
		return nil, false
	}
	if ps, ok := cache[n]; ok {
		return ps, true
	}
	ps, err := o.gh.PullStatus(ctx, n)
	if err != nil {
		log.Printf("oracle: pull status %s: %v", m.ContextRef, err)
		failed[n] = true
		return nil, false
	}
	cache[n] = ps
	return ps, true
}

// observable reports whether the oracle can read this market's PR merge status
// (i.e. it's a PR on the configured repo).
func (o *Oracle) observable(m ledger.Market) bool {
	_, ok := o.prNumber(m)
	return ok
}

// prNumber returns the PR number for a market on the configured repo.
func (o *Oracle) prNumber(m ledger.Market) (int, bool) {
	prefix := "pr:" + strings.ToLower(o.cfg.Owner) + "/" + strings.ToLower(o.cfg.Repo) + "#"
	if !strings.HasPrefix(m.ContextRef, prefix) {
		return 0, false
	}
	return market.PRNumber(m.ContextRef)
}

func solverNote(a Action) string {
	if a.Solver != "" {
		return " (" + a.Solver + ")"
	}
	return ""
}
