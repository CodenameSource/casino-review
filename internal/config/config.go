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
	Reviews      []string      // candidate review names WITHOUT leading slash, e.g. "tsetso-review"
	PollInterval time.Duration // how often to poll for new comments
	DisplayFor   time.Duration // delay between posting the GIF and posting the "/<winner>" trigger
	AssetsBranch string        // branch the GIF is committed to so it can be embedded
	AssetsTTL    time.Duration // prune committed GIFs older than this so they don't pile up
	Reaction     string        // reaction marking a comment processed — also the dedup state
}

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
	}

	c.Owner, c.Repo = parseRepo(env("GITHUB_REPO", ""))

	// Where to host the GIF. Defaults to the monitored repo; point it at a PUBLIC
	// repo when the monitored repo is private so the raw URL never expires.
	c.AssetsOwner, c.AssetsRepo = parseRepo(env("ASSETS_REPO", ""))
	if c.AssetsOwner == "" {
		c.AssetsOwner, c.AssetsRepo = c.Owner, c.Repo
	}

	reviews := env("REVIEWS", "tsetso-review,dimoreview,gigareview")
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
