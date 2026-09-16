-- 208_issue_archived_status.down.sql
-- Convert before constraining: an archived card becomes cancelled (the
-- nearest terminal status) so the seven-value constraint can be restored
-- without failing, and an image rollback can follow this schema rollback.
UPDATE issue SET status = 'cancelled', updated_at = now() WHERE status = 'archived';
ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_status_check;
ALTER TABLE issue ADD CONSTRAINT issue_status_check
    CHECK (status IN ('backlog', 'todo', 'in_progress', 'in_review', 'done', 'blocked', 'cancelled'));
