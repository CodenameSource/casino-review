package store

import (
	"context"
	"os"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping DB-backed store tests")
	}
	ctx := context.Background()
	st, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

// Regression: GitHub comment IDs (~5 billion) exceed int32. The insert must
// store them as BIGINT, not coerce the parameter to int4.
func TestInsertReviewRunLargeCommentID(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	n0 := 0
	id, err := st.InsertReviewRun(ctx, ReviewRun{
		Repo: "big/repo", PR: 3647, Engine: "gigareview", EngineKind: "dispatch",
		CommentID: 4901804863, FindingsCount: &n0, // real prod value from the field
		StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("large comment_id must insert: %v", err)
	}
	if id == 0 {
		t.Fatal("expected a returned id")
	}
}

func TestTrackedPRs(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	repo := "trk/" + time.Now().Format("150405.000000")
	n2 := 2

	base := time.Now().UTC().Add(-time.Hour)
	// PR 7: two runs; latest is an addon with 2 findings.
	st.InsertReviewRun(ctx, ReviewRun{Repo: repo, PR: 7, Engine: "tsetso-review", EngineKind: "dispatch", StartedAt: base, FinishedAt: base})
	st.InsertReviewRun(ctx, ReviewRun{Repo: repo, PR: 7, Engine: "static", EngineKind: "addon", FindingsCount: &n2, StartedAt: base.Add(time.Minute), FinishedAt: base.Add(time.Minute)})
	// PR 9: one run, errored, more recent than PR 7.
	st.InsertReviewRun(ctx, ReviewRun{Repo: repo, PR: 9, Engine: "eslint", EngineKind: "analyzer", Error: "boom\nsecond line", StartedAt: base.Add(2 * time.Minute), FinishedAt: base.Add(2 * time.Minute)})

	prs, err := st.TrackedPRs(ctx, repo, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 2 {
		t.Fatalf("tracked = %+v", prs)
	}
	// Most-recently-active first → PR 9.
	if prs[0].PR != 9 || prs[0].Runs != 1 || prs[0].LastError == "" || prs[0].LastFindings != nil {
		t.Fatalf("row0 = %+v", prs[0])
	}
	if prs[1].PR != 7 || prs[1].Runs != 2 || prs[1].LastKind != "addon" || prs[1].LastFindings == nil || *prs[1].LastFindings != 2 {
		t.Fatalf("row1 = %+v", prs[1])
	}
}
