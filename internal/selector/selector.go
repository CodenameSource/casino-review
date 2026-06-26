// Package selector decides which review wins a spin.
//
// Milestone 1 is pure chance. Milestone 2 will weight or gate choices on signals
// such as "did the previously chosen review actually post any findings?" — hence
// the Selector interface and the Context carrying prior state.
package selector

import "math/rand"

// Context carries the signals a selector may reason over.
type Context struct {
	Reviews       []string // candidate review names (no leading slash)
	PullRequest   int      // PR the spin was triggered on
	PreviousIndex int      // index of the last chosen review (-1 if none)
	// Milestone 2: add e.g. PreviousHadFindings bool, perReviewCooldowns, etc.
}

// Selector picks an index into ctx.Reviews.
type Selector interface {
	Choose(ctx Context, r *rand.Rand) int
}

// Random picks uniformly at random.
type Random struct{}

func (Random) Choose(ctx Context, r *rand.Rand) int {
	return r.Intn(len(ctx.Reviews))
}
