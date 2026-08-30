package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// lifecycleFixtureOpts configures newLifecycleFixture. prefix seeds the
// user/workspace/agent names, slug and issue number so concurrent tests never
// collide; assign controls whether the issue starts with an assignee;
// webhook selects the runtime mode (webhook-mode runtimes additionally get
// webhook_url/webhook_secret/webhook_event_type, which the dispatch test
// needs to actually deliver).
type lifecycleFixtureOpts struct {
	prefix  string
	number  int32
	assign  bool
	webhook bool
}

// newLifecycleFixture provisions a workspace/user/agent/runtime/issue set
// shared by the MarkIssueRunning, FailTask/MarkIssueBlocked and webhook
// dispatch DB tests. It differs from test to test only in the issue's
// starting status, whether it starts assigned, and whether the runtime is
// cloud- or webhook-mode - everything else (DDL, column lists, cleanup) was
// previously duplicated three times and is now written once. Returns the
// loaded issue row plus the agent name/id and runtime id, so callers that
// need to create their own task rows (webhook dispatch) or use the shared
// createLifecycleTask helper (FailTask) both have what they need.
func newLifecycleFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, opts lifecycleFixtureOpts, startStatus string) (issue db.Issue, agentName, agentID, runtimeID string) {
	t.Helper()
	suffix := time.Now().UnixNano()
	queries := db.New(pool)

	var userID string
	if err := pool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ($1,$2) RETURNING id`,
		opts.prefix+" Test", fmt.Sprintf("%s-%d@multica.ai", opts.prefix, suffix)).Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	var workspaceID string
	if err := pool.QueryRow(ctx, `INSERT INTO workspace (name, slug, description, issue_prefix) VALUES ($1,$2,$3,$4) RETURNING id`,
		opts.prefix+" Test", fmt.Sprintf("%s-%d", opts.prefix, suffix), "temp "+opts.prefix+" test", opts.prefix).Scan(&workspaceID); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1,$2,'owner')`, workspaceID, userID); err != nil {
		t.Fatalf("create member: %v", err)
	}

	if opts.webhook {
		if err := pool.QueryRow(ctx, `
			INSERT INTO agent_runtime (workspace_id, daemon_id, name, runtime_mode, provider, status, device_info, metadata, last_seen_at, visibility, owner_id, webhook_url, webhook_secret, webhook_event_type)
			VALUES ($1, $2, $3, 'webhook', $4, 'online', 'x', '{}'::jsonb, now(), 'private', $5, 'http://127.0.0.1:1/unreachable', $6, 'task.dispatched')
			RETURNING id`, workspaceID, "daemon-"+opts.prefix, opts.prefix+" RT", opts.prefix+"_provider", userID, opts.prefix+"-secret").Scan(&runtimeID); err != nil {
			t.Fatalf("create runtime: %v", err)
		}
	} else {
		if err := pool.QueryRow(ctx, `
			INSERT INTO agent_runtime (workspace_id, daemon_id, name, runtime_mode, provider, status, device_info, metadata, last_seen_at, visibility, owner_id)
			VALUES ($1, $2, $3, 'cloud', $4, 'online', 'x', '{}'::jsonb, now(), 'private', $5)
			RETURNING id`, workspaceID, "daemon-"+opts.prefix, opts.prefix+" RT", opts.prefix+"_provider", userID).Scan(&runtimeID); err != nil {
			t.Fatalf("create runtime: %v", err)
		}
	}

	agentName = fmt.Sprintf("%s Agent %d", opts.prefix, suffix)
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent (workspace_id, name, description, runtime_mode, runtime_config, runtime_id, visibility, max_concurrent_tasks, owner_id)
		VALUES ($1, $2, '', 'cloud', '{}'::jsonb, $3, 'private', 5, $4)
		RETURNING id`, workspaceID, agentName, runtimeID, userID).Scan(&agentID); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	var issueID string
	if opts.assign {
		if err := pool.QueryRow(ctx, `
			INSERT INTO issue (workspace_id, title, status, priority, creator_id, creator_type, number, position, assignee_type, assignee_id)
			VALUES ($1, $2, $3, 'none', $4, 'member', $5, 0, 'agent', $6)
			RETURNING id`, workspaceID, opts.prefix+" issue", startStatus, userID, opts.number, agentID).Scan(&issueID); err != nil {
			t.Fatalf("create issue: %v", err)
		}
	} else {
		if err := pool.QueryRow(ctx, `
			INSERT INTO issue (workspace_id, title, status, priority, creator_id, creator_type, number, position)
			VALUES ($1, $2, $3, 'none', $4, 'member', $5, 0)
			RETURNING id`, workspaceID, opts.prefix+" issue", startStatus, userID, opts.number).Scan(&issueID); err != nil {
			t.Fatalf("create issue: %v", err)
		}
	}

	t.Cleanup(func() {
		c := context.Background()
		pool.Exec(c, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
		pool.Exec(c, `DELETE FROM issue WHERE id = $1`, issueID)
		pool.Exec(c, `DELETE FROM agent WHERE id = $1`, agentID)
		pool.Exec(c, `DELETE FROM agent_runtime WHERE id = $1`, runtimeID)
		pool.Exec(c, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, workspaceID, userID)
		pool.Exec(c, `DELETE FROM workspace WHERE id = $1`, workspaceID)
		pool.Exec(c, `DELETE FROM "user" WHERE id = $1`, userID)
	})

	loaded, err := queries.GetIssue(ctx, util.MustParseUUID(issueID))
	if err != nil {
		t.Fatalf("load fixture issue: %v", err)
	}
	return loaded, agentName, agentID, runtimeID
}

// createLifecycleTask inserts an agent_task_queue row for the given
// agent/runtime/issue. attempt/maxAttempts default to 1/2 - the values the
// auto-retry test (TestFailTaskDoesNotBlockWhenAutoRetryIsPending) needs to
// have exactly one attempt left, so FailTask's retry-eligibility check finds
// budget remaining. Every other test's assertions are insensitive to these
// two values, so they share the same default rather than each test wiring
// its own.
func createLifecycleTask(t *testing.T, ctx context.Context, pool *pgxpool.Pool, agentID, runtimeID, issueID, taskStatus string) string {
	t.Helper()
	const attempt, maxAttempts = 1, 2
	var taskID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, context, attempt, max_attempts)
		VALUES ($1, $2, $3, $4, 0, '{}'::jsonb, $5, $6)
		RETURNING id`, agentID, runtimeID, issueID, taskStatus, attempt, maxAttempts).Scan(&taskID); err != nil {
		t.Fatalf("create task: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID)
	})
	return taskID
}

