package review

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

// Spec is one entry in the reviews registry file.
type Spec struct {
	Name       string   `json:"name"`
	Engine     string   `json:"engine"`                // dispatch | claude | analyzer
	PromptFile string   `json:"prompt_file,omitempty"` // claude: persona prompt
	Model      string   `json:"model,omitempty"`       // claude: optional model override
	MaxTurns   int      `json:"max_turns,omitempty"`   // claude
	Cmd        []string `json:"cmd,omitempty"`         // analyzer: command argv
	Parser     string   `json:"parser,omitempty"`      // analyzer: eslint | tsc | generic
	Timeout    string   `json:"timeout,omitempty"`     // e.g. "10m"
}

// AnalyzerStep is one tool inside the all-in-one static addon.
type AnalyzerStep struct {
	Cmd     []string `json:"cmd"`
	Parser  string   `json:"parser,omitempty"` // eslint | tsc | generic
	Timeout string   `json:"timeout,omitempty"`
}

// AddonSpec is the bonus reviewer: it is NOT on the reel. After the reel picks
// a winner, the addon fires with probability Chance (1.0 = permanent) and runs
// every analyzer step over one checkout, posting one merged findings comment.
type AddonSpec struct {
	Name      string         `json:"name"`
	Chance    float64        `json:"chance"` // 0..1
	Analyzers []AnalyzerStep `json:"analyzers"`
}

// JudgeSpec is an arbitration persona (used from P4; parsed now so one file
// carries the whole registry).
type JudgeSpec struct {
	Name       string `json:"name"`
	PromptFile string `json:"prompt_file"`
	Model      string `json:"model,omitempty"`
}

// Registry is the full parsed reviews file.
type Registry struct {
	Reviews []Spec      `json:"reviews"`
	Addon   *AddonSpec  `json:"addon,omitempty"`
	Judges  []JudgeSpec `json:"judges,omitempty"`
}

// LoadRegistry reads a registry: from the JSON file when path is set,
// otherwise from the legacy comma-list names (all dispatch engines) — the
// backward-compatible default. trigger is the bot's own trigger command
// (e.g. "/casino-review"): no engine may collide with it, or the dispatch
// comment the bot posts would re-trigger the bot itself, forever.
func LoadRegistry(path string, legacyNames []string, trigger string) (*Registry, error) {
	// A missing REVIEWS_FILE — or a *directory* at that path, the Docker
	// bind-mount footgun when reviews.json wasn't created on the host before
	// `up` — must not crash-loop the runner. Fall back to the legacy REVIEWS
	// list (the dispatch reel) with a loud warning instead. A file that exists
	// but is malformed is still a hard error: that's a real misconfiguration,
	// not an unconfigured one.
	if path != "" {
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			why := "does not exist"
			if err == nil && info.IsDir() {
				why = "is a directory (create reviews.json as a FILE on the host before `docker compose up`)"
			}
			log.Printf("reviews: REVIEWS_FILE %q %s — falling back to the REVIEWS list", path, why)
			path = ""
		}
	}

	var r *Registry
	if path == "" {
		r = &Registry{}
		for _, n := range legacyNames {
			r.Reviews = append(r.Reviews, Spec{Name: n, Engine: "dispatch"})
		}
		if len(r.Reviews) == 0 {
			return nil, fmt.Errorf("no reviews configured (set REVIEWS or REVIEWS_FILE)")
		}
	} else {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read reviews file: %w", err)
		}
		r = &Registry{}
		if err := json.Unmarshal(data, r); err != nil {
			return nil, fmt.Errorf("parse reviews file %s: %w", path, err)
		}
	}
	if err := r.validate(trigger); err != nil {
		if path != "" {
			return nil, fmt.Errorf("reviews file %s: %w", path, err)
		}
		return nil, err
	}
	return r, nil
}

