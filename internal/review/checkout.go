package review

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Checkouts manages per-repo working copies under a root directory. Each repo
// gets one clone reused across runs (fetch is cheap; clone is not) and one
// mutex so concurrent jobs never fight over a working tree.
//
// SECURITY: the checkout is later exposed to untrusted code — analyzers execute
// PR-controlled configs and claude reads the tree — so the GitHub token must
// NEVER be written into it. The remote URL stays token-free; credentials are
// injected per git invocation via an http.extraheader config flag, which git
// does not persist. (An earlier draft put the token in the remote URL: that
// lands it in .git/config where a prompt-injected review can read it.)
type Checkouts struct {
	root  string
	token string

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func NewCheckouts(root, token string) *Checkouts {
	return &Checkouts{root: root, token: token, locks: map[string]*sync.Mutex{}}
}

func (c *Checkouts) repoLock(slug string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.locks[slug] == nil {
		c.locks[slug] = &sync.Mutex{}
	}
	return c.locks[slug]
}

// authHeader is the value git sends; basic auth with any username works for PATs.
func (c *Checkouts) authHeader() string {
	return "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+c.token))
}

// PR checks out the head of a pull request and returns the directory plus an
// unlock func the caller must defer. The tree is reset hard and cleaned first,
// so engines can assume a pristine checkout.
func (c *Checkouts) PR(ctx context.Context, pr PR) (dir string, unlock func(), err error) {
	lock := c.repoLock(pr.Slug())
	lock.Lock()
	unlock = lock.Unlock

	dir = filepath.Join(c.root, pr.Owner+"__"+pr.Repo)
	cleanURL := fmt.Sprintf("https://github.com/%s/%s.git", pr.Owner, pr.Repo)

	if _, statErr := os.Stat(filepath.Join(dir, ".git")); statErr != nil {
		if err = os.MkdirAll(dir, 0o755); err != nil {
			unlock()
			return "", nil, err
		}
		if err = c.git(ctx, dir, true, "clone", "--quiet", cleanURL, "."); err != nil {
			unlock()
			return "", nil, err
		}
	}

	type step struct {
		auth bool
		args []string
	}
	steps := []step{
		{false, []string{"remote", "set-url", "origin", cleanURL}}, // heals pre-fix tokened URLs
		{true, []string{"fetch", "--quiet", "origin", fmt.Sprintf("pull/%d/head", pr.Number)}},
		{false, []string{"checkout", "--quiet", "--detach", "FETCH_HEAD"}},
		{false, []string{"reset", "--hard", "--quiet"}},
		{false, []string{"clean", "-fdxq"}},
	}
	for _, s := range steps {
		if err = c.git(ctx, dir, s.auth, s.args...); err != nil {
			unlock()
			return "", nil, err
		}
	}
	return dir, unlock, nil
}

// git runs a git command in dir. When auth is set, credentials are passed as a
// transient -c config (never persisted to .git/config). Errors are sanitized
// against both the raw token and its base64 form.
func (c *Checkouts) git(ctx context.Context, dir string, auth bool, args ...string) error {
	full := args
	if auth {
		full = append([]string{"-c", "http.https://github.com/.extraheader=" + c.authHeader()}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Env = append(minimalEnv(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := string(out)
		for _, secret := range []string{c.token, base64.StdEncoding.EncodeToString([]byte("x-access-token:" + c.token))} {
			if secret != "" {
				msg = strings.ReplaceAll(msg, secret, "***")
			}
		}
		return fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(msg))
	}
	return nil
}

// minimalEnv is the environment handed to subprocesses that touch untrusted
// checkouts (git, analyzers, claude). The runner's own secrets (GITHUB_TOKEN,
// DATABASE_URL, POSTHOG_API_KEY, …) must not be inherited: analyzers execute
// PR-controlled configs (eslint plugins are arbitrary code), so anything in
// their env is exfiltratable.
func minimalEnv(extraVars ...string) []string {
	keep := []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL", "TZ", "XDG_CACHE_HOME", "XDG_CONFIG_HOME"}
	keep = append(keep, extraVars...)
	var env []string
	for _, k := range keep {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}
