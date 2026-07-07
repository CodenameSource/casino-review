-- Context-first addressing: a market is named by its context + kind
-- (e.g. "#123 merge-by"), so that pair must resolve to exactly one live market.
-- Generalize the bounty-only uniqueness to every kind: at most one OPEN/LOCKED
-- market per (kind, context_ref). `open`/`fund` become find-or-create; a second
-- attempt returns the existing market instead of spawning a duplicate.
--
-- Before building the index, defuse any pre-existing duplicate live markets
-- (only parimutuel kinds could have them — bounty was already unique). Keep the
-- newest market per (kind, context_ref); void the older ones and refund their
-- active positions (off-chain: a status flip returns the stake), so the index
-- can be built without a crash-loop and no money is stranded. This whole file
-- runs in one transaction.

CREATE TEMP TABLE _dupe_markets ON COMMIT DROP AS
SELECT id FROM (
    SELECT id, row_number() OVER (
        PARTITION BY kind, context_ref ORDER BY created_at DESC, id DESC
    ) AS rn
    FROM markets
    WHERE state IN ('OPEN', 'LOCKED')
) ranked
WHERE rn > 1;

UPDATE positions SET status = 'REFUNDED', updated_at = now()
    WHERE market_id IN (SELECT id FROM _dupe_markets) AND status = 'ACTIVE';

UPDATE markets SET state = 'VOIDED', updated_at = now(), resolved_at = now()
    WHERE id IN (SELECT id FROM _dupe_markets);

DROP INDEX IF EXISTS markets_live_bounty;
CREATE UNIQUE INDEX markets_live_unique ON markets (kind, context_ref)
    WHERE state IN ('OPEN', 'LOCKED');
