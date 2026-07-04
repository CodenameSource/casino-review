-- P1 foundation: watermarks, job queue, experiment event spine,
-- review runs (selector signal + findings oracle), identity mapping.

CREATE TABLE kv (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Jobs decouple core (enqueue) from runner (execute). Claimed with
-- FOR UPDATE SKIP LOCKED; a job is uniquely keyed to its trigger comment so
-- redelivery can never double-spin.
CREATE TABLE jobs (
    id          BIGSERIAL PRIMARY KEY,
    kind        TEXT        NOT NULL,             -- 'spin' (P1); 'judge' (P4)
    dedup_key   TEXT        NOT NULL UNIQUE,      -- e.g. 'spin:<commentID>'
    payload     JSONB       NOT NULL,
    state       TEXT        NOT NULL DEFAULT 'queued',  -- queued|running|done|error
    error       TEXT,
    attempts    INT         NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at  TIMESTAMPTZ,
    finished_at TIMESTAMPTZ
);
CREATE INDEX jobs_state_idx ON jobs (state, created_at);

-- Append-only experiment record. Money/market events are inserted in the
-- same transaction as the state change they describe.
CREATE TABLE events (
    id          BIGSERIAL PRIMARY KEY,
    event_type  TEXT        NOT NULL,
    actor       TEXT        NOT NULL DEFAULT '',  -- 'slack:U…' | 'github:login' | 'system'
    context_ref TEXT        NOT NULL DEFAULT '',  -- 'pr:owner/repo#N' | 'ext:KEY' | ''
    payload     JSONB       NOT NULL DEFAULT '{}',
    schema_ver  INT         NOT NULL DEFAULT 1,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX events_type_idx ON events (event_type, created_at);
CREATE INDEX events_ctx_idx  ON events (context_ref, created_at);

-- One row per review-engine execution: the RCT outcome record and, later,
-- the resolution oracle for findings-count markets.
CREATE TABLE review_runs (
    id             BIGSERIAL PRIMARY KEY,
    repo           TEXT        NOT NULL,            -- owner/name
    pr             INT         NOT NULL,
    engine         TEXT        NOT NULL,            -- registry name
    engine_kind    TEXT        NOT NULL,            -- dispatch|claude|analyzer
    job_id         BIGINT REFERENCES jobs(id),
    findings_count INT,                             -- NULL = unknown (dispatch)
    findings       JSONB,
    summary        TEXT,
    comment_id     BIGINT,
    error          TEXT,
    started_at     TIMESTAMPTZ NOT NULL,
    finished_at    TIMESTAMPTZ
);
CREATE INDEX review_runs_pr_idx     ON review_runs (repo, pr, started_at);
CREATE INDEX review_runs_engine_idx ON review_runs (engine, started_at);

-- Slack <-> GitHub identity bridge; payout_address is the on-chain seam.
CREATE TABLE identities (
    slack_user_id  TEXT PRIMARY KEY,
    github_login   TEXT NOT NULL DEFAULT '',
    payout_address TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
