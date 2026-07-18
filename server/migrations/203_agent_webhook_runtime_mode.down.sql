-- Reverse 080_agent_webhook_runtime_mode.

UPDATE agent SET runtime_mode = 'local' WHERE runtime_mode = 'webhook';

ALTER TABLE agent
    DROP CONSTRAINT IF EXISTS agent_runtime_mode_check;

ALTER TABLE agent
    ADD CONSTRAINT agent_runtime_mode_check
        CHECK (runtime_mode IN ('local', 'cloud'));
