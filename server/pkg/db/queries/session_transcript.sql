-- name: InsertSessionTranscript :one
-- Append-only: a second upload for the same task inserts a second row rather
-- than replacing the first. Newest-wins on read makes that harmless, and it
-- keeps the archive a faithful record of what each run actually produced.
INSERT INTO agent_session_transcript (
  task_id, issue_id, agent_id, session_id, content, content_bytes
) VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetLatestSessionTranscriptForAgentIssue :one
-- The newest archived transcript for one (agent, issue) pair. Resume is
-- same-agent-same-issue only, which is Multica's own continuity model.
SELECT * FROM agent_session_transcript
WHERE agent_id = $1 AND issue_id = $2
ORDER BY created_at DESC
LIMIT 1;