func (r *Registry) validate(trigger string) error {
	if len(r.Reviews) == 0 {
		return fmt.Errorf("must define at least one review")
	}
	seen := map[string]bool{}
	for i, s := range r.Reviews {
		if s.Name == "" {
			return fmt.Errorf("reviews[%d]: name is required", i)
		}
		if seen[s.Name] {
			return fmt.Errorf("duplicate review name %q", s.Name)
		}
		seen[s.Name] = true
		if trigger != "" && "/"+s.Name == trigger {
			return fmt.Errorf("review %q collides with the trigger %q — the bot would trigger itself", s.Name, trigger)
		}
		switch s.Engine {
		case "dispatch":
		case "claude":
			// SUNSET: LLM review engines are gated off until the hosting infra
			// is ready to run them properly (claude CLI + key management in the
			// runner image, cost controls). The foundations remain — engine.go's
			// interface, claude.go's runner+parser, the persona files, and their
			// tests — so returning is: delete this branch, restore the CLI in
			// Dockerfile.runner, and re-add ANTHROPIC_API_KEY to the env.
			return fmt.Errorf("review %q: claude engines are sunset for now — the plumbing remains in internal/review/claude.go; see README \"LLM reviewers (sunset)\"", s.Name)
		case "analyzer":
			if len(s.Cmd) == 0 {
				return fmt.Errorf("review %q: analyzer engine requires cmd", s.Name)
			}
			switch s.Parser {
			case "", "generic", "eslint", "tsc":
			default:
				return fmt.Errorf("review %q: unknown parser %q", s.Name, s.Parser)
			}
		default:
			return fmt.Errorf("review %q: unknown engine %q", s.Name, s.Engine)
		}
		if s.Timeout != "" {
			if _, err := time.ParseDuration(s.Timeout); err != nil {
				return fmt.Errorf("review %q: invalid timeout: %w", s.Name, err)
			}
		}
	}
	if a := r.Addon; a != nil {
		if a.Name == "" {
			return fmt.Errorf("addon: name is required")
		}
		if seen[a.Name] {
			return fmt.Errorf("addon name %q duplicates a review name", a.Name)
		}
		if trigger != "" && "/"+a.Name == trigger {
			return fmt.Errorf("addon %q collides with the trigger %q", a.Name, trigger)
		}
		if a.Chance < 0 || a.Chance > 1 {
			return fmt.Errorf("addon %q: chance must be within [0,1], got %v", a.Name, a.Chance)
		}
		if len(a.Analyzers) == 0 {
			return fmt.Errorf("addon %q: at least one analyzer step is required", a.Name)
		}
		for i, st := range a.Analyzers {
			if len(st.Cmd) == 0 {
				return fmt.Errorf("addon %q analyzers[%d]: cmd is required", a.Name, i)
			}
			switch st.Parser {
			case "", "generic", "eslint", "tsc":
			default:
				return fmt.Errorf("addon %q analyzers[%d]: unknown parser %q", a.Name, i, st.Parser)
			}
			if st.Timeout != "" {
				if _, err := time.ParseDuration(st.Timeout); err != nil {
					return fmt.Errorf("addon %q analyzers[%d]: invalid timeout: %w", a.Name, i, err)
				}
			}
		}
	}
	for i, j := range r.Judges {
		if j.Name == "" || j.PromptFile == "" {
			return fmt.Errorf("judges[%d]: name and prompt_file are required", i)
		}
	}
	return nil
}

// Names returns the slot labels in registry order.
func (r *Registry) Names() []string {
	out := make([]string, len(r.Reviews))
	for i, s := range r.Reviews {
		out[i] = s.Name
	}
	return out
}

// Build constructs the engine for a spec.
func Build(s Spec, deps Deps) (Engine, error) {
	timeout := func(def time.Duration) time.Duration {
		if s.Timeout == "" {
			return def
		}
		d, _ := time.ParseDuration(s.Timeout) // validated at load
		return d
	}
	switch s.Engine {
	case "dispatch":
		return &dispatchEngine{name: s.Name, gh: deps.GH, dryRun: deps.DryRun}, nil
	case "claude":
		prompt, err := os.ReadFile(s.PromptFile)
		if err != nil {
			return nil, fmt.Errorf("review %q: %w", s.Name, err)
		}
		maxTurns := s.MaxTurns
		if maxTurns <= 0 {
			maxTurns = defaultMaxTurns
		}
		return &claudeEngine{
			name: s.Name, persona: strings.TrimSpace(string(prompt)), model: s.Model,
			maxTurns: maxTurns, timeout: timeout(defaultClaudeTimeout),
			bin: deps.ClaudeBin, checkouts: deps.Checkouts, gh: deps.GH, dryRun: deps.DryRun,
		}, nil
	case "analyzer":
		parser := s.Parser
		if parser == "" {
			parser = "generic"
		}
		return &analyzerEngine{
			name: s.Name, cmd: s.Cmd, parser: parser,
			timeout: timeout(defaultAnalyzerTimeout), checkouts: deps.Checkouts, gh: deps.GH, dryRun: deps.DryRun,
		}, nil
	default:
		return nil, fmt.Errorf("unknown engine %q", s.Engine)
	}
}

// BuildAll constructs every engine in the registry, keyed by name.
func BuildAll(r *Registry, deps Deps) (map[string]Engine, error) {
	out := make(map[string]Engine, len(r.Reviews))
	for _, s := range r.Reviews {
		e, err := Build(s, deps)
		if err != nil {
			return nil, err
		}
		out[s.Name] = e
	}
	return out, nil
}
