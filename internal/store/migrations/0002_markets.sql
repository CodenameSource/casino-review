-- P2: the market core. A market asks one question about a context (a PR or an
-- external tracker key); positions are per-participant stakes on an outcome;
-- payouts are the recorded (not yet transferred) results of resolution.

CREATE TABLE markets (
    id           BIGSERIAL PRIMARY KEY,
    kind         TEXT        NOT NULL,             -- bounty | merge-by | findings-count
    context_ref  TEXT        NOT NULL,             -- 'pr:owner/repo#N' | 'ext:KEY'
    question     TEXT        NOT NULL DEFAULT '',
    outcomes     JSONB       NOT NULL,             -- e.g. ["merged"] / ["yes","no"] / buckets
    outcome_spec JSONB       NOT NULL DEFAULT '{}',-- kind params (deadline, buckets, …)
    state        TEXT        NOT NULL DEFAULT 'OPEN', -- OPEN | LOCKED | RESOLVED | VOIDED
    resolution   JSONB,                            -- {"outcome":…, "evidence":…, "pools":…}
    created_by   TEXT        NOT NULL,             -- participant id ('slack:U…' | 'cli:…')
    locks_at     TIMESTAMPTZ,                      -- informational until the P3 sweeper
    resolves_by  TIMESTAMPTZ,                      -- expiry → void (P3 sweeper)
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at  TIMESTAMPTZ
);
CREATE INDEX markets_state_idx ON markets (state, created_at);
CREATE INDEX markets_ctx_idx   ON markets (context_ref);
-- One live bounty per context: /casino fund finds-or-creates it.
CREATE UNIQUE INDEX markets_live_bounty ON markets (kind, context_ref)
    WHERE kind = 'bounty' AND state IN ('OPEN', 'LOCKED');

-- Per-participant stakes. Kept per-row (never aggregated away): refunds need
-- them, parimutuel payouts need them, and future quadratic funding weights
-- are a pure function over them.
CREATE TABLE positions (
    id          BIGSERIAL PRIMARY KEY,
    market_id   BIGINT      NOT NULL REFERENCES markets(id),
    participant TEXT        NOT NULL,              -- 'slack:U…' | 'github:login' | 'cli:…'
    outcome     TEXT        NOT NULL,
    amount_usdc BIGINT      NOT NULL CHECK (amount_usdc > 0), -- micro-USDC
    status      TEXT        NOT NULL DEFAULT 'ACTIVE', -- ACTIVE | REFUNDED | PAID | SPENT
    payment_ref TEXT,                              -- x402 seam: pay-in proof, later
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX positions_market_idx      ON positions (market_id, status);
CREATE INDEX positions_participant_idx ON positions (participant, created_at);

-- Recorded results of resolution. MVP: recorded + notified, not transferred;
-- payee 'house' collects integer-division dust so every micro-USDC is audited.
CREATE TABLE payouts (
    id          BIGSERIAL PRIMARY KEY,
    market_id   BIGINT      NOT NULL REFERENCES markets(id),
    payee       TEXT        NOT NULL,              -- participant id | 'github:login' | 'house'
    amount_usdc BIGINT      NOT NULL CHECK (amount_usdc > 0),
    reason      TEXT        NOT NULL,              -- 'solver' | 'parimutuel-win' | 'dust'
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX payouts_market_idx ON payouts (market_id);

-- Dispute records (used from P4; schema lands with the rest of the market).
CREATE TABLE disputes (
    id         BIGSERIAL PRIMARY KEY,
    market_id  BIGINT      NOT NULL REFERENCES markets(id),
    disputer   TEXT        NOT NULL,
    reason     TEXT        NOT NULL,
    state      TEXT        NOT NULL DEFAULT 'OPEN', -- OPEN | JUDGED | NEEDS_HUMAN
    judge      TEXT,
    verdict    JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    judged_at  TIMESTAMPTZ
);
CREATE INDEX disputes_market_idx ON disputes (market_id, state);
