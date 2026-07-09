// casino is the admin / dry-run CLI:
//
//	casino gen [out.gif]                          render a sample GIF locally (no GitHub)
//	casino check                                  read-only GitHub smoke test
//	casino cleanup                                one prune pass of old GIF assets
//	casino db migrate                             apply schema migrations
//	casino review run <engine> --pr N [--post]    run one engine against a PR
//	casino oracle once [--apply]                  settle resolvable markets (dry-run unless --apply)
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"casino-review/internal/config"
	"casino-review/internal/github"
	"casino-review/internal/monitor"
	"casino-review/internal/review"
	"casino-review/internal/slots"
	"casino-review/internal/store"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[casino] ")

	if len(os.Args) < 2 {
		usage()
	}

	// gen needs no config/GitHub at all.
	if os.Args[1] == "gen" {
		genSample(os.Args[2:])
		return
	}
	// db migrate needs only DATABASE_URL — don't demand GitHub credentials.
	if os.Args[1] == "db" {
		if len(os.Args) < 3 || os.Args[2] != "migrate" {
			usage()
		}
		runMigrate(os.Getenv("DATABASE_URL"))
		return
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	switch os.Args[1] {
	case "check":
		runCheck(cfg)
	case "cleanup":
		// Monitor's cleanup path only touches GitHub, not the store.
		monitor.New(cfg, nil, nil).CleanupOnce()
	case "review":
		if len(os.Args) < 3 || os.Args[2] != "run" {
			usage()
		}
		runReview(cfg, os.Args[3:])
	case "prs":
		runPRs(cfg, os.Args[2:])
	case "market":
		runMarket(cfg, os.Args[2:])
	case "oracle":
		runOracle(cfg, os.Args[2:])
	default:
		usage()
	}
}

// runPRs lists the PRs casino-review has acted on (where /casino-review ran).
func runPRs(cfg *config.Config, args []string) {
	if cfg.DatabaseURL == "" {
		log.Fatalf("DATABASE_URL is required")
	}
	fs := flag.NewFlagSet("prs", flag.ExitOnError)
	limit := fs.Int("limit", 50, "max PRs to show")
	fs.Parse(args)

	ctx := context.Background()
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	prs, err := st.TrackedPRs(ctx, cfg.RepoSlug(), *limit)
	if err != nil {
		log.Fatalf("tracked prs: %v", err)
	}
	pending, _ := st.PendingSpins(ctx)

	if len(prs) == 0 {
		fmt.Printf("no PRs tracked yet for %s (post /%s on a PR to start)\n", cfg.RepoSlug(), strings.TrimPrefix(cfg.Trigger, "/"))
	} else {
		fmt.Printf("tracked PRs for %s (%d shown):\n", cfg.RepoSlug(), len(prs))
		fmt.Printf("  %-6s %-5s %-16s %-9s %-20s %s\n", "PR", "spins", "last-engine", "findings", "last-run (UTC)", "status")
		for _, p := range prs {
			findings := "?"
			if p.LastFindings != nil {
				findings = strconv.Itoa(*p.LastFindings)
			}
			status := "ok"
			if p.LastError != "" {
				status = "ERROR: " + firstLine(p.LastError)
			}
			engine := p.LastEngine
			if p.LastKind == "addon" {
				engine += " (bonus)"
			}
			fmt.Printf("  #%-5d %-5d %-16s %-9s %-20s %s\n",
				p.PR, p.Runs, engine, findings, p.LastAt.UTC().Format("2006-01-02 15:04"), status)
		}
	}
	if pending > 0 {
		fmt.Printf("\n%d spin(s) in flight (triggered, review not posted yet)\n", pending)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 60 {
		s = s[:60] + "…"
	}
	return s
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  casino gen [out.gif] [--bonus <label>]
  casino check
  casino cleanup
  casino db migrate
  casino review run <engine> --pr N [--post]
  casino prs [--limit N]                              # PRs /casino-review has acted on
  casino market fund <ctx> <amount> [--as id]
  casino market create <ctx> <kind> [deadline] [--as id]
  casino market bet <id> <outcome> <amount> [--as id]
  casino market board | show <id> | me [--as id]
  casino market refund <id> | lock <id> | void <id> [reason]
  casino market resolve <id> <outcome> [--solver login] [--as id]
  casino oracle once [--apply]                        # settle resolvable markets (dry-run unless --apply)`)
	os.Exit(2)
}

func runMigrate(databaseURL string) {
	if databaseURL == "" {
		log.Fatalf("DATABASE_URL is required")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, databaseURL)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	log.Printf("migrations applied")
}

// runReview executes a single engine against a PR. Dry-run by default: results
// print to stdout; --post also posts the PR comment (and dispatch trigger).
func runReview(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("review run", flag.ExitOnError)
	pr := fs.Int("pr", 0, "pull request number")
	post := fs.Bool("post", false, "post results to the PR (default: dry-run)")
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		log.Fatalf("engine name required: casino review run <engine> --pr N")
	}
	engineName := args[0]
	fs.Parse(args[1:])
	if *pr <= 0 {
		log.Fatalf("--pr is required")
	}

	registry, err := review.LoadRegistry(cfg.ReviewsFile, cfg.Reviews, cfg.Trigger)
	if err != nil {
		log.Fatalf("registry: %v", err)
	}
	gh := github.New(cfg.Token, cfg.Owner, cfg.Repo)
	deps := review.Deps{
		GH: gh, Token: cfg.Token,
		Checkouts: review.NewCheckouts(cfg.Workdir, cfg.Token),
		ClaudeBin: cfg.ClaudeBin,
		DryRun:    !*post,
	}

	var engine review.Engine
	for i := range registry.Reviews {
		if registry.Reviews[i].Name == engineName {
			engine, err = review.Build(registry.Reviews[i], deps)
			if err != nil {
				log.Fatalf("build engine: %v", err)
			}
		}
	}
	if engine == nil && registry.Addon != nil && registry.Addon.Name == engineName {
		addon, err := review.BuildAddon(registry.Addon, deps)
		if err != nil {
			log.Fatalf("build addon: %v", err)
		}
		engine = addon.Engine
	}
	if engine == nil {
		have := strings.Join(registry.Names(), ", ")
		if registry.Addon != nil {
			have += ", " + registry.Addon.Name + " (addon)"
		}
		log.Fatalf("engine %q not in registry (have: %s)", engineName, have)
	}

	log.Printf("running %s (%s) on %s#%d (post=%v)", engine.Name(), engine.Kind(), cfg.RepoSlug(), *pr, *post)
	start := time.Now()
	res, err := engine.Run(context.Background(), review.PR{Owner: cfg.Owner, Repo: cfg.Repo, Number: *pr})
	if err != nil {
		log.Fatalf("run: %v", err)
	}
	log.Printf("done in %s — %d finding(s), cost $%.4f", time.Since(start).Round(time.Second), len(res.Findings), res.CostUSD)
	if res.Summary != "" {
		fmt.Printf("\nSummary: %s\n", res.Summary)
	}
	for i, f := range res.Findings {
		fmt.Printf("%2d. [%s] %s:%d — %s\n", i+1, f.Severity, f.Path, f.Line, f.Title)
		if f.Body != "" {
			fmt.Printf("      %s\n", strings.ReplaceAll(f.Body, "\n", "\n      "))
		}
	}
}

