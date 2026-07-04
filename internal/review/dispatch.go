package review

import (
	"context"
	"fmt"

	"casino-review/internal/github"
	"casino-review/internal/templates"
)

// dispatchEngine is the original behavior: post "/<name>" and let an external
// bot do the review. It cannot observe findings (KnownFindings=false).
type dispatchEngine struct {
	name   string
	gh     *github.Client
	dryRun bool
}

func (d *dispatchEngine) Name() string { return d.name }
func (d *dispatchEngine) Kind() string { return "dispatch" }

func (d *dispatchEngine) Run(ctx context.Context, pr PR) (Result, error) {
	if d.dryRun {
		return Result{Summary: "dry-run: would post " + templates.DispatchTrigger(d.name)}, nil
	}
	id, err := d.gh.CreateComment(pr.Number, templates.DispatchTrigger(d.name))
	if err != nil {
		return Result{}, fmt.Errorf("dispatch %s: %w", d.name, err)
	}
	return Result{KnownFindings: false, CommentID: id}, nil
}
