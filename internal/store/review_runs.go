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
	// job_id and comment_id are BIGINT: the NULLIF sentinel MUST be 0::bigint,
	// not a bare 0. A bare 0 is an int4 literal and makes Postgres infer the
	// parameter as int4, overflowing on real GitHub comment IDs (~5 billion).
	err := s.Pool.QueryRow(ctx,
		`INSERT INTO review_runs
		   (repo, pr, engine, engine_kind, job_id, findings_count, findings, summary, comment_id, error, started_at, finished_at)
		 VALUES ($1,$2,$3,$4,NULLIF($5,0::bigint),$6,$7,NULLIF($8,''),NULLIF($9,0::bigint),NULLIF($10,''),$11,$12)
		 RETURNING id`,
		r.Repo, r.PR, r.Engine, r.EngineKind, r.JobID, r.FindingsCount, r.Findings,
		r.Summary, r.CommentID, r.Error, r.StartedAt, r.FinishedAt).Scan(&id)
	return id, err
}

// TrackedPR summarizes one PR casino-review has acted on.
type TrackedPR struct {
	PR           int
	Runs         int       // total review runs (spins × engines) on this PR
	LastEngine   string    // engine of the most recent run
	LastKind     string    // dispatch | claude | analyzer | addon
	LastFindings *int      // nil = unknown (dispatch) or the run errored
	LastAt       time.Time // most recent run
	LastError    string    // non-empty if the most recent run failed
}

// TrackedPRs lists the PRs in a repo that have had a casino review, most
// recently active first — the answer to "which PRs has /casino-review touched?".
func (s *Store) TrackedPRs(ctx context.Context, repo string, limit int) ([]TrackedPR, error) {
	rows, err := s.Pool.Query(ctx,
		`WITH latest AS (
		   SELECT DISTINCT ON (pr) pr, engine, engine_kind, findings_count, started_at, COALESCE(error,'') AS error
		   FROM review_runs WHERE repo=$1
		   ORDER BY pr, started_at DESC
		 ), counts AS (
		   SELECT pr, COUNT(*) AS runs FROM review_runs WHERE repo=$1 GROUP BY pr
		 )
		 SELECT l.pr, c.runs, l.engine, l.engine_kind, l.findings_count, l.started_at, l.error
		 FROM latest l JOIN counts c USING (pr)
		 ORDER BY l.started_at DESC
		 LIMIT $2`, repo, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrackedPR
	for rows.Next() {
		var t TrackedPR
		if err := rows.Scan(&t.PR, &t.Runs, &t.LastEngine, &t.LastKind, &t.LastFindings, &t.LastAt, &t.LastError); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// LastReviewRun returns the most recent finished REEL run for a repo (any PR),
// which feeds the selector's PreviousHadFindings signal. Addon (bonus) runs are
// excluded: they aren't reel assignments, and letting them shadow the previous
// reel engine would corrupt the milestone-2 weighting signal.
func (s *Store) LastReviewRun(ctx context.Context, repo string) (*ReviewRun, error) {
	var r ReviewRun
	var findingsCount *int
	err := s.Pool.QueryRow(ctx,
		`SELECT id, repo, pr, engine, engine_kind, findings_count, COALESCE(summary,''), COALESCE(error,''), started_at
		 FROM review_runs WHERE repo=$1 AND finished_at IS NOT NULL AND engine_kind <> 'addon'
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