// installStatusFailTrigger makes the real UPDATE issue SET status = ...
// statement fail for exactly the given issue row, so tests can prove a
// caller survives that failure. Uses a fixed trigger/function name (not one
// suffixed with UnixNano) and drops any leftover before creating: a
// UnixNano-suffixed name is only ever removed by t.Cleanup, so a Ctrl-C, a
// -timeout kill, or a panic elsewhere leaves the trigger on the shared dev
// database permanently, accumulating one per aborted run. The fixed name
// plus DROP ... IF EXISTS makes each run self-healing regardless of how the
// last one ended. The WHEN clause still scopes the trigger to one row so a
// concurrent run touching other issues is unaffected.
func installStatusFailTrigger(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name, issueID string) {
	t.Helper()
	triggerName := "lifecycle_status_fail_" + name
	functionName := triggerName + "_fn"

	drop := func() {
		pool.Exec(context.Background(), fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON issue", quoteIdent(triggerName)))
		pool.Exec(context.Background(), fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", quoteIdent(functionName)))
	}
	drop()
	t.Cleanup(drop)

	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION %s()
		RETURNS trigger
		LANGUAGE plpgsql
		AS $$
		BEGIN
			RAISE EXCEPTION 'lifecycle_status_fail: forced status write failure';
		END;
		$$;
	`, quoteIdent(functionName))); err != nil {
		t.Fatalf("create status-fail trigger function: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TRIGGER %s
		BEFORE UPDATE OF status ON issue
		FOR EACH ROW
		WHEN (NEW.id = %s::uuid)
		EXECUTE FUNCTION %s();
	`, quoteIdent(triggerName), quoteLiteral(issueID), quoteIdent(functionName))); err != nil {
		t.Fatalf("create status-fail trigger: %v", err)
	}
}

