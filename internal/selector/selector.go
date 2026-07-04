// Package selector decides which review wins a spin.
//
// The slot machine is the experiment's randomizer: each Choose is a random
// assignment of an engine to a PR, so selectors must be honest about their
// distribution and the monitor logs the full assignment (pool, weights,
// chosen) to the events spine — outcomes are only interpretable if the
// assignment mechanism is recorded.
package selector

import "math/rand"

// ReviewStats summarizes an engine's history (from review_runs).
type ReviewStats struct {
	Runs         int
	WithFindings int
	LastError    bool
}

// Context carries the signals a selector may reason over.
type Context struct {
	Reviews       []string // candidate review names (no leading slash)
	PullRequest   int      // PR the spin was triggered on
	PreviousIndex int      // index of the last chosen review (-1 if none)

	// PreviousHadFindings reports whether the previously chosen review
	// produced findings; nil = unknown (e.g. a dispatch engine we can't
	// observe). Milestone-2 selectors weight on this.
	PreviousHadFindings *bool

	// Stats by review name, for longer-horizon weighting (P5).
	Stats map[string]ReviewStats
}

// Selector picks an index into ctx.Reviews.
type Selector interface {
	Choose(ctx Context, r *rand.Rand) int
	// Name identifies the assignment mechanism in the experiment log.
	Name() string
}

// Random picks uniformly at random — the baseline assignment mechanism.
type Random struct{}

func (Random) Choose(ctx Context, r *rand.Rand) int {
	return r.Intn(len(ctx.Reviews))
}

func (Random) Name() string { return "random/v1" }
