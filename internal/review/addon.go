package review

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"casino-review/internal/github"
	"casino-review/internal/templates"
)

// Addon is the bonus reviewer: an all-in-one static pass that fires with
// probability Chance after the reel's winner is chosen. It is not a reel
// entry; the worker rolls for it per spin.
type Addon struct {
	Engine Engine
	Chance float64
}

// BuildAddon constructs the addon engine from its spec.
func BuildAddon(s *AddonSpec, deps Deps) (*Addon, error) {
	if s == nil {
		return nil, nil
	}
	steps := make([]addonStep, len(s.Analyzers))
	for i, a := range s.Analyzers {
		parser := a.Parser
		if parser == "" {
			parser = "generic"
		}
		timeout := defaultAnalyzerTimeout
		if a.Timeout != "" {
			timeout, _ = time.ParseDuration(a.Timeout) // validated at load
		}
		steps[i] = addonStep{cmd: a.Cmd, parser: parser, timeout: timeout}
	}
	return &Addon{
		Engine: &addonEngine{name: s.Name, steps: steps, checkouts: deps.Checkouts, gh: deps.GH, dryRun: deps.DryRun},
		Chance: s.Chance,
	}, nil
}

type addonStep struct {
	cmd     []string
	parser  string
	timeout time.Duration
}

func (s addonStep) label() string {
	if s.parser != "generic" && s.parser != "" {
		return s.parser
	}
	return s.cmd[0]
}

// addonEngine runs every analyzer step over ONE checkout and posts one merged
// comment. A step that fails hard becomes a finding rather than sinking the
// whole bonus — an all-in-one reviewer should degrade per-tool, not all-or-nothing.
type addonEngine struct {
	name      string
	steps     []addonStep
	checkouts *Checkouts
	gh        *github.Client
	dryRun    bool
}

func (a *addonEngine) Name() string { return a.name }
func (a *addonEngine) Kind() string { return "addon" }

func (a *addonEngine) Run(ctx context.Context, pr PR) (Result, error) {
	dir, unlock, err := a.checkouts.PR(ctx, pr)
	if err != nil {
		return Result{}, err
	}
	defer unlock()

	var findings []Finding
	var toolSummaries []string
	for _, step := range a.steps {
		stepFindings, stepErr := a.runStep(ctx, dir, step)
		if stepErr != nil {
			findings = append(findings, Finding{
				Severity: "medium",
				Title:    fmt.Sprintf("%s failed to run", step.label()),
				Body:     stepErr.Error(),
			})
			toolSummaries = append(toolSummaries, step.label()+": failed")
			continue
		}
		for _, f := range stepFindings {
			f.Title = "[" + step.label() + "] " + f.Title
			findings = append(findings, f)
		}
		toolSummaries = append(toolSummaries, fmt.Sprintf("%s: %d", step.label(), len(stepFindings)))
	}

	summary := "Bonus static pass — " + strings.Join(toolSummaries, ", ")
	res := Result{Findings: findings, Summary: summary, KnownFindings: true}
	if a.dryRun {
		return res, nil
	}
	commentID, err := a.gh.CreateComment(pr.Number,
		templates.ReviewFindings(a.name+" (bonus round)", toTemplateFindings(findings), summary))
	if err != nil {
		return res, fmt.Errorf("post findings: %w", err)
	}
	res.CommentID = commentID
	return res, nil
}

func (a *addonEngine) runStep(ctx context.Context, dir string, step addonStep) ([]Finding, error) {
	runCtx, cancel := context.WithTimeout(ctx, step.timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, step.cmd[0], step.cmd[1:]...)
	cmd.Dir = dir
	cmd.Env = minimalEnv() // untrusted checkout: no runner secrets
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if runCtx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("timed out after %s", step.timeout)
	}
	return parseAnalyzerOutput(step.parser, stdout.Bytes(), stderr.Bytes(), runErr)
}
