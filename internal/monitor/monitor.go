// Package monitor polls a repo's PR comments and runs the casino spin.
package monitor

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"fmt"
	"log"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"casino-review/internal/config"
	"casino-review/internal/github"
	"casino-review/internal/selector"
	"casino-review/internal/slots"
)

const (
	// startupLookback is how far back the first poll looks. Because dedup is the
	// reaction on each comment (durable, on GitHub — no local state), looking back
	// is safe: already-handled comments carry our reaction and are skipped. This
	// lets a restart pick up triggers posted while we were down.
	startupLookback = 24 * time.Hour

	// cleanupInterval is how often we prune GIFs past the TTL.
	cleanupInterval = 12 * time.Hour

	// assetDir is the folder (on the assets branch) the GIFs live in.
	assetDir = "casino"
)

// randSeed returns a non-reproducible seed from the OS CSPRNG, falling back to
// the wall clock if that ever fails.
func randSeed() int64 {
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		return time.Now().UnixNano()
	}
	return int64(binary.LittleEndian.Uint64(b[:]))
}

type Monitor struct {
	cfg      *config.Config
	gh       *github.Client // monitored repo: comments + reactions
	ghAssets *github.Client // repo the GIF is committed to (may be the same)
	sel      selector.Selector
	self     string         // our own login, to recognise our own reaction during dedup
	seen     map[int64]bool // per-session fast path so we don't re-query reactions every poll
	since    time.Time      // only consider comments updated at/after this time
	prevPick int            // last chosen review index (for milestone-2 selectors)
}

func New(cfg *config.Config) *Monitor {
	gh := github.New(cfg.Token, cfg.Owner, cfg.Repo)
	ghAssets := gh
	if cfg.AssetsOwner != cfg.Owner || cfg.AssetsRepo != cfg.Repo {
		ghAssets = github.New(cfg.Token, cfg.AssetsOwner, cfg.AssetsRepo)
	}
	return &Monitor{
		cfg:      cfg,
		gh:       gh,
		ghAssets: ghAssets,
		sel:      selector.Random{},
		seen:     map[int64]bool{},
		since:    time.Now().UTC().Add(-startupLookback),
		prevPick: -1,
	}
}

