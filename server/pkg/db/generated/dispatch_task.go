package db

// Hand-written query matching DispatchAgentTask in agent.sql.
// Kept in a separate file so sqlc generate does not overwrite it.
// Re-run sqlc generate to fold this into agent.sql.go, then delete this file.

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
)

const dispatchAgentTask = `-- name: DispatchAgentTask :one
UPDATE agent_task_queue
SET status = 'dispatched', dispatched_at = now()
WHERE id = $1 AND status = 'queued'
RETURNING id, agent_id, issue_id, status, priority, dispatched_at, started_at, completed_at, result, error, created_at, context, runtime_id, session_id, work_dir, trigger_comment_id, chat_session_id, autopilot_run_id, attempt, max_attempts, parent_task_id, failure_reason, trigger_summary, force_fresh_session
`

func (q *Queries) DispatchAgentTask(ctx context.Context, id pgtype.UUID) (AgentTaskQueue, error) {
	row := q.db.QueryRow(ctx, dispatchAgentTask, id)
	var i AgentTaskQueue
	err := row.Scan(
		&i.ID,
		&i.AgentID,
		&i.IssueID,
		&i.Status,
		&i.Priority,
		&i.DispatchedAt,
		&i.StartedAt,
		&i.CompletedAt,
		&i.Result,
		&i.Error,
		&i.CreatedAt,
		&i.Context,
		&i.RuntimeID,
		&i.SessionID,
		&i.WorkDir,
		&i.TriggerCommentID,
		&i.ChatSessionID,
		&i.AutopilotRunID,
		&i.Attempt,
		&i.MaxAttempts,
		&i.ParentTaskID,
		&i.FailureReason,
		&i.TriggerSummary,
		&i.ForceFreshSession,
	)
	return i, err
}
