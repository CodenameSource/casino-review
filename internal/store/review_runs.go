package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// ReviewRun is the persisted outcome of one engine execution — the RCT
// observation record and (later) the findings-count resolution oracle.
type ReviewRun struct {
	ID            int64
	Repo          string
	PR            int
	Engine        string
	EngineKind    string
	JobID         int64
	FindingsCount *int // nil = unknown (dispatch engines)
	Findings      json.RawMessage
	Summary       string
	CommentID     int64
	Error         string
	StartedAt     time.Time
	FinishedAt    time.Time
}

func (s *Store) InsertReviewRun(ctx context.Context, r ReviewRun) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx,
		`INSERT INTO review_runs
		   (repo, pr, engine, engine_kind, job_id, findings_count, findings, summary, comment_id, error, started_at, finished_at)
		 VALUES ($1,$2,$3,$4,NULLIF($5,0),$6,$7,NULLIF($8,''),NULLIF($9,0),NULLIF($10,''),$11,$12)
		 RETURNING id`,
		r.Repo, r.PR, r.Engine, r.EngineKind, r.JobID, r.FindingsCount, r.Findings,
		r.Summary, r.CommentID, r.Error, r.StartedAt, r.FinishedAt).Scan(&id)
	return id, err
}

// LastReviewRun returns the most recent finished run for a repo (any PR),
// which feeds the selector's PreviousHadFindings signal. Returns nil if none.
func (s *Store) LastReviewRun(ctx context.Context, repo string) (*ReviewRun, error) {
	var r ReviewRun
	var findingsCount *int
	err := s.Pool.QueryRow(ctx,
		`SELECT id, repo, pr, engine, engine_kind, findings_count, COALESCE(summary,''), COALESCE(error,''), started_at
		 FROM review_runs WHERE repo=$1 AND finished_at IS NOT NULL
		 ORDER BY started_at DESC LIMIT 1`, repo).
		Scan(&r.ID, &r.Repo, &r.PR, &r.Engine, &r.EngineKind, &findingsCount, &r.Summary, &r.Error, &r.StartedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.FindingsCount = findingsCount
	return &r, nil
}
