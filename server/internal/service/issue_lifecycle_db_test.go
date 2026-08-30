package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// markRunningFixture provisions a workspace/user/agent/issue trio for
// MarkIssueRunning tests, with the issue at the given starting status and
// (optionally) assigned to the fixture's own agent. Returns the created
// issue row and the agent name to pass to MarkIssueRunning.
func markRunningFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, startStatus string, assign bool) (db.Issue, string) {
	t.Helper()
	suffix := time.Now().UnixNano()
	queries := db.New(pool)

	var userID string
	if err := pool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ($1,$2) RETURNING id`,
		"Mark Running Test", fmt.Sprintf("mark-running-%d@multica.ai", suffix)).Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	var workspaceID string
	if err := pool.QueryRow(ctx, `INSERT INTO workspace (name, slug, description, issue_prefix) VALUES ($1,$2,$3,$4) RETURNING id`,
		"Mark Running Test", fmt.Sprintf("mark-running-%d", suffix), "temp mark-running test", "MRT").Scan(&workspaceID); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1,$2,'owner')`, workspaceID, userID); err != nil {
		t.Fatalf("create member: %v", err)
	}
	var runtimeID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_runtime (workspace_id, daemon_id, name, runtime_mode, provider, status, device_info, metadata, last_seen_at, visibility, owner_id)
		VALUES ($1, 'daemon-mrt', 'MRT RT', 'cloud', 'mrt_provider', 'online', 'x', '{}'::jsonb, now(), 'private', $2)
		RETURNING id`, workspaceID, userID).Scan(&runtimeID); err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	agentName := fmt.Sprintf("MRT Agent %d", suffix)
	var agentID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent (workspace_id, name, description, runtime_mode, runtime_config, runtime_id, visibility, max_concurrent_tasks, owner_id)
		VALUES ($1, $2, '', 'cloud', '{}'::jsonb, $3, 'private', 5, $4)
		RETURNING id`, workspaceID, agentName, runtimeID, userID).Scan(&agentID); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	var issueID string
	if assign {
		if err := pool.QueryRow(ctx, `
			INSERT INTO issue (workspace_id, title, status, priority, creator_id, creator_type, number, position, assignee_type, assignee_id)
			VALUES ($1, 'mrt issue', $2, 'none', $3, 'member', 700001, 0, 'agent', $4)
			RETURNING id`, workspaceID, startStatus, userID, agentID).Scan(&issueID); err != nil {
			t.Fatalf("create issue: %v", err)
		}
	} else {
		if err := pool.QueryRow(ctx, `
			INSERT INTO issue (workspace_id, title, status, priority, creator_id, creator_type, number, position)
			VALUES ($1, 'mrt issue', $2, 'none', $3, 'member', 700002, 0)
			RETURNING id`, workspaceID, startStatus, userID).Scan(&issueID); err != nil {
			t.Fatalf("create issue: %v", err)
		}
	}

	t.Cleanup(func() {
		c := context.Background()
		pool.Exec(c, `DELETE FROM issue WHERE id = $1`, issueID)
		pool.Exec(c, `DELETE FROM agent WHERE id = $1`, agentID)
		pool.Exec(c, `DELETE FROM agent_runtime WHERE id = $1`, runtimeID)
		pool.Exec(c, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, workspaceID, userID)
		pool.Exec(c, `DELETE FROM workspace WHERE id = $1`, workspaceID)
		pool.Exec(c, `DELETE FROM "user" WHERE id = $1`, userID)
	})

	issue, err := queries.GetIssue(ctx, util.MustParseUUID(issueID))
	if err != nil {
		t.Fatalf("load fixture issue: %v", err)
	}
	return issue, agentName
}

func TestMarkIssueRunningPromotesTodo(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	svc := NewTaskService(db.New(pool), pool, nil, events.New())

	issue, agentName := markRunningFixture(t, ctx, pool, StatusTodo, false)
	svc.MarkIssueRunning(ctx, issue, agentName)

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM issue WHERE id = $1`, issue.ID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != StatusInProgress {
		t.Fatalf("status = %q, want %q", status, StatusInProgress)
	}
}

func TestMarkIssueRunningPromotesBlocked(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	svc := NewTaskService(db.New(pool), pool, nil, events.New())

	issue, agentName := markRunningFixture(t, ctx, pool, StatusBlocked, false)
	svc.MarkIssueRunning(ctx, issue, agentName)

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM issue WHERE id = $1`, issue.ID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != StatusInProgress {
		t.Fatalf("status = %q, want %q", status, StatusInProgress)
	}
}

