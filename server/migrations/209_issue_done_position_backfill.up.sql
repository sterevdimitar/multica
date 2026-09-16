-- 209_issue_done_position_backfill.up.sql
--
-- One-off re-rank of the `done` column, per workspace, newest first. Until
-- the status-writing queries learned to place a moved card at the top of its
-- new column (issue.sql, UpdateIssue), every card reaching `done` kept the
-- position it had in its old column and landed anywhere. `updated_at` is the
-- closest thing to "when it was moved": every status write sets it.
--
-- Positions become 1, 2, 3, ...; the next card to land computes MIN - 1 = 0,
-- then -1, and so on, so the two schemes never collide. Only `done` is
-- touched: `todo` may carry a deliberate hand order that updated_at is not
-- good enough to overwrite, and `cancelled` is not on the default board.
-- updated_at is deliberately NOT written here - it is the ranking key.
WITH ranked AS (
    SELECT id,
           row_number() OVER (PARTITION BY workspace_id
                              ORDER BY updated_at DESC, number DESC) AS rn
    FROM issue
    WHERE status = 'done'
)
UPDATE issue SET position = ranked.rn
FROM ranked
WHERE issue.id = ranked.id;
