package review

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"casino-review/internal/github"
	"casino-review/internal/templates"
)

// analyzerEngine runs a static tool in the PR checkout and parses its output.
// Analyzers commonly exit non-zero when they find problems, so a non-zero exit
// with parseable output is a successful run WITH findings, not a failure.
type analyzerEngine struct {
	name      string
	cmd       []string
	parser    string // eslint | tsc | generic
	timeout   time.Duration
	checkouts *Checkouts
	gh        *github.Client
	dryRun    bool
}

func (a *analyzerEngine) Name() string { return a.name }
func (a *analyzerEngine) Kind() string { return "analyzer" }

func (a *analyzerEngine) Run(ctx context.Context, pr PR) (Result, error) {
	dir, unlock, err := a.checkouts.PR(ctx, pr)
	if err != nil {
		return Result{}, err
	}
	defer unlock()

	runCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, a.cmd[0], a.cmd[1:]...)
	cmd.Dir = dir
	// Minimal env: analyzers execute PR-controlled configs (eslint plugins are
	// arbitrary code), so the runner's secrets must not be in their environment.
	cmd.Env = minimalEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if runCtx.Err() == context.DeadlineExceeded {
		return Result{}, fmt.Errorf("%s timed out after %s", a.name, a.timeout)
	}

	findings, parseErr := parseAnalyzerOutput(a.parser, stdout.Bytes(), stderr.Bytes(), runErr)
	if parseErr != nil {
		return Result{}, fmt.Errorf("%s: %w", a.name, parseErr)
	}

	summary := fmt.Sprintf("`%s` completed.", strings.Join(a.cmd, " "))
	res := Result{Findings: findings, Summary: summary, KnownFindings: true}
	if a.dryRun {
		return res, nil
	}
	commentID, err := a.gh.CreateComment(pr.Number,
		templates.ReviewFindings(a.name, toTemplateFindings(findings), summary))
	if err != nil {
		return res, fmt.Errorf("post findings: %w", err)
	}
	res.CommentID = commentID
	return res, nil
}

func parseAnalyzerOutput(parser string, stdout, stderr []byte, runErr error) ([]Finding, error) {
	switch parser {
	case "eslint":
		return parseESLint(stdout, runErr)
	case "tsc":
		findings := parseTSC(append(stdout, stderr...))
		// tsc exits non-zero both for diagnostics and for real failures (bad
		// tsconfig, crash). Non-zero with zero parsed diagnostics is a failure —
		// reporting it as "clean" would silently disable the analyzer.
		if runErr != nil && len(findings) == 0 {
			return nil, fmt.Errorf("tsc failed with no parseable diagnostics: %v: %s", runErr,
				tail(string(stdout)+"\n"+string(stderr), 300))
		}
		return findings, nil
	default:
		return parseGeneric(stdout, stderr, runErr), nil
	}
}

// parseESLint reads `eslint --format json` output: an array of file results.
func parseESLint(out []byte, runErr error) ([]Finding, error) {
	var files []struct {
		FilePath string `json:"filePath"`
		Messages []struct {
			RuleID   string `json:"ruleId"`
			Severity int    `json:"severity"` // 1 warn, 2 error
			Message  string `json:"message"`
			Line     int    `json:"line"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &files); err != nil {
		// Output isn't the JSON report — a real failure (config error, crash).
		if runErr != nil {
			return nil, fmt.Errorf("eslint failed: %v: %s", runErr, tail(string(out), 300))
		}
		return nil, fmt.Errorf("eslint output not JSON: %w", err)
	}
	var findings []Finding
	for _, f := range files {
		for _, m := range f.Messages {
			sev := "low"
			if m.Severity >= 2 {
				sev = "medium"
			}
			title := m.Message
			if m.RuleID != "" {
				title = m.RuleID + ": " + m.Message
			}
			findings = append(findings, Finding{
				Path: relativize(f.FilePath), Line: m.Line, Severity: sev, Title: title,
			})
		}
	}
	return findings, nil
}

var tscLine = regexp.MustCompile(`(?m)^(.+?)\((\d+),\d+\): (error|warning) (TS\d+): (.+)$`)

// parseTSC reads `tsc --noEmit` plain-text diagnostics.
func parseTSC(out []byte) []Finding {
	var findings []Finding
	for _, m := range tscLine.FindAllStringSubmatch(string(out), -1) {
		line, _ := strconv.Atoi(m[2])
		sev := "medium"
		if m[3] == "warning" {
			sev = "low"
		}
		findings = append(findings, Finding{
			Path: m[1], Line: line, Severity: sev, Title: m[4] + ": " + m[5],
		})
	}
	return findings
}

// parseGeneric: exit 0 = clean; non-zero = one finding carrying the output.
func parseGeneric(stdout, stderr []byte, runErr error) []Finding {
	if runErr == nil {
		return nil
	}
	out := strings.TrimSpace(string(stdout) + "\n" + string(stderr))
	return []Finding{{
		Severity: "medium",
		Title:    fmt.Sprintf("analyzer exited non-zero (%v)", runErr),
		Body:     "```\n" + tail(out, 1500) + "\n```",
	}}
}

// relativize strips an absolute checkout prefix down to something readable.
func relativize(p string) string {
	if i := strings.LastIndex(p, "__"); i >= 0 {
		if j := strings.Index(p[i:], "/"); j >= 0 {
			return p[i+j+1:]
		}
	}
	return p
}