func TestMarkIssueRunningLeavesDoneAlone(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	svc := NewTaskService(db.New(pool), pool, nil, events.New())

	issue, agentName := markRunningFixture(t, ctx, pool, StatusDone, false)
	svc.MarkIssueRunning(ctx, issue, agentName)

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM issue WHERE id = $1`, issue.ID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != StatusDone {
		t.Fatalf("status = %q, want unchanged %q", status, StatusDone)
	}
}

func TestMarkIssueRunningLeavesInReviewAlone(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	svc := NewTaskService(db.New(pool), pool, nil, events.New())

	issue, agentName := markRunningFixture(t, ctx, pool, StatusInReview, false)
	svc.MarkIssueRunning(ctx, issue, agentName)

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM issue WHERE id = $1`, issue.ID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != StatusInReview {
		t.Fatalf("status = %q, want unchanged %q", status, StatusInReview)
	}
}

func TestMarkIssueRunningKeepsTheAssignee(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	svc := NewTaskService(db.New(pool), pool, nil, events.New())

	issue, agentName := markRunningFixture(t, ctx, pool, StatusTodo, true)
	if !issue.AssigneeID.Valid {
		t.Fatal("fixture issue expected to start assigned")
	}
	svc.MarkIssueRunning(ctx, issue, agentName)

	var assigneeType, status string
	var assigneeID pgtype.UUID
	if err := pool.QueryRow(ctx, `SELECT status, assignee_type, assignee_id FROM issue WHERE id = $1`, issue.ID).
		Scan(&status, &assigneeType, &assigneeID); err != nil {
		t.Fatalf("read issue: %v", err)
	}
	if status != StatusInProgress {
		t.Fatalf("status = %q, want %q", status, StatusInProgress)
	}
	if assigneeType != "agent" || !assigneeID.Valid || assigneeID != issue.AssigneeID {
		t.Fatalf("assignee changed: type=%q id=%v, want unchanged from %v", assigneeType, assigneeID, issue.AssigneeID)
	}
}

func TestMarkIssueRunningPublishesIssueUpdated(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	bus := events.New()
	svc := NewTaskService(db.New(pool), pool, nil, bus)

	issue, agentName := markRunningFixture(t, ctx, pool, StatusTodo, false)

	var got events.Event
	seen := false
	bus.Subscribe("issue:updated", func(e events.Event) {
		seen = true
		got = e
	})

	svc.MarkIssueRunning(ctx, issue, agentName)

	if !seen {
		t.Fatal("expected an issue:updated event to be published")
	}
	if got.WorkspaceID != util.UUIDToString(issue.WorkspaceID) {
		t.Fatalf("event workspace_id = %q, want %q", got.WorkspaceID, util.UUIDToString(issue.WorkspaceID))
	}
	payload, ok := got.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T, want map[string]any", got.Payload)
	}
	if changed, _ := payload["status_changed"].(bool); !changed {
		t.Fatalf("status_changed = %v, want true", payload["status_changed"])
	}
	if prev, _ := payload["prev_status"].(string); prev != StatusTodo {
		t.Fatalf("prev_status = %q, want %q", prev, StatusTodo)
	}
}

