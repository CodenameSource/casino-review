// Package config loads runtime configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

type Config struct {
	Token        string        // GitHub token. Read scope monitors; write scope is required to react, comment, and commit the GIF.
	Owner        string        // monitored repo owner
	Repo         string        // monitored repo name
	AssetsOwner  string        // repo the GIF is committed to (defaults to the monitored repo)
	AssetsRepo   string        // ^ make this a PUBLIC repo if the monitored repo is private, so the embed URL never expires
	Trigger      string        // comment that starts a spin, e.g. "/casino-review"
	Reviews      []string      // legacy fallback: candidate review names (all dispatch engines)
	PollInterval time.Duration // how often to poll for new comments
	DisplayFor   time.Duration // delay between posting the GIF and running the winning review
	AssetsBranch string        // branch the GIF is committed to so it can be embedded
	AssetsTTL    time.Duration // prune committed GIFs older than this so they don't pile up
	Reaction     string        // reaction marking a comment processed — also the dedup state

	DatabaseURL string // Postgres; source of truth for jobs, review runs, events, markets
	ReviewsFile string // JSON registry of typed review engines + judges; overrides Reviews
	Workdir     string // runner scratch space for PR checkouts
	ClaudeBin   string // claude CLI binary for LLM engines/judges

	MetricsAddr string // prometheus /metrics listen address; empty = disabled

	SlackBotToken string   // xoxb-… (chat)
	SlackAppToken string   // xapp-… (socket mode)
	SlackChannel  string   // channel ID (C…) or #name — the ONLY channel the bot honors
	SlackAdmins   []string // Slack user IDs allowed to lock/resolve/void; empty = those verbs disabled in Slack

	OracleEnabled      bool          // run the resolution oracle in core (auto-settle markets on merge/findings/expiry)
	OraclePollInterval time.Duration // how often the oracle scans for resolvable markets
}

// RepoSlug returns "owner/repo" for the monitored repo.
func (c *Config) RepoSlug() string { return c.Owner + "/" + c.Repo }

// parseRepo accepts "owner/repo" or a full URL like
// https://github.com/owner/repo(.git)(/) and returns owner, repo ("" on failure).
func parseRepo(s string) (owner, repo string) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "github.com/")
	s = strings.Trim(s, "/")
	if parts := strings.Split(s, "/"); len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
		return parts[0], strings.TrimSuffix(parts[1], ".git")
	}
	return "", ""
}

// validReactions are the only contents GitHub's reactions API accepts. There is
// no 8-ball; "eyes" is the closest "I'm looking at this / processing" marker.
var validReactions = map[string]bool{
	"+1": true, "-1": true, "laugh": true, "confused": true,
	"heart": true, "hooray": true, "rocket": true, "eyes": true,
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// Load reads configuration from the environment and validates it.
func Load() (*Config, error) {
	c := &Config{
		Token:        os.Getenv("GITHUB_TOKEN"),
		Trigger:      env("TRIGGER", "/casino-review"),
		AssetsBranch: env("ASSETS_BRANCH", "casino-review-assets"),
		Reaction:     strings.ToLower(env("REACTION", "rocket")),
		DatabaseURL:  env("DATABASE_URL", ""),
		ReviewsFile:  env("REVIEWS_FILE", ""),
		Workdir:      env("WORKDIR", "./work"),
		ClaudeBin:    env("CLAUDE_BIN", "claude"),
		MetricsAddr:  env("METRICS_ADDR", ""),

		SlackBotToken: env("SLACK_BOT_TOKEN", ""),
		SlackAppToken: env("SLACK_APP_TOKEN", ""),
		SlackChannel:  env("SLACK_CHANNEL", ""),
	}
	for _, id := range strings.Split(env("SLACK_ADMINS", ""), ",") {
		if id = strings.TrimSpace(id); id != "" {
			c.SlackAdmins = append(c.SlackAdmins, id)
		}
	}

	c.Owner, c.Repo = parseRepo(env("GITHUB_REPO", ""))

	// Where to host the GIF. Defaults to the monitored repo; point it at a PUBLIC
	// repo when the monitored repo is private so the raw URL never expires.
	c.AssetsOwner, c.AssetsRepo = parseRepo(env("ASSETS_REPO", ""))
	if c.AssetsOwner == "" {
		c.AssetsOwner, c.AssetsRepo = c.Owner, c.Repo
	}

	reviews := env("REVIEWS", "tsetso-review,dimoreview,gigareview,barbie-review")
	for _, r := range strings.Split(reviews, ",") {
		if r = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(r), "/")); r != "" {
			c.Reviews = append(c.Reviews, r)
		}
	}

	var err error
	if c.PollInterval, err = time.ParseDuration(env("POLL_INTERVAL", "30s")); err != nil {
		return nil, fmt.Errorf("invalid POLL_INTERVAL: %w", err)
	}
	if c.DisplayFor, err = time.ParseDuration(env("DISPLAY_DURATION", "20s")); err != nil {
		return nil, fmt.Errorf("invalid DISPLAY_DURATION: %w", err)
	}
	if c.AssetsTTL, err = time.ParseDuration(env("ASSETS_TTL", "720h")); err != nil { // 30 days
		return nil, fmt.Errorf("invalid ASSETS_TTL: %w", err)
	}
	c.OracleEnabled = env("ORACLE_ENABLED", "true") == "true"
	if c.OraclePollInterval, err = time.ParseDuration(env("ORACLE_POLL_INTERVAL", "60s")); err != nil {
		return nil, fmt.Errorf("invalid ORACLE_POLL_INTERVAL: %w", err)
	}

	if c.Token == "" {
		return nil, fmt.Errorf("GITHUB_TOKEN is required")
	}
	if c.Owner == "" || c.Repo == "" {
		return nil, fmt.Errorf("GITHUB_REPO must be set as owner/repo")
	}
	if len(c.Reviews) == 0 {
		return nil, fmt.Errorf("REVIEWS must contain at least one review name")
	}
	if !validReactions[c.Reaction] {
		return nil, fmt.Errorf("REACTION %q is not a valid GitHub reaction (one of: +1 -1 laugh confused heart hooray rocket eyes)", c.Reaction)
	}
	return c, nil
}