// installCommentBeforeAssigneeClearTrigger makes an INSERT into comment fail
// for the given issue if, at the moment of insert, the issue still has a
// non-NULL assignee_id. It exists to prove MarkIssueBlocked's comment lands
// strictly after the status-write's assignee-clear, not merely that both are
// true by the time the test looks at end state - if the comment insert were
// ever moved ahead of the unassign, this trigger fires and the insert (and
// so the test) fails. Same shape as installStatusFailTrigger: fixed names,
// DROP ... IF EXISTS before CREATE, a row-scoped WHEN clause, drops
// registered in t.Cleanup.
func installCommentBeforeAssigneeClearTrigger(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name, issueID string) {
	t.Helper()
	triggerName := "lifecycle_comment_assignee_" + name
	functionName := triggerName + "_fn"

	drop := func() {
		pool.Exec(context.Background(), fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON comment", quoteIdent(triggerName)))
		pool.Exec(context.Background(), fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", quoteIdent(functionName)))
	}
	drop()
	t.Cleanup(drop)

	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION %s()
		RETURNS trigger
		LANGUAGE plpgsql
		AS $$
		DECLARE
			still_assigned uuid;
		BEGIN
			SELECT assignee_id INTO still_assigned FROM issue WHERE id = NEW.issue_id;
			IF still_assigned IS NOT NULL THEN
				RAISE EXCEPTION 'lifecycle_comment_assignee: comment inserted before assignee was cleared';
			END IF;
			RETURN NEW;
		END;
		$$;
	`, quoteIdent(functionName))); err != nil {
		t.Fatalf("create comment-before-unassign trigger function: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TRIGGER %s
		BEFORE INSERT ON comment
		FOR EACH ROW
		WHEN (NEW.issue_id = %s::uuid)
		EXECUTE FUNCTION %s();
	`, quoteIdent(triggerName), quoteLiteral(issueID), quoteIdent(functionName))); err != nil {
		t.Fatalf("create comment-before-unassign trigger: %v", err)
	}
}