// TestDispatchProceedsWhenTheStatusWriteFails proves the invariant that the
// in_progress status write is a side effect of dispatch, never a
// precondition: dispatchWebhookTask must still transition the task to
// `dispatched` even when MarkIssueRunning's own UPDATE of issue.status fails.
// The fixture is entirely valid (no FK games) - it forces the failure with a
// trigger, scoped by a WHEN clause to only the fixture's own issue row, that
// raises on the real UPDATE issue SET status = ... statement.
func TestDispatchProceedsWhenTheStatusWriteFails(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	svc := NewTaskService(db.New(pool), pool, nil, events.New())
	queries := db.New(pool)
	suffix := time.Now().UnixNano()

	var userID string
	if err := pool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ($1,$2) RETURNING id`,
		"Dispatch Survives Test", fmt.Sprintf("dispatch-survives-%d@multica.ai", suffix)).Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, userID)
	})

	var workspaceID string
	if err := pool.QueryRow(ctx, `INSERT INTO workspace (name, slug, description, issue_prefix) VALUES ($1,$2,$3,$4) RETURNING id`,
		"Dispatch Survives Test", fmt.Sprintf("dispatch-survives-%d", suffix), "temp dispatch-survives test", "DST").Scan(&workspaceID); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, workspaceID)
	})

	if _, err := pool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1,$2,'owner')`, workspaceID, userID); err != nil {
		t.Fatalf("create member: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, workspaceID, userID)
	})

	var runtimeID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_runtime (workspace_id, daemon_id, name, runtime_mode, provider, status, device_info, metadata, last_seen_at, visibility, owner_id, webhook_url, webhook_secret, webhook_event_type)
		VALUES ($1, 'daemon-dst', 'DST RT', 'webhook', 'dst_provider', 'online', 'x', '{}'::jsonb, now(), 'private', $2, 'http://127.0.0.1:1/unreachable', 'dst-secret', 'task.dispatched')
		RETURNING id`, workspaceID, userID).Scan(&runtimeID); err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM agent_runtime WHERE id = $1`, runtimeID)
	})

	var agentID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent (workspace_id, name, description, runtime_mode, runtime_config, runtime_id, visibility, max_concurrent_tasks, owner_id)
		VALUES ($1, 'DST Agent', '', 'cloud', '{}'::jsonb, $2, 'private', 5, $3)
		RETURNING id`, workspaceID, runtimeID, userID).Scan(&agentID); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM agent WHERE id = $1`, agentID)
	})

	var issueID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, title, status, priority, creator_id, creator_type, number, position)
		VALUES ($1, 'dispatch survives issue', 'todo', 'none', $2, 'member', 700003, 0)
		RETURNING id`, workspaceID, userID).Scan(&issueID); err != nil {
		t.Fatalf("create issue: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
	})

	// Force the real UPDATE issue SET status = ... to fail, scoped by a WHEN
	// clause to exactly this fixture's issue row so it cannot affect anything
	// else in the shared database.
	triggerName := fmt.Sprintf("dispatch_survives_status_fail_%d", suffix)
	functionName := triggerName + "_fn"
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION %s()
		RETURNS trigger
		LANGUAGE plpgsql
		AS $$
		BEGIN
			RAISE EXCEPTION 'dispatch_survives: forced status write failure';
		END;
		$$;
	`, quoteIdent(functionName))); err != nil {
		t.Fatalf("create status-fail trigger function: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", quoteIdent(functionName)))
	})
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TRIGGER %s
		BEFORE UPDATE OF status ON issue
		FOR EACH ROW
		WHEN (NEW.id = %s::uuid)
		EXECUTE FUNCTION %s();
	`, quoteIdent(triggerName), quoteLiteral(issueID), quoteIdent(functionName))); err != nil {
		t.Fatalf("create status-fail trigger: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON issue", quoteIdent(triggerName)))
	})

	var taskID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, context)
		VALUES ($1, $2, $3, 'queued', 0, '{}'::jsonb)
		RETURNING id`, agentID, runtimeID, issueID).Scan(&taskID); err != nil {
		t.Fatalf("create queued task: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID)
	})

	task, err := queries.GetAgentTask(ctx, util.MustParseUUID(taskID))
	if err != nil {
		t.Fatalf("load task: %v", err)
	}
	runtime, err := queries.GetAgentRuntime(ctx, util.MustParseUUID(runtimeID))
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	agent, err := queries.GetAgent(ctx, util.MustParseUUID(agentID))
	if err != nil {
		t.Fatalf("load agent: %v", err)
	}

	svc.dispatchWebhookTask(ctx, task, runtime, agent)

	var taskStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM agent_task_queue WHERE id = $1`, taskID).Scan(&taskStatus); err != nil {
		t.Fatalf("read task status: %v", err)
	}
	if taskStatus != "dispatched" {
		t.Fatalf("task status = %q, want dispatched despite the status-write failure", taskStatus)
	}

	var issueStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM issue WHERE id = $1`, issueID).Scan(&issueStatus); err != nil {
		t.Fatalf("read issue status: %v", err)
	}
	if issueStatus != StatusTodo {
		t.Fatalf("issue status = %q, want unchanged %q since the write genuinely failed", issueStatus, StatusTodo)
	}
}
