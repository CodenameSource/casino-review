// Command casino-review watches a repo's PRs for a trigger comment, spins a
// slot-machine GIF to pick a review, posts it, then fires the real review.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"casino-review/internal/config"
	"casino-review/internal/github"
	"casino-review/internal/monitor"
	"casino-review/internal/slots"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[casino] ")

	// `casino-review gen out.gif` renders a sample GIF locally — no GitHub needed.
	if len(os.Args) >= 2 && os.Args[1] == "gen" {
		genSample(os.Args[2:])
		return
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// `casino-review check` is a read-only dry test: confirm the token can read
	// the repo's PR comments. It posts/reacts/deletes nothing.
	if len(os.Args) >= 2 && os.Args[1] == "check" {
		runCheck(cfg)
		return
	}

	// `casino-review cleanup` runs one prune pass of old GIFs and exits. Set
	// ASSETS_TTL=0s to purge them all (e.g. to clear test spins).
	if len(os.Args) >= 2 && os.Args[1] == "cleanup" {
		monitor.New(cfg).CleanupOnce()
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := monitor.New(cfg).Run(ctx); err != nil && err != context.Canceled {
		log.Fatalf("monitor: %v", err)
	}
}

// runCheck is a read-only dry test of the monitor's read path: authenticate, then
// list the repo's issue/PR comments and report what it found. Nothing is posted.
func runCheck(cfg *config.Config) {
	gh := github.New(cfg.Token, cfg.Owner, cfg.Repo)

	login, err := gh.AuthUser()
	if err != nil {
		log.Fatalf("FAIL: token cannot authenticate: %v", err)
	}
	log.Printf("authenticated as %q on %s/%s", login, cfg.Owner, cfg.Repo)

	since := time.Now().Add(-90 * 24 * time.Hour)
	comments, err := gh.ListComments(since)
	if err != nil {
		log.Fatalf("FAIL: cannot read comments: %v", err)
	}
	log.Printf("OK: read %d issue/PR comments updated since %s", len(comments), since.Format("2006-01-02"))
	if len(comments) == 0 {
		log.Printf("(none in the window — open a PR and comment, then re-run to see them)")
		return
	}

	// Sample which comments are on PRs (capped to keep the call count low) and how
	// many already carry the trigger.
	isPR := map[int]bool{}
	checked, prComments, triggers := 0, 0, 0
	for _, c := range comments {
		n, ok := c.IssueNumber()
		if !ok {
			continue
		}
		pr, known := isPR[n]
		if !known && checked < 40 {
			pr, _ = gh.IsPullRequest(n)
			isPR[n] = pr
			checked++
		}
		if pr {
			prComments++
		}
		if monitor.MatchesTrigger(c.Body, cfg.Trigger) {
			triggers++
		}
	}
	log.Printf("OK: %d are on PRs (sampled %d distinct issues/PRs); %d contain %q",
		prComments, checked, triggers, cfg.Trigger)

	log.Printf("most recent comments:")
	for i := len(comments) - 1; i >= 0 && i >= len(comments)-5; i-- {
		c := comments[i]
		num, _ := c.IssueNumber()
		body := strings.ReplaceAll(strings.TrimSpace(c.Body), "\n", " ")
		if len(body) > 70 {
			body = body[:70] + "…"
		}
		log.Printf("  #%d by %s: %q", num, c.User.Login, body)
	}
}

func genSample(args []string) {
	out := "casino-sample.gif"
	if len(args) > 0 {
		out = args[0]
	}
	reviews := []string{"tsetso-review", "dimoreview", "gigareview"}
	if env := os.Getenv("REVIEWS"); env != "" {
		reviews = nil
		for _, r := range strings.Split(env, ",") {
			if r = strings.TrimPrefix(strings.TrimSpace(r), "/"); r != "" {
				reviews = append(reviews, r)
			}
		}
	}
	// Pick a winner from the clock so repeated runs vary.
	idx := int(time.Now().UnixNano()) % len(reviews)
	if idx < 0 {
		idx += len(reviews)
	}
	data, err := slots.Generate(reviews, idx, time.Now().UnixNano())
	if err != nil {
		log.Fatalf("generate: %v", err)
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		log.Fatalf("write: %v", err)
	}
	log.Printf("wrote %s (%d bytes), winner=%q", out, len(data), reviews[idx])
}
