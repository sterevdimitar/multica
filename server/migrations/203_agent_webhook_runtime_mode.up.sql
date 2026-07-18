-- 080_agent_webhook_runtime_mode: extend agent.runtime_mode CHECK to include 'webhook'.
--
-- Migration 079 added 'webhook' to agent_runtime.runtime_mode_check but left
-- the separate CHECK on the agent table at ('local', 'cloud'). Creating an
-- agent bound to a webhook runtime fails with agent_runtime_mode_check
-- (SQLSTATE 23514). This was patched live on the GCP DB via ALTER; this
-- migration makes it survive a from-scratch rebuild.

DO $$
DECLARE
    cname text;
BEGIN
    FOR cname IN
        SELECT conname
        FROM pg_constraint
        WHERE conrelid = 'agent'::regclass
          AND contype  = 'c'
          AND pg_get_constraintdef(oid) ILIKE '%runtime_mode%'
    LOOP
        EXECUTE 'ALTER TABLE agent DROP CONSTRAINT ' || quote_ident(cname);
    END LOOP;
END $$;

ALTER TABLE agent
    ADD CONSTRAINT agent_runtime_mode_check
        CHECK (runtime_mode IN ('local', 'cloud', 'webhook'));