func genSample(args []string) {
	out := "casino-sample.gif"
	bonus := ""
	rest := args[:0:0]
	for i := 0; i < len(args); i++ {
		if args[i] == "--bonus" && i+1 < len(args) {
			bonus = args[i+1]
			i++
			continue
		}
		rest = append(rest, args[i])
	}
	if len(rest) > 0 {
		out = rest[0]
	}
	reviews := []string{"tsetso-review", "dimoreview", "gigareview", "barbie-review"}
	if env := os.Getenv("REVIEWS"); env != "" {
		reviews = nil
		for _, r := range strings.Split(env, ",") {
			if r = strings.TrimPrefix(strings.TrimSpace(r), "/"); r != "" {
				reviews = append(reviews, r)
			}
		}
	}
	if len(reviews) == 0 {
		log.Fatalf("REVIEWS is set but contains no usable names")
	}
	idx := int(time.Now().UnixNano()) % len(reviews)
	if idx < 0 {
		idx += len(reviews)
	}
	var opts []slots.Option
	if bonus != "" {
		opts = append(opts, slots.WithBonus(bonus))
	}
	data, err := slots.Generate(reviews, idx, time.Now().UnixNano(), opts...)
	if err != nil {
		log.Fatalf("generate: %v", err)
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		log.Fatalf("write: %v", err)
	}
	log.Printf("wrote %s (%d bytes), winner=%q bonus=%q", out, len(data), reviews[idx], bonus)
}

// runCheck is a read-only dry test of the monitor's read path.
func runCheck(cfg *config.Config) {
	gh := github.New(cfg.Token, cfg.Owner, cfg.Repo)

	login, err := gh.AuthUser()
	if err != nil {
		log.Fatalf("FAIL: token cannot authenticate: %v", err)
	}
	log.Printf("authenticated as %q on %s", login, cfg.RepoSlug())

	since := time.Now().Add(-90 * 24 * time.Hour)
	comments, err := gh.ListComments(since)
	if err != nil {
		log.Fatalf("FAIL: cannot read comments: %v", err)
	}
	log.Printf("OK: read %d issue/PR comments updated since %s", len(comments), since.Format("2006-01-02"))
	if len(comments) == 0 {
		log.Printf("(none in the window — open a PR and comment, then re-run)")
		return
	}

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
