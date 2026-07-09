// Package github is a tiny REST client covering just the calls this tool needs.
package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const apiBase = "https://api.github.com"

// APIError carries the HTTP status so callers can special-case e.g. 404.
type APIError struct {
	Status int
	msg    string
}

func (e *APIError) Error() string { return e.msg }

type Client struct {
	http        *http.Client
	token       string
	owner, repo string
}

func New(token, owner, repo string) *Client {
	return &Client{
		http:  &http.Client{Timeout: 30 * time.Second},
		token: token,
		owner: owner,
		repo:  repo,
	}
}

// Comment is an issue/PR comment (PRs are issues for the comments API).
type Comment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	HTMLURL   string    `json:"html_url"`
	IssueURL  string    `json:"issue_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
}

// IssueNumber extracts the issue/PR number from the comment's issue_url.
func (c Comment) IssueNumber() (int, bool) {
	i := strings.LastIndex(c.IssueURL, "/")
	if i < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(c.IssueURL[i+1:])
	return n, err == nil
}

func (c *Client) do(method, url string, body io.Reader, out any) error {
	return c.doCtx(context.Background(), method, url, body, out)
}

// doCtx is do with a caller-supplied context, so latency-sensitive callers (e.g.
// populating a Slack modal within the 3s trigger window) can bound the request.
func (c *Client) doCtx(ctx context.Context, method, url string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{
			Status: resp.StatusCode,
			msg:    fmt.Sprintf("%s %s: %s: %s", method, url, resp.Status, strings.TrimSpace(string(data))),
		}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// AuthUser returns the login of the authenticated user (used to skip our own comments).
func (c *Client) AuthUser() (string, error) {
	var u struct {
		Login string `json:"login"`
	}
	err := c.do(http.MethodGet, apiBase+"/user", nil, &u)
	return u.Login, err
}

// ListComments returns issue/PR comments updated at or after `since`.
func (c *Client) ListComments(since time.Time) ([]Comment, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/issues/comments?sort=updated&direction=asc&per_page=100&since=%s",
		apiBase, c.owner, c.repo, since.UTC().Format(time.RFC3339))
	var out []Comment
	err := c.do(http.MethodGet, url, nil, &out)
	return out, err
}

// Pull is an open pull request, enough to populate a picker.
type Pull struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Draft  bool   `json:"draft"`
	User   struct {
		Login string `json:"login"`
	} `json:"user"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ListOpenPulls returns the repo's open pull requests, most-recently-updated
// first (up to 100). Context-bounded for the modal-open path.
func (c *Client) ListOpenPulls(ctx context.Context) ([]Pull, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls?state=open&sort=updated&direction=desc&per_page=100",
		apiBase, c.owner, c.repo)
	var out []Pull
	err := c.doCtx(ctx, http.MethodGet, url, nil, &out)
	return out, err
}

// PullStatus is a PR's merge state — enough for the resolution oracle to settle
// markets on it (bounty pays User.Login; merge-by compares MergedAt to the deadline).
type PullStatus struct {
	Number   int        `json:"number"`
	State    string     `json:"state"` // "open" | "closed"
	Merged   bool       `json:"merged"`
	MergedAt *time.Time `json:"merged_at"`
	Title    string     `json:"title"`
	User     struct {
		Login string `json:"login"`
	} `json:"user"` // the PR author (the bounty payee)
}

// PullStatus fetches one PR's merge state. Context-bounded for the oracle loop.
func (c *Client) PullStatus(ctx context.Context, number int) (*PullStatus, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", apiBase, c.owner, c.repo, number)
	var ps PullStatus
	if err := c.doCtx(ctx, http.MethodGet, url, nil, &ps); err != nil {
		return nil, err
	}
	return &ps, nil
}

// IsPullRequest reports whether an issue number is actually a pull request.
func (c *Client) IsPullRequest(number int) (bool, error) {
	var issue struct {
		PullRequest *struct{} `json:"pull_request"`
	}
	url := fmt.Sprintf("%s/repos/%s/%s/issues/%d", apiBase, c.owner, c.repo, number)
	if err := c.do(http.MethodGet, url, nil, &issue); err != nil {
		return false, err
	}
	return issue.PullRequest != nil, nil
}

