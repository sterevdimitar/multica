-- Reverse 079_webhook_runtime.
--
-- Note: rows with runtime_mode='webhook' would violate the restored CHECK
-- below. Down-migration deletes them rather than fail; if you need to keep
-- them, mark them 'local' before running this.

DELETE FROM agent_runtime WHERE runtime_mode = 'webhook';

ALTER TABLE agent_runtime
    DROP CONSTRAINT IF EXISTS agent_runtime_webhook_url_required;

ALTER TABLE agent_runtime
    DROP CONSTRAINT IF EXISTS agent_runtime_runtime_mode_check;

ALTER TABLE agent_runtime
    DROP COLUMN IF EXISTS webhook_event_type,
    DROP COLUMN IF EXISTS webhook_secret,
    DROP COLUMN IF EXISTS webhook_url;

-- Restore the original ('local', 'cloud') CHECK from migration 004.
ALTER TABLE agent_runtime
    ADD CONSTRAINT agent_runtime_runtime_mode_check
        CHECK (runtime_mode IN ('local', 'cloud'));