// Run polls until ctx is cancelled.
func (m *Monitor) Run(ctx context.Context) error {
	if login, err := m.gh.AuthUser(); err != nil {
		log.Printf("warning: could not resolve authenticated user (%v); reaction dedup will match any user's %q", err, m.cfg.Reaction)
	} else {
		m.self = login
		log.Printf("authenticated as %q", login)
	}
	log.Printf("watching %s/%s for %q comments every %s", m.cfg.Owner, m.cfg.Repo, m.cfg.Trigger, m.cfg.PollInterval)
	if m.ghAssets == m.gh {
		log.Printf("hosting GIFs in %s/%s — if that repo is private the embed URL expires (~5 min); set ASSETS_REPO to a public repo to keep them", m.cfg.Owner, m.cfg.Repo)
	} else {
		log.Printf("hosting GIFs in %s/%s", m.cfg.AssetsOwner, m.cfg.AssetsRepo)
	}

	poll := time.NewTicker(m.cfg.PollInterval)
	defer poll.Stop()
	clean := time.NewTicker(cleanupInterval)
	defer clean.Stop()

	m.poll(ctx) // run once immediately
	m.cleanup() // prune anything already past its TTL
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

// CleanupOnce runs a single prune pass and logs a summary — used by the
// `cleanup` subcommand to test the prune or purge on demand (set ASSETS_TTL=0s
// to delete everything now).
func (m *Monitor) CleanupOnce() {
	log.Printf("cleanup: pruning GIFs older than %s in %s/%s (%s)",
		m.cfg.AssetsTTL, m.cfg.AssetsOwner, m.cfg.AssetsRepo, m.cfg.AssetsBranch)
	m.cleanup()
	log.Printf("cleanup: done")
}

// cleanup deletes committed GIFs older than the configured TTL so the assets
// branch doesn't grow without bound. Age comes from the filename timestamp.
func (m *Monitor) cleanup() {
	entries, err := m.ghAssets.ListDir(m.cfg.AssetsBranch, assetDir)
	if err != nil {
		log.Printf("cleanup: list %s: %v", assetDir, err)
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
		if err := m.ghAssets.DeleteFile(m.cfg.AssetsBranch, assetDir+"/"+e.Name, e.SHA, msg); err != nil {
			log.Printf("cleanup: delete %s: %v", e.Name, err)
			continue
		}
		pruned++
	}
	if pruned > 0 {
		log.Printf("cleanup: pruned %d GIF(s) older than %s", pruned, m.cfg.AssetsTTL)
	}
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

// MatchesTrigger reports whether the body invokes the trigger as a command: the
// trigger must be the first whitespace-delimited token of some line.
//
// This must NOT be a substring match. Our own GIF comment embeds a raw URL on the
// "casino-review-assets" branch — a substring check finds "/casino-review" inside
// that URL and the bot re-triggers on its own comment, looping forever. Requiring
// the trigger to lead a line also ignores the "/<winner>" comment we post.
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
	for _, c := range comments {
		// Advance the watermark past everything we've looked at.
		if c.UpdatedAt.After(m.since) {
			m.since = c.UpdatedAt
		}
		if m.seen[c.ID] {
			continue
		}
		// Match the trigger as a command (first token of a line), not a substring:
		// our own GIF comment's URL contains "casino-review-assets", which a
		// substring check would mistake for the trigger and loop on itself. We can
		// therefore safely act on our own account's comments too.
		if !MatchesTrigger(c.Body, m.cfg.Trigger) {
			continue
		}
		// Durable dedup: a trigger comment we've already handled carries our
		// reaction. Check GitHub rather than a local file. On a transient error,
		// skip this round and retry next poll (don't risk a double-spin).
		reacted, err := m.gh.HasReaction(c.ID, m.self, m.cfg.Reaction)
		if err != nil {
			log.Printf("check reaction on comment %d: %v (will retry)", c.ID, err)
			continue
		}
		m.seen[c.ID] = true // fast path: don't re-query this comment this session
		if reacted {
			continue
		}
		if err := m.handle(ctx, c); err != nil {
			log.Printf("handle comment %d: %v", c.ID, err)
		}
	}
}

func (m *Monitor) handle(ctx context.Context, c github.Comment) error {
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

	// Acknowledge immediately: react so the comment visibly shows it has started
	// processing (a human-visible companion to the persisted dedup state).
	if err := m.gh.AddReaction(c.ID, m.cfg.Reaction); err != nil {
		log.Printf("add reaction to comment %d: %v", c.ID, err)
	}

	// Non-deterministic seed: the comment ID is public and monotonic, so seeding
	// from it would let anyone reproduce the GIF and read the winner before the
	// reveal. The selector's choice and the GIF's decoys both draw from this.
	seed := randSeed()
	r := rand.New(rand.NewSource(seed))
	idx := m.sel.Choose(selector.Context{
		Reviews:       m.cfg.Reviews,
		PullRequest:   pr,
		PreviousIndex: m.prevPick,
	}, r)
	m.prevPick = idx
	winner := m.cfg.Reviews[idx]
	log.Printf("PR #%d: spinning the casino → %q", pr, winner)

	gifBytes, err := slots.Generate(m.cfg.Reviews, idx, randSeed())
	if err != nil {
		return fmt.Errorf("generate gif: %w", err)
	}

	// Stage the GIF on the assets repo/branch and embed it. When the assets repo is
	// public, the returned download_url has no expiring token, so the embed lasts.
	if err := m.ghAssets.EnsureBranch(m.cfg.AssetsBranch); err != nil {
		return fmt.Errorf("ensure assets branch: %w", err)
	}
	// Timestamp-prefixed so cleanup() can prune by age from the name alone.
	path := fmt.Sprintf("%s/%d-%d-%d.gif", assetDir, time.Now().UTC().Unix(), pr, c.ID)
	asset, err := m.ghAssets.PutFile(m.cfg.AssetsBranch, path, gifBytes, "casino-review: spin asset")
	if err != nil {
		return fmt.Errorf("upload gif: %w", err)
	}

	// The comment is just the GIF, and it stays.
	if _, err := m.gh.CreateComment(pr, fmt.Sprintf("![🎰](%s)", asset.DownloadURL)); err != nil {
		return fmt.Errorf("post spin comment: %w", err)
	}

	// Give the spin time to play out, then trigger the real review.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(m.cfg.DisplayFor):
	}

	if _, err := m.gh.CreateComment(pr, "/"+winner); err != nil {
		return fmt.Errorf("post review trigger: %w", err)
	}
	log.Printf("PR #%d: triggered /%s", pr, winner)
	return nil
}
