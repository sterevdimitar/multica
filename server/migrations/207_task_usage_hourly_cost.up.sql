-- 207_task_usage_hourly_cost.up.sql
--
-- Roll task_usage.total_cost_usd up into task_usage_hourly so the dashboard
-- can show the STORED cost instead of estimating one client-side from a
-- price table that does not know the pipeline's engine aliases (dcc-glm-*),
-- which is how GLM spend came to read as $0 while the card showed it at
-- 33x its real cost. See dev-command-center
-- docs/superpowers/specs/2026-09-11-engine-priced-cost-design.md.
--
-- Idempotent by rule (204's header): ADD COLUMN IF NOT EXISTS, CREATE OR
-- REPLACE, and an ON CONFLICT enqueue. Safe to re-run against a database
-- that already has all of it.
--
-- The column is NULLABLE with no default. A bucket whose source rows carry
-- no cost sums to NULL, and NULL must stay NULL: a 0 would read as "free",
-- which is a different wrong number from the one this migration fixes.
ALTER TABLE task_usage_hourly ADD COLUMN IF NOT EXISTS total_cost_usd DOUBLE PRECISION;

-- The rollup function, re-declared. This is migration 102's body VERBATIM
-- with exactly four additions, each marked `-- 207`: the aggregate in
-- `recomputed`, the column in the INSERT list, the column in its SELECT,
-- and the assignment in ON CONFLICT ... DO UPDATE SET. Diff it against 102
-- rather than reading it; nothing else moved.
CREATE OR REPLACE FUNCTION rollup_task_usage_hourly_window(
    p_from TIMESTAMPTZ,
    p_to   TIMESTAMPTZ
)
RETURNS BIGINT
LANGUAGE plpgsql
AS $$
DECLARE
    v_rows BIGINT;
BEGIN
    IF p_from >= p_to THEN
        RETURN 0;
    END IF;

    WITH
    dirty_from_updates AS (
        SELECT DISTINCT
            task_usage_hour_bucket(tu.created_at) AS bucket_hour,
            a.workspace_id                        AS workspace_id,
            atq.runtime_id                        AS runtime_id,
            atq.agent_id                          AS agent_id,
            i.project_id                          AS project_id,
            tu.provider                           AS provider,
            tu.model                              AS model
          FROM task_usage tu
          JOIN agent_task_queue atq ON atq.id      = tu.task_id
          JOIN agent            a   ON a.id        = atq.agent_id
          LEFT JOIN issue       i   ON i.id        = atq.issue_id
         WHERE atq.runtime_id IS NOT NULL
           AND (
                (tu.updated_at >= p_from AND tu.updated_at < p_to)
                -- Legacy updated_at-NULL rows; partial index from 078.
                OR (tu.updated_at IS NULL
                    AND tu.created_at >= p_from
                    AND tu.created_at <  p_to)
           )
    ),
    dirty_from_queue AS (
        SELECT bucket_hour, workspace_id, runtime_id, agent_id,
               project_id, provider, model
          FROM task_usage_hourly_dirty
         WHERE enqueued_at < p_to
    ),
    dirty_keys AS (
        SELECT * FROM dirty_from_updates
        UNION
        SELECT * FROM dirty_from_queue
    ),
    recomputed AS (
        SELECT
            dk.bucket_hour,
            dk.workspace_id,
            dk.runtime_id,
            dk.agent_id,
            dk.project_id,
            dk.provider,
            dk.model,
            SUM(tu.input_tokens)::bigint       AS input_tokens,
            SUM(tu.output_tokens)::bigint      AS output_tokens,
            SUM(tu.cache_read_tokens)::bigint  AS cache_read_tokens,
            SUM(tu.cache_write_tokens)::bigint AS cache_write_tokens,
            SUM(tu.total_cost_usd)::double precision AS total_cost_usd,   -- 207
            COUNT(DISTINCT tu.task_id)::bigint AS task_count,
            COUNT(*)::bigint                   AS event_count
          FROM dirty_keys dk
          JOIN agent_task_queue atq ON atq.runtime_id  = dk.runtime_id
                                    AND atq.agent_id    = dk.agent_id
          JOIN agent            a   ON a.id            = atq.agent_id
                                    AND a.workspace_id = dk.workspace_id
          LEFT JOIN issue       i   ON i.id            = atq.issue_id
          JOIN task_usage       tu  ON tu.task_id      = atq.id
                                    AND tu.provider    = dk.provider
                                    AND tu.model       = dk.model
                                    AND task_usage_hour_bucket(tu.created_at) = dk.bucket_hour
         WHERE (i.project_id IS NOT DISTINCT FROM dk.project_id)
         GROUP BY 1, 2, 3, 4, 5, 6, 7
    ),
    upserted AS (
        INSERT INTO task_usage_hourly AS d (
            bucket_hour, workspace_id, runtime_id, agent_id,
            project_id, provider, model,
            input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
            total_cost_usd, task_count, event_count   -- 207
        )
        SELECT
            bucket_hour, workspace_id, runtime_id, agent_id,
            project_id, provider, model,
            input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
            total_cost_usd, task_count, event_count   -- 207
          FROM recomputed
        ON CONFLICT ON CONSTRAINT uq_task_usage_hourly_key DO UPDATE
            SET input_tokens       = EXCLUDED.input_tokens,
                output_tokens      = EXCLUDED.output_tokens,
                cache_read_tokens  = EXCLUDED.cache_read_tokens,
                cache_write_tokens = EXCLUDED.cache_write_tokens,
                total_cost_usd     = EXCLUDED.total_cost_usd,   -- 207
                task_count         = EXCLUDED.task_count,
                event_count        = EXCLUDED.event_count,
                updated_at         = now()
        RETURNING 1
    ),
    deleted_empty AS (
        DELETE FROM task_usage_hourly d
         USING dirty_keys dk
         WHERE d.bucket_hour  = dk.bucket_hour
           AND d.workspace_id = dk.workspace_id
           AND d.runtime_id   = dk.runtime_id
           AND d.agent_id     = dk.agent_id
           AND d.project_id IS NOT DISTINCT FROM dk.project_id
           AND d.provider     = dk.provider
           AND d.model        = dk.model
           AND NOT EXISTS (
               SELECT 1 FROM recomputed r
                WHERE r.bucket_hour  = dk.bucket_hour
                  AND r.workspace_id = dk.workspace_id
                  AND r.runtime_id   = dk.runtime_id
                  AND r.agent_id     = dk.agent_id
                  AND r.project_id IS NOT DISTINCT FROM dk.project_id
                  AND r.provider     = dk.provider
                  AND r.model        = dk.model
           )
        RETURNING 1
    )
    SELECT (SELECT COUNT(*) FROM upserted) + (SELECT COUNT(*) FROM deleted_empty)
      INTO v_rows;

    DELETE FROM task_usage_hourly_dirty WHERE enqueued_at < p_to;

    RETURN v_rows;
END;
$$;

-- Recompute every existing bucket so history gets the new column. The
-- scheduler's next tick drains task_usage_hourly_dirty through the
-- function above; enqueued_at defaults to now() (101).
INSERT INTO task_usage_hourly_dirty (
    bucket_hour, workspace_id, runtime_id, agent_id, project_id, provider, model
)
SELECT bucket_hour, workspace_id, runtime_id, agent_id, project_id, provider, model
  FROM task_usage_hourly
ON CONFLICT ON CONSTRAINT uq_task_usage_hourly_dirty_key DO UPDATE
    SET enqueued_at = GREATEST(task_usage_hourly_dirty.enqueued_at, EXCLUDED.enqueued_at);
