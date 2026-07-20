-- 202_webhook_runtime: allow runtime_mode='webhook' with dispatch fields.
--
-- A webhook runtime receives tasks via server-initiated HTTP POST instead of
-- daemon polling. See server/docs/webhook-runtime.md for the protocol; design
-- note in docs/webhook-runtime-design.md.
--
-- The upstream runtime_mode CHECK (migrations 001/004) limits values to
-- ('local', 'cloud'); we drop it dynamically (its auto-generated name has
-- varied across history) and re-add the expanded set.
--
-- IDEMPOTENT BY DESIGN — do not "simplify" the IF NOT EXISTS / IF EXISTS away.
-- This migration was numbered 079 before the v0.4.4 rebase (2026-07-20). Any
-- database that ran the pre-rebase fork already has these columns and
-- constraints from the old 079, while `079_webhook_runtime` remains recorded
-- in schema_migrations under a version string that no longer maps to a file.
-- The migrator is file-driven, so THIS file will still run against such a
-- database and must not fail on "already exists". Both the local dev DB and
-- the GCP production DB are in exactly that state.

-- Drop the existing CHECK on runtime_mode regardless of how Postgres has
-- normalized the definition (IN-list vs. ANY(ARRAY[...])) and regardless of
-- any historical renames. Note this also matches agent_runtime_webhook_url_
-- required, whose definition mentions runtime_mode — it is re-added below.
DO $$
DECLARE
    cname text;
BEGIN
    FOR cname IN
        SELECT conname
        FROM pg_constraint
        WHERE conrelid = 'agent_runtime'::regclass
          AND contype  = 'c'
          AND pg_get_constraintdef(oid) ILIKE '%runtime_mode%'
    LOOP
        EXECUTE 'ALTER TABLE agent_runtime DROP CONSTRAINT ' || quote_ident(cname);
    END LOOP;
END $$;

ALTER TABLE agent_runtime
    ADD CONSTRAINT agent_runtime_runtime_mode_check
        CHECK (runtime_mode IN ('local', 'cloud', 'webhook'));

ALTER TABLE agent_runtime
    ADD COLUMN IF NOT EXISTS webhook_url        TEXT,
    ADD COLUMN IF NOT EXISTS webhook_secret     TEXT,
    ADD COLUMN IF NOT EXISTS webhook_event_type TEXT;

-- A webhook runtime is useless without a destination URL; enforce it.
-- Dropped first so a re-run — or an upgrade from the old 079 — can't collide.
ALTER TABLE agent_runtime
    DROP CONSTRAINT IF EXISTS agent_runtime_webhook_url_required;

ALTER TABLE agent_runtime
    ADD CONSTRAINT agent_runtime_webhook_url_required
        CHECK (runtime_mode <> 'webhook' OR webhook_url IS NOT NULL);
