-- 206_issue_pull_request_metadata_index.up.sql
-- Idempotent by rule (lessons.md §renumbering migrations): this file must
-- survive re-running against a DB where its old number already applied.
CREATE INDEX IF NOT EXISTS idx_issue_autopilot_pull_request
    ON issue ((metadata ->> 'pull_request'))
    WHERE origin_type = 'autopilot' AND metadata ->> 'pull_request' IS NOT NULL;
