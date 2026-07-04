// Package monitor is the trigger side of the pipeline: it polls the monitored
// repo's PR comments, dedups (reaction + job key), and enqueues spin jobs.
// Execution — the GIF, the engine run — happens in the runner (internal/worker).
package monitor

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"casino-review/internal/config"
	"casino-review/internal/github"
	"casino-review/internal/spin"
	"casino-review/internal/store"
	"casino-review/internal/telemetry"
)

const (
	// startupLookback bounds the first poll when no watermark is stored.
	// Reaction dedup makes lookback safe: handled comments carry our reaction.
	startupLookback = 24 * time.Hour

	// cleanupInterval is how often we prune GIFs past the TTL.
	cleanupInterval = 12 * time.Hour

	sinceKey = "monitor.since"
)

// SpinJob is the payload enqueued for the runner.
type SpinJob struct {
	Owner     string `json:"owner"`
	Repo      string `json:"repo"`
	PR        int    `json:"pr"`
	CommentID int64  `json:"comment_id"`
	Actor     string `json:"actor"` // github:<login> of the triggering comment
}

type Monitor struct {
	cfg      *config.Config
	gh       *github.Client // monitored repo: comments + reactions
	ghAssets *github.Client // repo the GIFs are committed to (may be the same)
	st       *store.Store
	tel      *telemetry.T
	self     string         // our own login, to recognise our own reaction during dedup
	seen     map[int64]bool // per-session fast path so we don't re-query reactions every poll
	since    time.Time
}

func New(cfg *config.Config, st *store.Store, tel *telemetry.T) *Monitor {
	gh := github.New(cfg.Token, cfg.Owner, cfg.Repo)
	ghAssets := gh
	if cfg.AssetsOwner != cfg.Owner || cfg.AssetsRepo != cfg.Repo {
		ghAssets = github.New(cfg.Token, cfg.AssetsOwner, cfg.AssetsRepo)
	}
	return &Monitor{cfg: cfg, gh: gh, ghAssets: ghAssets, st: st, tel: tel, seen: map[int64]bool{}}
}

// Run polls until ctx is cancelled.
func (m *Monitor) Run(ctx context.Context) error {
	if login, err := m.gh.AuthUser(); err != nil {
		log.Printf("warning: could not resolve authenticated user (%v); reaction dedup will match any user's %q", err, m.cfg.Reaction)
	} else {
		m.self = login
		log.Printf("authenticated as %q", login)
	}
	log.Printf("watching %s for %q comments every %s", m.cfg.RepoSlug(), m.cfg.Trigger, m.cfg.PollInterval)

	// Resume from the stored watermark; fall back to the bounded lookback.
	if ts, err := m.st.GetKVTime(ctx, sinceKey); err == nil && !ts.IsZero() {
		m.since = ts
	} else {
		m.since = time.Now().UTC().Add(-startupLookback)
	}

	poll := time.NewTicker(m.cfg.PollInterval)
	defer poll.Stop()
	clean := time.NewTicker(cleanupInterval)
	defer clean.Stop()

	m.poll(ctx)
	m.cleanup()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-poll.C:
			m.poll(ctx)
		case <-clean.C:
			m.cleanup()
		}
	}
}

// MatchesTrigger reports whether the body invokes the trigger as a command:
// the trigger must be the first whitespace-delimited token of some line.
//
// This must NOT be a substring match. Our own GIF comment embeds a raw URL on
// the "casino-review-assets" branch — a substring check finds "/casino-review"
// inside that URL and the bot re-triggers on its own comment, looping forever.
// The trigger-safety test runs every bot comment template through this.
func MatchesTrigger(body, trigger string) bool {
	for _, line := range strings.Split(body, "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == trigger {
			return true
		}
	}
	return false
}

func (m *Monitor) poll(ctx context.Context) {
	comments, err := m.gh.ListComments(m.since)
	if err != nil {
		log.Printf("list comments: %v", err)
		return
	}
	telemetry.PollLag.WithLabelValues("monitor").Set(time.Since(m.since).Seconds())

	// The watermark may only advance past comments that were handled (or
	// skipped on purpose). If handling a comment fails transiently, the
	// watermark is clamped to it so the next ListComments (inclusive `since`)
	// re-lists it — otherwise "retry next poll" is a lie whenever a newer
	// comment arrived in the same batch.
	newSince := m.since
	var retryFloor time.Time
	failed := func(t time.Time) {
		if retryFloor.IsZero() || t.Before(retryFloor) {
			retryFloor = t
		}
	}

	for _, c := range comments {
		if c.UpdatedAt.After(newSince) {
			newSince = c.UpdatedAt
		}
		if m.seen[c.ID] {
			continue
		}
		if !MatchesTrigger(c.Body, m.cfg.Trigger) {
			continue
		}
		// Durable dedup: a handled trigger comment carries our reaction. On a
		// transient error, retry next poll rather than risk a double-spin.
		reacted, err := m.gh.HasReaction(c.ID, m.self, m.cfg.Reaction)
		if err != nil {
			log.Printf("check reaction on comment %d: %v (will retry)", c.ID, err)
			failed(c.UpdatedAt)
			continue
		}
		m.seen[c.ID] = true
		if reacted {
			continue
		}
		if err := m.accept(ctx, c); err != nil {
			log.Printf("accept comment %d: %v (will retry)", c.ID, err)
			delete(m.seen, c.ID)
			failed(c.UpdatedAt)
		}
	}
	if !retryFloor.IsZero() && retryFloor.Before(newSince) {
		newSince = retryFloor
	}
	m.since = newSince
	if err := m.st.SetKVTime(ctx, sinceKey, m.since); err != nil {
		log.Printf("persist watermark: %v", err)
	}
	// The seen map is a fast path, not the source of truth (reactions + job
	// dedup keys are); cap it so a long-lived core doesn't grow unbounded.
	if len(m.seen) > 20000 {
		m.seen = map[int64]bool{}
	}
}

