// Package worker is the runner's job loop: claim a spin job, select the
// winning engine, post the GIF, wait out the display window, execute the
// engine, and record the run — the assignment AND the outcome — for the
// experiment record.
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"time"

	"casino-review/internal/config"
	"casino-review/internal/github"
	"casino-review/internal/monitor"
	"casino-review/internal/review"
	"casino-review/internal/selector"
	"casino-review/internal/spin"
	"casino-review/internal/store"
	"casino-review/internal/telemetry"
	"casino-review/internal/templates"
)

const (
	claimInterval  = 2 * time.Second
	stuckJobMaxAge = 30 * time.Minute
)

type Worker struct {
	cfg     *config.Config
	st      *store.Store
	tel     *telemetry.T
	gh      *github.Client // monitored repo (error comments)
	spinner *spin.Spinner
	engines map[string]review.Engine
	names   []string // registry order = slot entries
	sel     selector.Selector
	addon   *review.Addon // bonus reviewer; nil = no addon configured
}

func New(cfg *config.Config, st *store.Store, tel *telemetry.T, gh *github.Client, spinner *spin.Spinner,
	engines map[string]review.Engine, names []string, sel selector.Selector, addon *review.Addon) *Worker {
	return &Worker{cfg: cfg, st: st, tel: tel, gh: gh, spinner: spinner, engines: engines, names: names, sel: sel, addon: addon}
}

// Run claims and executes jobs until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	log.Printf("runner: %d engines loaded (%v), selector %s", len(w.names), w.names, w.sel.Name())
	tick := time.NewTicker(claimInterval)
	defer tick.Stop()
	requeue := time.NewTicker(time.Minute)
	defer requeue.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-requeue.C:
			if n, err := w.st.RequeueStuckJobs(ctx, stuckJobMaxAge); err != nil {
				log.Printf("requeue stuck jobs: %v", err)
			} else if n > 0 {
				log.Printf("requeued %d stuck job(s)", n)
			}
		case <-tick.C:
			for { // drain the queue before sleeping again
				job, err := w.st.ClaimJob(ctx, []string{"spin"})
				if err != nil {
					log.Printf("claim job: %v", err)
					break
				}
				if job == nil {
					break
				}
				w.execute(ctx, job)
			}
		}
	}
}

func (w *Worker) execute(ctx context.Context, job *store.Job) {
	started := time.Now()
	err := w.spinAndReview(ctx, job)
	outcome := "done"
	if err != nil {
		outcome = "error"
		log.Printf("job %d failed: %v", job.ID, err)
	}
	telemetry.JobsProcessed.WithLabelValues(job.Kind, outcome).Inc()
	telemetry.JobDuration.WithLabelValues(job.Kind).Observe(time.Since(started).Seconds())
	// Terminal record writes must survive a SIGTERM that cancelled ctx mid-job —
	// otherwise shutdowns silently lose outcomes and strand jobs in 'running'.
	bg := context.WithoutCancel(ctx)
	if err != nil && ctx.Err() != nil {
		// Shutdown, not failure: hand the job back for the next runner.
		if rerr := w.st.RequeueJob(bg, job.ID); rerr != nil {
			log.Printf("requeue job %d on shutdown: %v", job.ID, rerr)
		}
		return
	}
	if ferr := w.st.FinishJob(bg, job.ID, err); ferr != nil {
		log.Printf("finish job %d: %v", job.ID, ferr)
	}
}