// countCommentsForIssue returns the number of comment rows for issueID whose
// content contains substr ("" matches every comment). Used instead of a bare
// count(*) where a test needs to isolate MarkIssueBlocked's own comment from
// FailTask's separate error comment on the same issue.
func countCommentsForIssue(t *testing.T, ctx context.Context, pool *pgxpool.Pool, issueID, substr string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM comment WHERE issue_id = $1 AND content LIKE '%' || $2 || '%'`, issueID, substr).Scan(&count); err != nil {
		t.Fatalf("count comments: %v", err)
	}
	return count
}

func TestBlockedCommentNamesTheReason(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	svc := NewTaskService(db.New(pool), pool, nil, events.New())

	issue, _, agentID, _ := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "MBC", number: 700010, assign: true}, StatusInProgress)

	svc.MarkIssueBlocked(ctx, issue, util.MustParseUUID(agentID), "config-drift")

	if got := countCommentsForIssue(t, ctx, pool, util.UUIDToString(issue.ID), "config-drift"); got != 1 {
		t.Fatalf("comments mentioning config-drift = %d, want 1", got)
	}
}

func TestBlockedCommentIsPostedAfterTheAssigneeIsCleared(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	svc := NewTaskService(db.New(pool), pool, nil, events.New())

	issue, _, agentID, _ := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "MBC", number: 700011, assign: true}, StatusInProgress)
	issueID := util.UUIDToString(issue.ID)
	if !issue.AssigneeID.Valid {
		t.Fatal("fixture issue expected to start assigned")
	}

	installCommentBeforeAssigneeClearTrigger(t, ctx, pool, "ordering", issueID)

	svc.MarkIssueBlocked(ctx, issue, util.MustParseUUID(agentID), "config-drift")

	// If the comment insert had raced ahead of the assignee-clear, the
	// trigger above would have raised and createAgentComment's swallowed
	// error would leave zero comments behind. Its presence here proves the
	// insert happened - and thus ran - only once the issue's assignee_id
	// was already NULL.
	if got := countCommentsForIssue(t, ctx, pool, issueID, ""); got != 1 {
		t.Fatalf("comment count = %d, want 1 (comment must be posted only after the assignee is cleared)", got)
	}

	var assigneeID pgtype.UUID
	if err := pool.QueryRow(ctx, `SELECT assignee_id FROM issue WHERE id = $1`, issueID).Scan(&assigneeID); err != nil {
		t.Fatalf("read issue assignee: %v", err)
	}
	if assigneeID.Valid {
		t.Fatalf("assignee_id still set after MarkIssueBlocked")
	}
}

func TestBlockedCommentNeverStartsWithSlash(t *testing.T) {
	reasons := []string{"config-drift", "gha-workflow-failure", "/etc/passwd-shaped-reason", ""}
	for _, reason := range reasons {
		t.Run(reason, func(t *testing.T) {
			body := blockedReasonCommentBody(reason)
			if body == "" {
				t.Fatal("comment body must not be empty")
			}
			if body[0] == '/' {
				t.Fatalf("comment body must never start with '/', got %q", body)
			}
		})
	}
}

func TestBlockedCommentCarriesNoMentionLink(t *testing.T) {
	reasons := []string{"config-drift", "gha-workflow-failure", "mention://agent/00000000-0000-0000-0000-000000000000"}
	for _, reason := range reasons {
		t.Run(reason, func(t *testing.T) {
			body := blockedReasonCommentBody(reason)
			if strings.Contains(body, "mention://") {
				t.Fatalf("comment body must never contain a mention:// link, got %q", body)
			}
		})
	}
}

// TestNoCommentWhenTheStatusWriteFailed proves MarkIssueBlocked posts nothing
// when the status write it depends on fails: a reason comment on a card that
// never moved would describe a state that doesn't exist. Calls
// MarkIssueBlocked directly (rather than through FailTask) so the only
// comment in play is the one this function might post - FailTask posts its
// own separate error comment on the same condition, which would otherwise
// have to be filtered out of the count.
func TestNoCommentWhenTheStatusWriteFailed(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	svc := NewTaskService(db.New(pool), pool, nil, events.New())

	issue, _, agentID, _ := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "MBC", number: 700012, assign: true}, StatusInProgress)
	issueID := util.UUIDToString(issue.ID)

	installStatusFailTrigger(t, ctx, pool, "blockedcomment", issueID)

	svc.MarkIssueBlocked(ctx, issue, util.MustParseUUID(agentID), "config-drift")

	if got := countCommentsForIssue(t, ctx, pool, issueID, ""); got != 0 {
		t.Fatalf("comment count = %d, want 0 when the status write failed", got)
	}
}

func TestMarkIssueRunningPromotesOrLeavesAlone(t *testing.T) {
	cases := []struct {
		name  string
		start string
		want  string
	}{
		{"PromotesTodo", StatusTodo, StatusInProgress},
		{"PromotesBlocked", StatusBlocked, StatusInProgress},
		{"LeavesDoneAlone", StatusDone, StatusDone},
		{"LeavesInReviewAlone", StatusInReview, StatusInReview},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTaskClaimRacePool(t)
			svc := NewTaskService(db.New(pool), pool, nil, events.New())

			issue, agentName, _, _ := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "MRT", number: 700001}, tc.start)
			svc.MarkIssueRunning(ctx, issue, agentName)

			var status string
			if err := pool.QueryRow(ctx, `SELECT status FROM issue WHERE id = $1`, issue.ID).Scan(&status); err != nil {
				t.Fatalf("read status: %v", err)
			}
			if status != tc.want {
				t.Fatalf("status = %q, want %q", status, tc.want)
			}
		})
	}
}

func TestMarkIssueRunningKeepsTheAssignee(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	svc := NewTaskService(db.New(pool), pool, nil, events.New())

	issue, agentName, _, _ := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "MRT", number: 700002, assign: true}, StatusTodo)
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
		t.Fatalf("assignee changed: type=%q id.valid=%v, want unchanged from id.valid=%v", assigneeType, assigneeID.Valid, issue.AssigneeID.Valid)
	}
}

func TestMarkIssueRunningPublishesIssueUpdated(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	bus := events.New()
	svc := NewTaskService(db.New(pool), pool, nil, bus)

	issue, agentName, _, _ := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "MRT", number: 700003}, StatusTodo)

	var got events.Event
	seen := false
	bus.Subscribe(protocol.EventIssueUpdated, func(e events.Event) {
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

	issue, _, agentID, runtimeID := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "DST", number: 700004, webhook: true}, StatusTodo)
	issueID := util.UUIDToString(issue.ID)

	installStatusFailTrigger(t, ctx, pool, "dispatch", issueID)

	taskID := createLifecycleTask(t, ctx, pool, agentID, runtimeID, issueID, "queued")

	task, err := svc.Queries.GetAgentTask(ctx, util.MustParseUUID(taskID))
	if err != nil {
		t.Fatalf("load task: %v", err)
	}
	runtime, err := svc.Queries.GetAgentRuntime(ctx, util.MustParseUUID(runtimeID))
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	agent, err := svc.Queries.GetAgent(ctx, util.MustParseUUID(agentID))
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

func TestFailTaskBlocksOrLeavesAlone(t *testing.T) {
	cases := []struct {
		name  string
		start string
		want  string
	}{
		{"BlocksTheIssue", StatusInProgress, StatusBlocked},
		{"LeavesDoneAlone", StatusDone, StatusDone},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTaskClaimRacePool(t)
			svc := NewTaskService(db.New(pool), pool, nil, events.New())

			issue, _, agentID, runtimeID := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "FTT", number: 700005 + int32(i), assign: true}, tc.start)
			taskID := createLifecycleTask(t, ctx, pool, agentID, runtimeID, util.UUIDToString(issue.ID), "running")

			if _, err := svc.FailTask(ctx, util.MustParseUUID(taskID), "boom", "", "", "agent_error"); err != nil {
				t.Fatalf("FailTask: %v", err)
			}

			var status string
			if err := pool.QueryRow(ctx, `SELECT status FROM issue WHERE id = $1`, issue.ID).Scan(&status); err != nil {
				t.Fatalf("read issue status: %v", err)
			}
			if status != tc.want {
				t.Fatalf("issue status = %q, want %q", status, tc.want)
			}
		})
	}
}

func TestFailTaskClearsBothAssigneeFields(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	svc := NewTaskService(db.New(pool), pool, nil, events.New())

	issue, _, agentID, runtimeID := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "FTT", number: 700007, assign: true}, StatusInProgress)
	if !issue.AssigneeID.Valid {
		t.Fatal("fixture issue expected to start assigned")
	}
	taskID := createLifecycleTask(t, ctx, pool, agentID, runtimeID, util.UUIDToString(issue.ID), "running")

	if _, err := svc.FailTask(ctx, util.MustParseUUID(taskID), "boom", "", "", "agent_error"); err != nil {
		t.Fatalf("FailTask: %v", err)
	}

	var assigneeType pgtype.Text
	var assigneeID pgtype.UUID
	if err := pool.QueryRow(ctx, `SELECT assignee_type, assignee_id FROM issue WHERE id = $1`, issue.ID).
		Scan(&assigneeType, &assigneeID); err != nil {
		t.Fatalf("read issue assignee: %v", err)
	}
	if assigneeType.Valid || assigneeID.Valid {
		t.Fatalf("assignee_type.valid=%v assignee_id.valid=%v, want both NULL", assigneeType.Valid, assigneeID.Valid)
	}
}

// TestFailTaskStillFailsTheTaskWhenTheIssueWriteFails proves MarkIssueBlocked
// is best-effort in FailTask: forcing the real UPDATE issue SET status = ...
// statement to fail must not prevent the task row from reaching 'failed'.
// Same mechanism as TestDispatchProceedsWhenTheStatusWriteFails - a row-scoped
// trigger, not a disabled constraint.
func TestFailTaskStillFailsTheTaskWhenTheIssueWriteFails(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	svc := NewTaskService(db.New(pool), pool, nil, events.New())

	issue, _, agentID, runtimeID := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "FTT", number: 700008, assign: true}, StatusInProgress)
	issueID := util.UUIDToString(issue.ID)
	taskID := createLifecycleTask(t, ctx, pool, agentID, runtimeID, issueID, "running")

	installStatusFailTrigger(t, ctx, pool, "failtask", issueID)

	if _, err := svc.FailTask(ctx, util.MustParseUUID(taskID), "boom", "", "", "agent_error"); err != nil {
		t.Fatalf("FailTask: %v", err)
	}

	var taskStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM agent_task_queue WHERE id = $1`, taskID).Scan(&taskStatus); err != nil {
		t.Fatalf("read task status: %v", err)
	}
	if taskStatus != "failed" {
		t.Fatalf("task status = %q, want failed despite the issue status-write failure", taskStatus)
	}

	var issueStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM issue WHERE id = $1`, issue.ID).Scan(&issueStatus); err != nil {
		t.Fatalf("read issue status: %v", err)
	}
	if issueStatus != StatusInProgress {
		t.Fatalf("issue status = %q, want unchanged %q since the write genuinely failed", issueStatus, StatusInProgress)
	}
}

// TestFailTaskDoesNotBlockWhenAutoRetryIsPending proves the auto-retry gate on
// MarkIssueBlocked: a failure whose reason is retryable and has remaining
// attempt budget must leave the issue exactly where it was (in_progress) and
// create a retry child, not park the card at blocked. See retryableReasons
// and retryEligible in task.go. createLifecycleTask's default attempt=1,
// maxAttempts=2 is exactly the budget this test depends on.
func TestFailTaskDoesNotBlockWhenAutoRetryIsPending(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	svc := NewTaskService(db.New(pool), pool, nil, events.New())

	issue, _, agentID, runtimeID := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "FTT", number: 700009, assign: true}, StatusInProgress)
	taskID := createLifecycleTask(t, ctx, pool, agentID, runtimeID, util.UUIDToString(issue.ID), "running")

	if _, err := svc.FailTask(ctx, util.MustParseUUID(taskID), "boom", "", "", "timeout"); err != nil {
		t.Fatalf("FailTask: %v", err)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM issue WHERE id = $1`, issue.ID).Scan(&status); err != nil {
		t.Fatalf("read issue status: %v", err)
	}
	if status != StatusInProgress {
		t.Fatalf("issue status = %q, want unchanged %q while a retry is pending", status, StatusInProgress)
	}

	var retryCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND id != $2`, issue.ID, taskID).Scan(&retryCount); err != nil {
		t.Fatalf("count retry tasks: %v", err)
	}
	if retryCount != 1 {
		t.Fatalf("retry task count = %d, want 1", retryCount)
	}
}
