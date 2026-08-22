ALTER TABLE task_usage DROP COLUMN IF EXISTS total_cost_usd;
ALTER TABLE task_usage DROP COLUMN IF EXISTS duration_api_ms;
ALTER TABLE task_usage DROP COLUMN IF EXISTS duration_ms;
ALTER TABLE task_usage DROP COLUMN IF EXISTS num_turns;
