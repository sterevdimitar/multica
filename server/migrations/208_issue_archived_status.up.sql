-- 208_issue_archived_status.up.sql
-- An eighth issue status, `archived`: terminal (excluded from every "open"
-- read the way done/cancelled are) and inert (no trigger dispatches a run
-- on it — see internal/service/issue_trigger.go and handler/comment.go).
-- Entered by a human or by the archive sweeper (cmd/server/
-- issue_archive_sweeper.go) once a card has sat in done/cancelled for
-- ISSUE_ARCHIVE_AFTER; left only by a human. The constraint is the only
-- schema change: no column, no index (idx_issue_status already covers the
-- sweeper's (workspace_id, status) scan), no foreign key.
--
-- Idempotent by rule (lessons.md §renumbering migrations): DROP … IF EXISTS
-- so re-running against a database where this already applied succeeds.
ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_status_check;
ALTER TABLE issue ADD CONSTRAINT issue_status_check
    CHECK (status IN ('backlog', 'todo', 'in_progress', 'in_review', 'done', 'blocked', 'cancelled', 'archived'));