func (w *Worker) spinAndReview(ctx context.Context, job *store.Job) error {
	var sj monitor.SpinJob
	if err := json.Unmarshal(job.Payload, &sj); err != nil {
		return fmt.Errorf("bad payload: %w", err)
	}
	repoSlug := sj.Owner + "/" + sj.Repo
	ctxRef := fmt.Sprintf("pr:%s#%d", repoSlug, sj.PR)

	// Assignment: build the selector context (previous run's findings feed
	// milestone-2 weighting) and pick the winner.
	selCtx := selector.Context{Reviews: w.names, PullRequest: sj.PR, PreviousIndex: -1}
	if last, err := w.st.LastReviewRun(ctx, repoSlug); err != nil {
		log.Printf("last review run: %v", err)
	} else if last != nil {
		for i, n := range w.names {
			if n == last.Engine {
				selCtx.PreviousIndex = i
			}
		}
		if last.FindingsCount != nil {
			had := *last.FindingsCount > 0
			selCtx.PreviousHadFindings = &had
		}
	}
	r := rand.New(rand.NewSource(spin.RandSeed()))
	chosen := w.sel.Choose(selCtx, r)
	engine := w.engines[w.names[chosen]]

	// The bonus (addon) roll is a Bernoulli assignment: rolled BEFORE the GIF
	// renders (so the animation matches reality) and logged with its chance
	// (the propensity — without it the bonus arm isn't analyzable).
	bonusLabel := ""
	var bonusChance float64
	bonusHit := false
	if w.addon != nil {
		bonusChance = w.addon.Chance
		bonusHit = r.Float64() < w.addon.Chance
		if bonusHit {
			bonusLabel = w.addon.Engine.Name()
		}
	}

	// Record the assignment BEFORE the outcome exists — RCT hygiene.
	if err := telemetry.Emit(ctx, w.st.Pool, telemetry.Event{
		Type: "spin.assigned", Actor: sj.Actor, ContextRef: ctxRef,
		Payload: map[string]any{
			"job_id": job.ID, "pool": w.names, "chosen": engine.Name(),
			"chosen_index": chosen, "selector": w.sel.Name(),
			"prev_index": selCtx.PreviousIndex, "prev_had_findings": selCtx.PreviousHadFindings,
			"addon": addonName(w.addon), "addon_chance": bonusChance, "addon_hit": bonusHit,
		},
	}); err != nil {
		log.Printf("emit spin.assigned: %v", err)
	}

	gifURL, _, err := w.spinner.Spin(sj.PR, w.names, chosen, job.ID, bonusLabel)
	if err != nil {
		return err
	}
	log.Printf("PR #%d: reel landed on %q (bonus=%v, gif %s)", sj.PR, engine.Name(), bonusHit, gifURL)

	// Let the spin play out before the review reveals the winner's work.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(w.cfg.DisplayFor):
	}

	runErr := w.runEngine(ctx, job, sj, ctxRef, engine)

	// The bonus round: the addon runs regardless of the main engine's outcome
	// (the GIF promised it), but never on shutdown — the requeued retry
	// replays the whole job. The job's success is the MAIN engine's success;
	// an addon failure is recorded but doesn't fail or retry the job.
	if bonusHit && ctx.Err() == nil {
		if aerr := w.runEngine(ctx, job, sj, ctxRef, w.addon.Engine); aerr != nil {
			log.Printf("PR #%d: bonus addon failed: %v", sj.PR, aerr)
		}
	}
	return runErr
}

// runEngine executes one engine and records the outcome — the review_runs row,
// prometheus, the events spine, PostHog, and a PR error comment on failure.
func (w *Worker) runEngine(ctx context.Context, job *store.Job, sj monitor.SpinJob, ctxRef string, engine review.Engine) error {
	runStart := time.Now()
	res, runErr := engine.Run(ctx, review.PR{Owner: sj.Owner, Repo: sj.Repo, Number: sj.PR})

	// Record writes below must survive a SIGTERM mid-run: a cancelled ctx would
	// silently lose the outcome (and the RCT observation) at the finish line.
	bg := context.WithoutCancel(ctx)

	run := store.ReviewRun{
		Repo: sj.Owner + "/" + sj.Repo, PR: sj.PR, Engine: engine.Name(), EngineKind: engine.Kind(),
		JobID: job.ID, Summary: res.Summary, CommentID: res.CommentID,
		StartedAt: runStart, FinishedAt: time.Now(),
	}
	if res.KnownFindings {
		n := len(res.Findings)
		run.FindingsCount = &n
		run.Findings, _ = json.Marshal(res.Findings)
	}
	outcome := "ok"
	if runErr != nil {
		run.Error = runErr.Error()
		outcome = "error"
	}
	if _, err := w.st.InsertReviewRun(bg, run); err != nil {
		log.Printf("insert review run: %v", err)
	}

	telemetry.ReviewRuns.WithLabelValues(engine.Name(), outcome).Inc()
	if res.KnownFindings && runErr == nil {
		telemetry.ReviewFindings.WithLabelValues(engine.Name()).Observe(float64(len(res.Findings)))
	}
	evType := "review.completed"
	if runErr != nil {
		evType = "review.failed"
	}
	if err := telemetry.Emit(bg, w.st.Pool, telemetry.Event{
		Type: evType, Actor: sj.Actor, ContextRef: ctxRef,
		Payload: map[string]any{
			"job_id": job.ID, "engine": engine.Name(), "kind": engine.Kind(),
			"findings": run.FindingsCount, "cost_usd": res.CostUSD,
			"duration_ms": time.Since(runStart).Milliseconds(),
		},
	}); err != nil {
		log.Printf("emit %s: %v", evType, err)
	}
	w.tel.Track(sj.Actor, evType, map[string]any{
		"engine": engine.Name(), "engine_kind": engine.Kind(), "pr": sj.PR,
		"findings": run.FindingsCount, "cost_usd": res.CostUSD,
		"duration_s": time.Since(runStart).Seconds(),
	})

	if runErr != nil {
		log.Printf("PR #%d: engine %s failed: %v", sj.PR, engine.Name(), runErr)
		// The spin promised a review; tell the PR the reel jammed rather than
		// leaving a GIF with no payoff. Skipped on shutdown-caused failures —
		// the requeued retry will deliver the real result.
		if ctx.Err() == nil {
			if _, cerr := w.gh.CreateComment(sj.PR, templates.ReviewError(engine.Name(), runErr)); cerr != nil {
				log.Printf("post error comment: %v", cerr)
			}
		}
		return runErr
	}
	log.Printf("PR #%d: %s completed (%d findings)", sj.PR, engine.Name(), len(res.Findings))
	return nil
}

func addonName(a *review.Addon) string {
	if a == nil {
		return ""
	}
	return a.Engine.Name()
}
