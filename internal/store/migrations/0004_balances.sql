-- B-1: per-participant spendable USDC balance — the custodial accounting plane.
-- Betting debits it, winnings/refunds credit it, deposits credit it, cash-outs
-- debit it. Off-chain and authoritative; the chain (later) is reconciled to it.
-- A row is created on first credit; absence == balance 0. The CHECK makes an
-- overdraw impossible at the storage layer, backing the guarded debit UPDATE.
CREATE TABLE balances (
    participant TEXT        PRIMARY KEY,             -- 'slack:U…' | 'github:login' | 'cli:…' | 'house'
    amount_usdc BIGINT      NOT NULL DEFAULT 0 CHECK (amount_usdc >= 0), -- micro-USDC
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
