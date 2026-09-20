ALTER TABLE agent
    DROP COLUMN fallback_runtime_ids;

ALTER TABLE agent_runtime
    DROP COLUMN availability_checked_at,
    DROP COLUMN down_reason,
    DROP COLUMN down_until,
    DROP COLUMN dispatch_order,
    DROP COLUMN max_concurrent_tasks;
