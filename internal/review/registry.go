package review

import (
	"encoding/json"
	"fmt"
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
	Judges  []JudgeSpec `json:"judges,omitempty"`
}

// LoadRegistry reads a registry: from the JSON file when path is set,
// otherwise from the legacy comma-list names (all dispatch engines) — the
// backward-compatible default. trigger is the bot's own trigger command
// (e.g. "/casino-review"): no engine may collide with it, or the dispatch
// comment the bot posts would re-trigger the bot itself, forever.
func LoadRegistry(path string, legacyNames []string, trigger string) (*Registry, error) {
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
			if s.PromptFile == "" {
				return fmt.Errorf("review %q: claude engine requires prompt_file", s.Name)
			}
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
