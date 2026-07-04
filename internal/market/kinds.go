// Package market sits on top of the ledger: it knows the market kinds (what
// outcomes they have, how they pay out, what their spec means), parses context
// references, and fans notifications out. Adding a kind = one entry here.
package market

import (
	"fmt"
	"time"
)

// Kind describes one market type.
type Kind struct {
	Name       string
	PayoutRule string // "solver" | "parimutuel"
	// Outcomes returns the outcome set for a new market of this kind.
	Outcomes func(spec map[string]any) []string
	// ValidateSpec normalizes/validates kind params at creation.
	ValidateSpec func(spec map[string]any) error
	// DefaultQuestion renders a human question for the board.
	DefaultQuestion func(contextRef string, spec map[string]any) string
}

// Kinds is the registry. bounty pays the PR author on merge; merge-by and
// findings-count are parimutuel prediction markets over the PR (resolution
// automation lands in P3 — until then `resolve` is an admin action).
var Kinds = map[string]Kind{
	"bounty": {
		Name:       "bounty",
		PayoutRule: "solver",
		Outcomes:   func(map[string]any) []string { return []string{"merged"} },
		ValidateSpec: func(spec map[string]any) error {
			return nil
		},
		DefaultQuestion: func(ctx string, _ map[string]any) string {
			return "Bounty: pool pays the author when " + ctx + " merges"
		},
	},
	"merge-by": {
		Name:       "merge-by",
		PayoutRule: "parimutuel",
		Outcomes:   func(map[string]any) []string { return []string{"yes", "no"} },
		ValidateSpec: func(spec map[string]any) error {
			d, ok := spec["deadline"].(string)
			if !ok || d == "" {
				return fmt.Errorf("merge-by requires a deadline (RFC3339 or a duration like 72h)")
			}
			if _, err := time.Parse(time.RFC3339, d); err != nil {
				return fmt.Errorf("deadline %q is not RFC3339 (parse durations before storing)", d)
			}
			return nil
		},
		DefaultQuestion: func(ctx string, spec map[string]any) string {
			return fmt.Sprintf("Will %s be merged by %v?", ctx, spec["deadline"])
		},
	},
	"findings-count": {
		Name:       "findings-count",
		PayoutRule: "parimutuel",
		Outcomes: func(spec map[string]any) []string {
			if raw, ok := spec["buckets"].([]any); ok && len(raw) > 0 {
				out := make([]string, 0, len(raw))
				for _, b := range raw {
					if s, ok := b.(string); ok {
						out = append(out, s)
					}
				}
				if len(out) > 0 {
					return out
				}
			}
			return []string{"0", "1-2", "3-5", "6+"}
		},
		ValidateSpec: func(spec map[string]any) error {
			return nil
		},
		DefaultQuestion: func(ctx string, _ map[string]any) string {
			return "How many findings will the casino review of " + ctx + " produce?"
		},
	},
}

// BucketFor maps a findings count onto the default buckets (P3's findings
// oracle uses this; defined with the kind so they can't drift apart).
func BucketFor(count int) string {
	switch {
	case count <= 0:
		return "0"
	case count <= 2:
		return "1-2"
	case count <= 5:
		return "3-5"
	default:
		return "6+"
	}
}