// CreateComment posts a comment and returns its ID. The body is scrubbed of
// the client's token as a last line of defense: comment bodies can carry
// engine/model/subprocess output, and no path should ever publish credentials.
func (c *Client) CreateComment(issueNumber int, body string) (int64, error) {
	if c.token != "" {
		body = strings.ReplaceAll(body, c.token, "***")
	}
	payload, _ := json.Marshal(map[string]string{"body": body})
	url := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments", apiBase, c.owner, c.repo, issueNumber)
	var out Comment
	if err := c.do(http.MethodPost, url, bytes.NewReader(payload), &out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

// AddReaction reacts to a comment. content must be one of GitHub's fixed set:
// +1 -1 laugh confused heart hooray rocket eyes. Re-reacting is idempotent
// (GitHub returns the existing reaction rather than erroring).
func (c *Client) AddReaction(commentID int64, content string) error {
	payload, _ := json.Marshal(map[string]string{"content": content})
	url := fmt.Sprintf("%s/repos/%s/%s/issues/comments/%d/reactions", apiBase, c.owner, c.repo, commentID)
	return c.do(http.MethodPost, url, bytes.NewReader(payload), nil)
}

// HasReaction reports whether `login` has already reacted to the comment with
// `content`. This is the durable "already processed" marker — no local state.
// An empty login matches any user's reaction of that content.
func (c *Client) HasReaction(commentID int64, login, content string) (bool, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/issues/comments/%d/reactions", apiBase, c.owner, c.repo, commentID)
	var out []struct {
		Content string `json:"content"`
		User    struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := c.do(http.MethodGet, url, nil, &out); err != nil {
		return false, err
	}
	for _, rx := range out {
		if rx.Content == content && (login == "" || rx.User.Login == login) {
			return true, nil
		}
	}
	return false, nil
}

// EnsureBranch makes sure `branch` exists as an ORPHAN branch — its own root
// history, holding only our assets (a README + the casino/ GIF folder), never a
// copy of the repo's main files. Safe to call when it already exists.
func (c *Client) EnsureBranch(branch string) error {
	ref := fmt.Sprintf("%s/repos/%s/%s/git/ref/heads/%s", apiBase, c.owner, c.repo, branch)
	if err := c.do(http.MethodGet, ref, nil, nil); err == nil {
		return nil // already exists
	}

	repo := func(p string) string { return fmt.Sprintf("%s/repos/%s/%s/%s", apiBase, c.owner, c.repo, p) }

	// 1. a README blob so the branch has a valid (non-empty) tree.
	readme := "# casino-review assets\n\nAuto-generated slot-machine GIFs live under `casino/`.\nThis is an orphan branch (no main-repo files); old GIFs are pruned by TTL.\n"
	var blob struct {
		SHA string `json:"sha"`
	}
	bp, _ := json.Marshal(map[string]string{"content": readme, "encoding": "utf-8"})
	if err := c.do(http.MethodPost, repo("git/blobs"), bytes.NewReader(bp), &blob); err != nil {
		return err
	}
	// 2. a tree containing just that README.
	var tree struct {
		SHA string `json:"sha"`
	}
	tp, _ := json.Marshal(map[string]any{
		"tree": []map[string]string{{"path": "README.md", "mode": "100644", "type": "blob", "sha": blob.SHA}},
	})
	if err := c.do(http.MethodPost, repo("git/trees"), bytes.NewReader(tp), &tree); err != nil {
		return err
	}
	// 3. a root commit with no parents (this is what makes it an orphan).
	var commit struct {
		SHA string `json:"sha"`
	}
	cp, _ := json.Marshal(map[string]any{
		"message": "casino-review: init assets branch",
		"tree":    tree.SHA,
		"parents": []string{},
	})
	if err := c.do(http.MethodPost, repo("git/commits"), bytes.NewReader(cp), &commit); err != nil {
		return err
	}
	// 4. point the new branch ref at it.
	rp, _ := json.Marshal(map[string]string{"ref": "refs/heads/" + branch, "sha": commit.SHA})
	return c.do(http.MethodPost, repo("git/refs"), bytes.NewReader(rp), nil)
}

// Asset is an uploaded file's raw URL, suitable for embedding in a comment.
type Asset struct {
	DownloadURL string
}

// PutFile commits `content` to `path` on `branch` and returns the asset handle.
func (c *Client) PutFile(branch, path string, content []byte, message string) (Asset, error) {
	payload, _ := json.Marshal(map[string]string{
		"message": message,
		"content": base64.StdEncoding.EncodeToString(content),
		"branch":  branch,
	})
	url := fmt.Sprintf("%s/repos/%s/%s/contents/%s", apiBase, c.owner, c.repo, path)
	var out struct {
		Content struct {
			DownloadURL string `json:"download_url"`
		} `json:"content"`
	}
	if err := c.do(http.MethodPut, url, bytes.NewReader(payload), &out); err != nil {
		return Asset{}, err
	}
	return Asset{DownloadURL: out.Content.DownloadURL}, nil
}

// DirEntry is one item from a directory listing.
type DirEntry struct {
	Name string `json:"name"`
	SHA  string `json:"sha"`
	Type string `json:"type"` // "file" or "dir"
}

// ListDir lists the contents of a directory on a branch. A missing directory
// (404) returns (nil, nil) rather than an error.
func (c *Client) ListDir(branch, path string) ([]DirEntry, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/contents/%s?ref=%s", apiBase, c.owner, c.repo, path, branch)
	var out []DirEntry
	if err := c.do(http.MethodGet, url, nil, &out); err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	return out, nil
}

// DeleteFile removes `path` (identified by blob `sha`) from `branch`.
func (c *Client) DeleteFile(branch, path, sha, message string) error {
	payload, _ := json.Marshal(map[string]string{"message": message, "sha": sha, "branch": branch})
	url := fmt.Sprintf("%s/repos/%s/%s/contents/%s", apiBase, c.owner, c.repo, path)
	return c.do(http.MethodDelete, url, bytes.NewReader(payload), nil)
}