// accept validates the trigger, enqueues the spin job, then reacts.
//
// Ordering matters: enqueue FIRST. The job's unique dedup key makes enqueueing
// idempotent, so a reaction failure after a successful enqueue self-heals (next
// poll re-accepts, hits ErrDuplicateJob, retries the reaction). The reverse
// order has an unrecoverable hole: reaction persisted + enqueue failed = the
// comment looks handled forever and no spin ever runs.
func (m *Monitor) accept(ctx context.Context, c github.Comment) error {
	pr, ok := c.IssueNumber()
	if !ok {
		return fmt.Errorf("could not parse issue number from %q", c.IssueURL)
	}
	if isPR, err := m.gh.IsPullRequest(pr); err != nil {
		log.Printf("could not confirm #%d is a PR (%v); proceeding anyway", pr, err)
	} else if !isPR {
		log.Printf("#%d is an issue, not a PR; skipping", pr)
		return nil
	}

	actor := "github:" + c.User.Login
	job := SpinJob{Owner: m.cfg.Owner, Repo: m.cfg.Repo, PR: pr, CommentID: c.ID, Actor: actor}
	_, err := m.st.EnqueueJob(ctx, "spin", "spin:"+strconv.FormatInt(c.ID, 10), job)
	if err != nil && !errors.Is(err, store.ErrDuplicateJob) {
		return fmt.Errorf("enqueue: %w", err)
	}
	alreadyQueued := errors.Is(err, store.ErrDuplicateJob)

	if err := m.gh.AddReaction(c.ID, m.cfg.Reaction); err != nil {
		return fmt.Errorf("add reaction: %w", err)
	}
	if alreadyQueued {
		return nil // healed a previously-failed reaction; job already recorded
	}

	ctxRef := fmt.Sprintf("pr:%s#%d", m.cfg.RepoSlug(), pr)
	if err := telemetry.Emit(ctx, m.st.Pool, telemetry.Event{
		Type: "trigger.received", Actor: actor, ContextRef: ctxRef,
		Payload: map[string]any{"comment_id": c.ID},
	}); err != nil {
		log.Printf("emit trigger.received: %v", err)
	}
	m.tel.Track(actor, "spin_triggered", map[string]any{"pr": pr, "repo": m.cfg.RepoSlug()})
	log.Printf("PR #%d: accepted trigger from %s", pr, c.User.Login)
	return nil
}

// cleanup deletes committed GIFs older than the configured TTL so the assets
// branch doesn't grow without bound. Age comes from the filename timestamp.
func (m *Monitor) cleanup() {
	entries, err := m.ghAssets.ListDir(m.cfg.AssetsBranch, spin.AssetDir)
	if err != nil {
		log.Printf("cleanup: list %s: %v", spin.AssetDir, err)
		return
	}
	cutoff := time.Now().UTC().Add(-m.cfg.AssetsTTL)
	pruned := 0
	for _, e := range entries {
		if e.Type != "file" {
			continue
		}
		ts, ok := stampOf(e.Name)
		if !ok || !ts.Before(cutoff) {
			continue
		}
		msg := fmt.Sprintf("casino-review: prune GIF older than %s", m.cfg.AssetsTTL)
		if err := m.ghAssets.DeleteFile(m.cfg.AssetsBranch, spin.AssetDir+"/"+e.Name, e.SHA, msg); err != nil {
			log.Printf("cleanup: delete %s: %v", e.Name, err)
			continue
		}
		pruned++
	}
	if pruned > 0 {
		log.Printf("cleanup: pruned %d GIF(s) older than %s", pruned, m.cfg.AssetsTTL)
	}
}

// CleanupOnce runs a single prune pass — the `casino cleanup` subcommand.
func (m *Monitor) CleanupOnce() {
	log.Printf("cleanup: pruning GIFs older than %s in %s/%s (%s)",
		m.cfg.AssetsTTL, m.cfg.AssetsOwner, m.cfg.AssetsRepo, m.cfg.AssetsBranch)
	m.cleanup()
	log.Printf("cleanup: done")
}

// stampOf reads the leading unix-seconds timestamp from "<unix>-<pr>-<id>.gif".
func stampOf(name string) (time.Time, bool) {
	i := strings.IndexByte(name, '-')
	if i <= 0 {
		return time.Time{}, false
	}
	sec, err := strconv.ParseInt(name[:i], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(sec, 0).UTC(), true
}
