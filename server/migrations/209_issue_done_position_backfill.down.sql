-- 209_issue_done_position_backfill.down.sql
-- No-op: positions are best-effort ordering and the pre-backfill order was
-- accidental - there is nothing worth restoring.
SELECT 1;
