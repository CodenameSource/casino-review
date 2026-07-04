package review

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"casino-review/internal/github"
	"casino-review/internal/telemetry"
	"casino-review/internal/templates"
)

// claudeEngine runs a Claude Code persona over a PR checkout, headless, with
// read-only tools, and parses a strict-JSON findings report from its output.
type claudeEngine struct {
	name      string
	persona   string
	model     string
	maxTurns  int
	timeout   time.Duration
	bin       string
	checkouts *Checkouts
	gh        *github.Client
	dryRun    bool
}

func (c *claudeEngine) Name() string { return c.name }
func (c *claudeEngine) Kind() string { return "claude" }

// findingsSchemaInstr tells the model exactly what to return. The engine
// refuses to guess: unparseable output is an error, never fabricated findings.
const findingsSchemaInstr = `
Respond with ONLY a JSON object, no prose before or after, of this exact shape:
{"findings":[{"path":"relative/file","line":123,"severity":"high|medium|low|note","title":"one line","body":"short explanation"}],"summary":"1-3 sentence overall assessment"}
An empty findings array is a perfectly good answer for a clean PR.`

func (c *claudeEngine) Run(ctx context.Context, pr PR) (Result, error) {
	dir, unlock, err := c.checkouts.PR(ctx, pr)
	if err != nil {
		return Result{}, err
	}
	defer unlock()

	prompt := fmt.Sprintf(`%s

You are reviewing pull request #%d of %s. The PR's head is checked out in the
current directory. Use git to inspect what the PR changes, e.g.:
  git log --oneline -10
  git diff HEAD~1 --stat   (adjust the base as appropriate)
Read the changed files, focus your persona's lens on the diff, and report.
%s`, c.persona, pr.Number, pr.Slug(), findingsSchemaInstr)

	args := []string{
		"-p", prompt,
		"--output-format", "json",
		"--max-turns", strconv.Itoa(c.maxTurns),
		"--allowedTools", "Read,Grep,Glob,Bash(git diff:*),Bash(git log:*),Bash(git show:*)",
	}
	if c.model != "" {
		args = append(args, "--model", c.model)
	}

	runCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, c.bin, args...)
	cmd.Dir = dir
	// Minimal env: the checkout is untrusted input to the model; the runner's
	// secrets must not be reachable. Only the Anthropic key goes through.
	cmd.Env = minimalEnv("ANTHROPIC_API_KEY")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return Result{}, fmt.Errorf("claude run: %v: %s", err, tail(stderr.String(), 400))
	}

	report, cost, err := parseClaudeOutput(stdout.Bytes())
	if err != nil {
		return Result{}, fmt.Errorf("claude output: %w", err)
	}
	telemetry.ClaudeCostUSD.WithLabelValues(c.name).Add(cost)

	res := Result{Findings: report.Findings, Summary: report.Summary, KnownFindings: true, CostUSD: cost}
	if c.dryRun {
		return res, nil
	}
	commentID, err := c.gh.CreateComment(pr.Number,
		templates.ReviewFindings(c.name, toTemplateFindings(report.Findings), report.Summary))
	if err != nil {
		return res, fmt.Errorf("post findings: %w", err)
	}
	res.CommentID = commentID
	return res, nil
}

// findingsReport is the strict JSON the persona must return.
type findingsReport struct {
	Findings []Finding `json:"findings"`
	Summary  string    `json:"summary"`
}

// parseClaudeOutput unwraps `claude -p --output-format json` (a result
// envelope whose .result field holds the model's final text) and parses the
// findings report out of that text.
func parseClaudeOutput(out []byte) (findingsReport, float64, error) {
	var envelope struct {
		Type      string  `json:"type"`
		Subtype   string  `json:"subtype"`
		IsError   bool    `json:"is_error"`
		Result    string  `json:"result"`
		TotalCost float64 `json:"total_cost_usd"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		return findingsReport{}, 0, fmt.Errorf("bad envelope: %w (output tail: %s)", err, tail(string(out), 200))
	}
	if envelope.IsError {
		return findingsReport{}, envelope.TotalCost, fmt.Errorf("claude reported error (%s): %s", envelope.Subtype, tail(envelope.Result, 300))
	}
	report, err := extractReport(envelope.Result)
	return report, envelope.TotalCost, err
}

// extractReport digs the JSON report out of the model's text, tolerating code
// fences and surrounding prose but requiring valid JSON of the right shape.
func extractReport(text string) (findingsReport, error) {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return findingsReport{}, fmt.Errorf("no JSON object in response: %s", tail(text, 200))
	}
	var r findingsReport
	dec := json.NewDecoder(strings.NewReader(text[start : end+1]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		// Second, laxer attempt: unknown fields allowed.
		if err2 := json.Unmarshal([]byte(text[start:end+1]), &r); err2 != nil {
			return findingsReport{}, fmt.Errorf("unparseable report: %v", err2)
		}
	}
	return r, nil
}

func toTemplateFindings(fs []Finding) []templates.Finding {
	out := make([]templates.Finding, len(fs))
	for i, f := range fs {
		out[i] = templates.Finding{Path: f.Path, Line: f.Line, Severity: f.Severity, Title: f.Title, Body: f.Body}
	}
	return out
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
