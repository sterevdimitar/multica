-- 079_webhook_runtime: allow runtime_mode='webhook' with dispatch fields.
--
-- A webhook runtime receives tasks via server-initiated HTTP POST instead of
-- daemon polling. See server/docs/webhook-runtime.md (added in a later commit
-- of this branch) for the protocol; design note in docs/webhook-runtime-design.md.
--
-- The existing runtime_mode CHECK from migration 004 limits values to
-- ('local', 'cloud'); we drop it dynamically (its auto-generated name has
-- varied across history) and re-add the expanded set.

-- Drop the existing CHECK on runtime_mode regardless of how Postgres
-- has normalized the definition (IN-list vs. ANY(ARRAY[...])) and
-- regardless of any historical renames.
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
    ADD COLUMN webhook_url        TEXT,
    ADD COLUMN webhook_secret     TEXT,
    ADD COLUMN webhook_event_type TEXT;

-- A webhook runtime is useless without a destination URL; enforce it.
ALTER TABLE agent_runtime
    ADD CONSTRAINT agent_runtime_webhook_url_required
        CHECK (runtime_mode <> 'webhook' OR webhook_url IS NOT NULL);
