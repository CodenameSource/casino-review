// Package review defines the typed review engines the slot machine picks
// among: dispatch (post a trigger comment for an external bot — the original
// behavior), claude (run a Claude Code persona over a PR checkout), and
// analyzer (run a static tool like eslint/tsc and parse its output).
//
// Engines are also the experiment's measurement instrument: each run's
// findings land in review_runs, feeding the selector (milestone 2) and the
// findings-count resolution oracle (markets, later phases).
package review

import (
	"context"
	"time"

	"casino-review/internal/github"
)

// PR identifies the pull request under review.
type PR struct {
	Owner  string
	Repo   string
	Number int
}

func (p PR) Slug() string { return p.Owner + "/" + p.Repo }

// Finding is one issue surfaced by an engine.
type Finding struct {
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Severity string `json:"severity"` // high | medium | low | note
	Title    string `json:"title"`
	Body     string `json:"body,omitempty"`
}

// Result is the outcome of one engine run.
type Result struct {
	Findings      []Finding
	Summary       string
	KnownFindings bool    // false = engine can't observe findings (dispatch)
	CommentID     int64   // PR comment posted with the results, 0 if none
	CostUSD       float64 // LLM spend, 0 for non-LLM engines
}

// Engine executes one kind of review.
type Engine interface {
	Name() string // slot label, unique within the registry
	Kind() string // "dispatch" | "claude" | "analyzer"
	Run(ctx context.Context, pr PR) (Result, error)
}

// Deps carries the shared plumbing engines need.
type Deps struct {
	GH        *github.Client // client for the monitored repo (posting comments)
	Token     string         // used for authenticated PR checkouts
	Checkouts *Checkouts
	ClaudeBin string
	DryRun    bool // don't post comments; print/return results only (CLI dry runs)
}

const (
	defaultClaudeTimeout   = 10 * time.Minute
	defaultAnalyzerTimeout = 5 * time.Minute
	defaultMaxTurns        = 30
)
