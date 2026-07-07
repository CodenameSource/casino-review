package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type Job struct {
	ID       int64
	Kind     string
	DedupKey string
	Payload  []byte
	Attempts int
}

// ErrDuplicateJob means a job with the same dedup key was already enqueued —
// the caller can treat the work as already accepted.
var ErrDuplicateJob = errors.New("duplicate job")

// EnqueueJob inserts a job; the dedup key makes enqueueing idempotent.
func (s *Store) EnqueueJob(ctx context.Context, kind, dedupKey string, payload any) (int64, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.Pool.QueryRow(ctx,
		`INSERT INTO jobs (kind, dedup_key, payload) VALUES ($1,$2,$3) RETURNING id`,
		kind, dedupKey, body).Scan(&id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
		return 0, ErrDuplicateJob
	}
	return id, err
}

// MaxAttempts bounds crash-retry loops: a job that keeps killing its runner
// (or keeps getting stuck) is parked as errored, not retried forever — each
// spin retry posts a fresh GIF comment, so unbounded retries spam the PR.
const MaxAttempts = 4

// ClaimJob atomically takes the oldest claimable queued job (SKIP LOCKED so
// concurrent runners never double-claim). Returns nil when the queue is empty.
func (s *Store) ClaimJob(ctx context.Context, kinds []string) (*Job, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	var j Job
	err = tx.QueryRow(ctx,
		`SELECT id, kind, dedup_key, payload, attempts FROM jobs
		 WHERE state='queued' AND kind = ANY($1) AND attempts < $2
		 ORDER BY created_at
		 FOR UPDATE SKIP LOCKED
		 LIMIT 1`, kinds, MaxAttempts).Scan(&j.ID, &j.Kind, &j.DedupKey, &j.Payload, &j.Attempts)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE jobs SET state='running', started_at=now(), attempts=attempts+1 WHERE id=$1`, j.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &j, nil
}

// FinishJob marks a claimed job done or errored. Guarded on state='running':
// if the job was requeued (judged stuck) while we were still executing, a late
// finish must not clobber the requeued row — the retry owns it now.
func (s *Store) FinishJob(ctx context.Context, id int64, jobErr error) error {
	state, msg := "done", ""
	if jobErr != nil {
		state, msg = "error", jobErr.Error()
	}
	_, err := s.Pool.Exec(ctx,
		`UPDATE jobs SET state=$2, error=NULLIF($3,''), finished_at=now()
		 WHERE id=$1 AND state='running'`, id, state, msg)
	return err
}

// PendingSpins counts spin jobs still queued or running — triggers that have
// been accepted but not yet turned into a posted review.
func (s *Store) PendingSpins(ctx context.Context) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM jobs WHERE kind='spin' AND state IN ('queued','running')`).Scan(&n)
	return n, err
}

// RequeueJob returns one running job to the queue — used when a shutdown
// (not a failure) interrupted it. Attempts already counted at claim time.
func (s *Store) RequeueJob(ctx context.Context, id int64) error {
	_, err := s.Pool.Exec(ctx,
		`UPDATE jobs SET state='queued', started_at=NULL WHERE id=$1 AND state='running'`, id)
	return err
}

// RequeueStuckJobs handles runners that died mid-job: 'running' rows older
// than maxAge go back to 'queued' while they have attempts left, else they are
// parked as errored. Re-running a spin is safe for correctness (reactions and
// dedup keys prevent double-triggering) though it may post a duplicate GIF.
func (s *Store) RequeueStuckJobs(ctx context.Context, maxAge time.Duration) (int64, error) {
	if _, err := s.Pool.Exec(ctx,
		`UPDATE jobs SET state='error', error='exceeded max attempts', finished_at=now()
		 WHERE state='running' AND started_at < now() - $1::interval AND attempts >= $2`,
		maxAge.String(), MaxAttempts); err != nil {
		return 0, err
	}
	tag, err := s.Pool.Exec(ctx,
		`UPDATE jobs SET state='queued', started_at=NULL
		 WHERE state='running' AND started_at < now() - $1::interval AND attempts < $2`,
		maxAge.String(), MaxAttempts)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
