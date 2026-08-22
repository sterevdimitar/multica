-- 204_task_usage_turns_duration.up.sql
-- Idempotent by rule (renumbering migrations across a rebase re-runs a file
-- against a DB where its DDL already applied): this file must survive
-- re-running against a database that already has these columns.
ALTER TABLE task_usage ADD COLUMN IF NOT EXISTS num_turns BIGINT NOT NULL DEFAULT 0;
ALTER TABLE task_usage ADD COLUMN IF NOT EXISTS duration_ms BIGINT;
ALTER TABLE task_usage ADD COLUMN IF NOT EXISTS duration_api_ms BIGINT;
ALTER TABLE task_usage ADD COLUMN IF NOT EXISTS total_cost_usd DOUBLE PRECISION;
