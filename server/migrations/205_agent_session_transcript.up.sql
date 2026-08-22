-- 205_agent_session_transcript.up.sql
-- The lossless per-run session archive.
--
-- A claude session is a client-side JSONL under ~/.claude/projects/. On an
-- ephemeral runner that file dies with the job, so the session id survives
-- while the conversation it names does not. This table is where the
-- conversation goes instead, uploaded over the per-task daemon callback — the
-- one channel every substrate must already speak, so resume survives a move
-- off GitHub Actions.
--
-- It doubles as the lossless analytics record: it keeps the thinking blocks
-- and exact structure the task-message display stream drops. The two are
-- deliberately NOT merged; task_message stays a display stream.
--
-- Append-only, one row per run. Resume reads the newest row for an
-- (agent_id, issue_id) pair, which is why the index is on that pair with
-- created_at descending.
--
-- Idempotent by rule (renumbering migrations across a rebase re-runs a file
-- against a DB where its DDL already applied).
CREATE TABLE IF NOT EXISTS agent_session_transcript (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  task_id       UUID NOT NULL,
  issue_id      UUID NOT NULL,
  agent_id      UUID NOT NULL,
  session_id    TEXT NOT NULL,
  content       BYTEA NOT NULL,          -- gzip, stored exactly as received
  content_bytes INTEGER NOT NULL,        -- compressed size actually stored
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_session_transcript_pair
  ON agent_session_transcript (agent_id, issue_id, created_at DESC);
