package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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
//
// Hazard: the WHEN clause scopes the trigger's firing to one row, but the
// DROP TRIGGER / CREATE FUNCTION / CREATE TRIGGER statements above take an
// ACCESS EXCLUSIVE lock on the shared issue table, and the name is fixed
// (not per-run). Two copies of this suite running at the same time will
// destroy each other: run B's DROP removes run A's still-live trigger,
// silently inverting whatever run A is in the middle of asserting. Do not
// run this test suite concurrently with another copy of itself against the
// same database.
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
//
// Hazard: same as installStatusFailTrigger above - the WHEN clause scopes
// firing to one row, but the DDL itself takes an ACCESS EXCLUSIVE lock on the
// shared comment table under a fixed name. Two simultaneous runs of this
// suite will destroy each other's triggers. Do not run this test suite
// concurrently with another copy of itself against the same database.
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

// TestBlockedCommentNeutralisesCodeSpanBreakers proves the F5 hardening:
// failure_reason is not constrained to the classifier's taxonomy at the API
// boundary (TaskFailRequest.FailureReason passes it through unchecked), so a
// backtick or newline in a reason must not be able to break the markdown code
// span blockedReasonCommentBody interpolates it into.
func TestBlockedCommentNeutralisesCodeSpanBreakers(t *testing.T) {
	reasons := []string{
		"config-drift`with`backticks",
		"multi\nline\nreason",
	}
	for _, reason := range reasons {
		t.Run(reason, func(t *testing.T) {
			body := blockedReasonCommentBody(reason)
			if body[0] == '/' {
				t.Fatalf("comment body must never start with '/', got %q", body)
			}
			if strings.Contains(body, "mention://") {
				t.Fatalf("comment body must never contain a mention:// link, got %q", body)
			}
			// The interpolated reason must not contain a raw backtick or
			// newline, or it would close the code span early / break it
			// across lines.
			start := strings.Index(body, "`")
			end := strings.LastIndex(body, "`")
			if start == -1 || end == -1 || start == end {
				t.Fatalf("comment body must contain a closed code span, got %q", body)
			}
			inner := body[start+1 : end]
			if strings.Contains(inner, "`") || strings.Contains(inner, "\n") {
				t.Fatalf("code span contents must not contain a backtick or newline, got %q", inner)
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
		{"PromotesBacklog", StatusBacklog, StatusInProgress},
		{"RedispatchesInProgress", StatusInProgress, StatusInProgress},
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

// TestDispatchWritesInProgress proves the headline behaviour of this branch:
// dispatching a queued webhook task actually writes in_progress onto its
// issue, not merely that dispatch survives when that write fails (see
// TestDispatchProceedsWhenTheStatusWriteFails above, which only covers the
// negative). Uses the same webhook-mode fixture and unreachable webhook_url
// as that test - the POST itself is fire-and-forget in a background
// goroutine and irrelevant here.
func TestDispatchWritesInProgress(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	svc := NewTaskService(db.New(pool), pool, nil, events.New())

	issue, _, agentID, runtimeID := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "DWP", number: 700013, webhook: true}, StatusTodo)
	issueID := util.UUIDToString(issue.ID)

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

	var issueStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM issue WHERE id = $1`, issueID).Scan(&issueStatus); err != nil {
		t.Fatalf("read issue status: %v", err)
	}
	if issueStatus != StatusInProgress {
		t.Fatalf("issue status = %q, want %q after dispatch", issueStatus, StatusInProgress)
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

// TestMarkIssueNotRunningReturnsInProgressToTodo proves the abandoned-run
// unwind: a card sitting at in_progress (a run was dispatched, then the task
// was cancelled out from under it rather than failing) returns to todo, and
// its assignee is left untouched - unlike MarkIssueBlocked, which clears it.
func TestMarkIssueNotRunningReturnsInProgressToTodo(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	svc := NewTaskService(db.New(pool), pool, nil, events.New())

	issue, _, _, _ := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "MNR", number: 700010, assign: true}, StatusInProgress)
	if !issue.AssigneeID.Valid {
		t.Fatal("fixture issue expected to start assigned")
	}

	svc.MarkIssueNotRunning(ctx, issue.ID, "agent_tasks_cancelled")

	var status, assigneeType string
	var assigneeID pgtype.UUID
	if err := pool.QueryRow(ctx, `SELECT status, assignee_type, assignee_id FROM issue WHERE id = $1`, issue.ID).
		Scan(&status, &assigneeType, &assigneeID); err != nil {
		t.Fatalf("read issue: %v", err)
	}
	if status != StatusTodo {
		t.Fatalf("status = %q, want %q", status, StatusTodo)
	}
	if assigneeType != "agent" || !assigneeID.Valid || assigneeID != issue.AssigneeID {
		t.Fatalf("assignee changed: type=%q id.valid=%v, want unchanged from id.valid=%v", assigneeType, assigneeID.Valid, issue.AssigneeID.Valid)
	}
}

// TestMarkIssueNotRunningLeavesOtherStatusesAlone pins the narrow guard:
// MarkIssueNotRunning must fire ONLY when the card is exactly at
// in_progress. This is the important test in the group - it is the one most
// likely to be broken by a future "unify with promotableStatuses" refactor,
// which would incorrectly let this writer drag an already-blocked card (a
// real failure signal) back to todo on an unrelated cancel/delete/revoke.
func TestMarkIssueNotRunningLeavesOtherStatusesAlone(t *testing.T) {
	cases := []struct {
		name  string
		start string
	}{
		{"LeavesBacklogAlone", StatusBacklog},
		{"LeavesTodoAlone", StatusTodo},
		{"LeavesBlockedAlone", StatusBlocked},
		{"LeavesInReviewAlone", StatusInReview},
		{"LeavesDoneAlone", StatusDone},
		{"LeavesCancelledAlone", StatusCancelled},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTaskClaimRacePool(t)
			svc := NewTaskService(db.New(pool), pool, nil, events.New())

			issue, _, _, _ := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "MNR", number: 700011 + int32(i)}, tc.start)
			svc.MarkIssueNotRunning(ctx, issue.ID, "agent_tasks_cancelled")

			var status string
			if err := pool.QueryRow(ctx, `SELECT status FROM issue WHERE id = $1`, issue.ID).Scan(&status); err != nil {
				t.Fatalf("read status: %v", err)
			}
			if status != tc.start {
				t.Fatalf("status = %q, want unchanged %q", status, tc.start)
			}
		})
	}
}

// TestMarkIssueNotRunningLeavesCardAloneWhenAnotherAgentIsActive pins the
// concurrency gate: a card at in_progress is not returned to todo when a
// DIFFERENT agent still has a live task on the same issue. This is the
// scenario promotableStatuses' own comment calls out ("a second run against
// the same card, or a retry") from the opposite direction - agent A is still
// running on the issue, agent B's tasks on the same issue get cancelled
// (simulated here by calling MarkIssueNotRunning directly, as
// CancelTasksForAgent would after cancelling B's rows), and the card must
// stay in_progress because A is still working it. Flipping it to todo here
// would be the same class of lie this writer exists to fix, in reverse.
func TestMarkIssueNotRunningLeavesCardAloneWhenAnotherAgentIsActive(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	svc := NewTaskService(db.New(pool), pool, nil, events.New())

	// Agent A's card, sitting at in_progress from A's own dispatch.
	issue, _, _, runtimeID := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "MNRC", number: 700030, assign: true}, StatusInProgress)

	// A second agent (B) with a live task pointed at the SAME issue - the
	// surviving run that must block the unwind.
	var agentBID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent (workspace_id, name, runtime_mode, runtime_config, runtime_id, visibility, max_concurrent_tasks)
		VALUES ($1, $2, 'cloud', '{}'::jsonb, $3, 'private', 5)
		RETURNING id`, util.UUIDToString(issue.WorkspaceID), fmt.Sprintf("MNRC Agent B %d", time.Now().UnixNano()), runtimeID).Scan(&agentBID); err != nil {
		t.Fatalf("create second agent: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM agent WHERE id = $1`, agentBID)
	})
	createLifecycleTask(t, ctx, pool, agentBID, runtimeID, util.UUIDToString(issue.ID), "running")

	// Simulate agent A's tasks having just been cancelled out from under
	// the card - exactly what CancelTasksForAgent does before calling this.
	svc.MarkIssueNotRunning(ctx, issue.ID, "agent_tasks_cancelled")

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM issue WHERE id = $1`, issue.ID).Scan(&status); err != nil {
		t.Fatalf("read issue status: %v", err)
	}
	if status != StatusInProgress {
		t.Fatalf("issue status = %q, want unchanged %q while agent B's task is still active", status, StatusInProgress)
	}
}

// TestCancelTasksForAgentReturnsCardsToTodo proves the unwind is wired into
// the actual "cancel all tasks for an agent" call site, not just reachable in
// isolation: a card at in_progress with an active task reaches todo once
// CancelTasksForAgent cancels that task.
func TestCancelTasksForAgentReturnsCardsToTodo(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	svc := NewTaskService(db.New(pool), pool, nil, events.New())

	issue, _, agentID, runtimeID := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "CTA", number: 700020, assign: true}, StatusInProgress)
	createLifecycleTask(t, ctx, pool, agentID, runtimeID, util.UUIDToString(issue.ID), "running")

	if _, err := svc.CancelTasksForAgent(ctx, util.MustParseUUID(agentID)); err != nil {
		t.Fatalf("CancelTasksForAgent: %v", err)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM issue WHERE id = $1`, issue.ID).Scan(&status); err != nil {
		t.Fatalf("read issue status: %v", err)
	}
	if status != StatusTodo {
		t.Fatalf("issue status = %q, want %q", status, StatusTodo)
	}
}

// TestCancelTasksForAgentKeepsTheAssignee proves the assignee survives the
// unwind, unlike the blocked path - the operator cancelling this agent's
// tasks did not learn anything that says the assignee can't do the work; an
// archived agent's lingering assignment is harmless (isAgentAssigneeReady
// returns false once archived_at is set). It also asserts the status itself
// reached todo, so this test independently proves the unwind fired rather
// than merely observing an assignee that was never touched in the first
// place - without that assertion this test would keep passing even if the
// unwind were dropped entirely, and would only ever catch a switch to
// UpdateIssueStatusAndUnassign.
func TestCancelTasksForAgentKeepsTheAssignee(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	svc := NewTaskService(db.New(pool), pool, nil, events.New())

	issue, _, agentID, runtimeID := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "CTA", number: 700021, assign: true}, StatusInProgress)
	if !issue.AssigneeID.Valid {
		t.Fatal("fixture issue expected to start assigned")
	}
	createLifecycleTask(t, ctx, pool, agentID, runtimeID, util.UUIDToString(issue.ID), "running")

	if _, err := svc.CancelTasksForAgent(ctx, util.MustParseUUID(agentID)); err != nil {
		t.Fatalf("CancelTasksForAgent: %v", err)
	}

	var status, assigneeType string
	var assigneeID pgtype.UUID
	if err := pool.QueryRow(ctx, `SELECT status, assignee_type, assignee_id FROM issue WHERE id = $1`, issue.ID).
		Scan(&status, &assigneeType, &assigneeID); err != nil {
		t.Fatalf("read issue: %v", err)
	}
	if status != StatusTodo {
		t.Fatalf("issue status = %q, want %q - the unwind must have fired for this test to prove anything about the assignee", status, StatusTodo)
	}
	if assigneeType != "agent" || !assigneeID.Valid || assigneeID != issue.AssigneeID {
		t.Fatalf("assignee changed: type=%q id.valid=%v, want unchanged from id.valid=%v", assigneeType, assigneeID.Valid, issue.AssigneeID.Valid)
	}
}

// createIssueDoneRaceTrigger makes the literal "drag a card to done" UPDATE
// (any transition into status = 'done' on the fixture's own issue row) sleep
// for 0.2s while holding that row's lock. This is what widens the window for
// TestMarkIssueRunningDoesNotClobberAConcurrentDone and its siblings: the
// lifecycle writer's own GetIssue is a plain read that returns immediately
// with the pre-transaction status (read committed semantics - the concurrent
// done write hasn't committed yet), but the writer's own guarded UPDATE then
// has to wait for this row's lock. By the time it gets it, the done write has
// committed, and Postgres re-evaluates the UPDATE's WHERE clause against that
// newly-committed row before applying anything - exactly reproducing (and,
// for the guarded queries, closing) the TOCTOU this suite is proving. Scoped
// by a WHEN clause to one issue id; fixed trigger/function names with
// DROP ... IF EXISTS before CREATE and the drops registered in t.Cleanup,
// matching this file's other trigger helpers (installStatusFailTrigger,
// installCommentBeforeAssigneeClearTrigger).
func createIssueDoneRaceTrigger(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name, issueID string) {
	t.Helper()
	triggerName := "lifecycle_done_race_" + name
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
			PERFORM pg_sleep(0.2);
			RETURN NEW;
		END;
		$$;
	`, quoteIdent(functionName))); err != nil {
		t.Fatalf("create issue done-race sleep trigger function: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TRIGGER %s
		BEFORE UPDATE OF status ON issue
		FOR EACH ROW
		WHEN (NEW.id = %s::uuid AND NEW.status = 'done')
		EXECUTE FUNCTION %s();
	`, quoteIdent(triggerName), quoteLiteral(issueID), quoteIdent(functionName))); err != nil {
		t.Fatalf("create issue done-race sleep trigger: %v", err)
	}
}

// concurrentDoneWrite issues the literal "drag to done" UPDATE against
// issueID in a goroutine, returning a WaitGroup callers must Wait() on once
// they're done exercising the lifecycle writer under test. Callers must
// sleep briefly after starting this before invoking the writer under test,
// so the done write has had time to start its transaction, acquire the row
// lock, and enter createIssueDoneRaceTrigger's 0.2s sleep before the
// writer's own GetIssue runs.
func concurrentDoneWrite(t *testing.T, pool *pgxpool.Pool, issueID string) *sync.WaitGroup {
	t.Helper()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := pool.Exec(context.Background(), `UPDATE issue SET status = 'done', updated_at = now() WHERE id = $1`, issueID); err != nil {
			t.Errorf("concurrent done write: %v", err)
		}
	}()
	return &wg
}

// TestMarkIssueRunningDoesNotClobberAConcurrentDone is the headline proof for
// the guarded write. done is the merge trigger, and dragging a card out of
// done is how a human cancels a merge. If a run-start status write is
// computed from a stale pre-done read and then lands after a concurrent done
// write, the card is silently dragged back to in_progress, cancelling the
// merge with no error and no log - the exact bug this fix closes. See
// createIssueDoneRaceTrigger for how the interleaving is forced.
func TestMarkIssueRunningDoesNotClobberAConcurrentDone(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	svc := NewTaskService(db.New(pool), pool, nil, events.New())

	issue, agentName, _, _ := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "MRR", number: 700040}, StatusTodo)
	issueID := util.UUIDToString(issue.ID)

	createIssueDoneRaceTrigger(t, ctx, pool, "running", issueID)
	wg := concurrentDoneWrite(t, pool, issueID)
	time.Sleep(50 * time.Millisecond)

	svc.MarkIssueRunning(ctx, issue, agentName)

	wg.Wait()

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM issue WHERE id = $1`, issueID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != StatusDone {
		t.Fatalf("status = %q, want %q (a concurrent done write must survive a run-start status write based on a stale read)", status, StatusDone)
	}
}

// TestMarkIssueBlockedDoesNotClobberAConcurrentDone is MarkIssueRunning's
// sibling proof: a task failing after its PR already merged (done) must not
// drag the card back to blocked because MarkIssueBlocked's own status check
// read a stale pre-done value. Same interleaving as
// TestMarkIssueRunningDoesNotClobberAConcurrentDone; see
// createIssueDoneRaceTrigger.
func TestMarkIssueBlockedDoesNotClobberAConcurrentDone(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	svc := NewTaskService(db.New(pool), pool, nil, events.New())

	issue, _, agentID, _ := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "MBR", number: 700041, assign: true}, StatusInProgress)
	issueID := util.UUIDToString(issue.ID)

	createIssueDoneRaceTrigger(t, ctx, pool, "blocked", issueID)
	wg := concurrentDoneWrite(t, pool, issueID)
	time.Sleep(50 * time.Millisecond)

	svc.MarkIssueBlocked(ctx, issue, util.MustParseUUID(agentID), "race-check")

	wg.Wait()

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM issue WHERE id = $1`, issueID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != StatusDone {
		t.Fatalf("status = %q, want %q (a concurrent done write must survive a blocked-status write based on a stale read)", status, StatusDone)
	}
}

// TestMarkIssueNotRunningDoesNotClobberAConcurrentDone proves the same
// property for MarkIssueNotRunning, whose guarded write
// (UpdateIssueStatusIfCurrentAndInactive) folds in the second,
// active-task race alongside the status one - see the writer's docstring and
// the query's own comment in issue.sql.
func TestMarkIssueNotRunningDoesNotClobberAConcurrentDone(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	svc := NewTaskService(db.New(pool), pool, nil, events.New())

	issue, _, _, _ := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "MNRR", number: 700042}, StatusInProgress)
	issueID := util.UUIDToString(issue.ID)

	createIssueDoneRaceTrigger(t, ctx, pool, "notrunning", issueID)
	wg := concurrentDoneWrite(t, pool, issueID)
	time.Sleep(50 * time.Millisecond)

	svc.MarkIssueNotRunning(ctx, issue.ID, "agent_tasks_cancelled")

	wg.Wait()

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM issue WHERE id = $1`, issueID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != StatusDone {
		t.Fatalf("status = %q, want %q (a concurrent done write must survive a not-running status write based on a stale read)", status, StatusDone)
	}
}

// assertIssueStatus is a small helper for the guarded-query unit tests below:
// it reads back issueID's status and fails the test if it doesn't match
// want, used to prove a guarded write that matched no row also left the row
// completely unchanged.
func assertIssueStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, issueID pgtype.UUID, want string) {
	t.Helper()
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM issue WHERE id = $1`, issueID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != want {
		t.Fatalf("status = %q, want unchanged %q", status, want)
	}
}

// TestGuardedStatusQueriesMatchNoRowWhenStatusExcluded is the direct unit
// test on the guarded queries themselves (independent of the service-layer
// writers above): calling any of them with a permitted-status set that
// excludes the row's actual current status must match no row - surfaced by
// sqlc as pgx.ErrNoRows for a :one query - and must leave the row completely
// unchanged, including any column (assignee) the statement would otherwise
// have written.
func TestGuardedStatusQueriesMatchNoRowWhenStatusExcluded(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	queries := db.New(pool)

	t.Run("UpdateIssueStatusIfCurrent", func(t *testing.T) {
		issue, _, _, _ := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "GRD", number: 700043}, StatusDone)

		_, err := queries.UpdateIssueStatusIfCurrent(ctx, db.UpdateIssueStatusIfCurrentParams{
			ID:              issue.ID,
			Status:          StatusInProgress,
			WorkspaceID:     issue.WorkspaceID,
			CurrentStatuses: promotableStatusList,
		})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("err = %v, want pgx.ErrNoRows", err)
		}
		assertIssueStatus(t, ctx, pool, issue.ID, StatusDone)
	})

	t.Run("UpdateIssueStatusAndUnassignIfCurrent", func(t *testing.T) {
		issue, _, _, _ := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "GRD", number: 700044, assign: true}, StatusDone)
		if !issue.AssigneeID.Valid {
			t.Fatal("fixture issue expected to start assigned")
		}

		_, err := queries.UpdateIssueStatusAndUnassignIfCurrent(ctx, db.UpdateIssueStatusAndUnassignIfCurrentParams{
			ID:              issue.ID,
			Status:          StatusBlocked,
			WorkspaceID:     issue.WorkspaceID,
			CurrentStatuses: promotableStatusList,
		})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("err = %v, want pgx.ErrNoRows", err)
		}
		assertIssueStatus(t, ctx, pool, issue.ID, StatusDone)

		var assigneeID pgtype.UUID
		if err := pool.QueryRow(ctx, `SELECT assignee_id FROM issue WHERE id = $1`, issue.ID).Scan(&assigneeID); err != nil {
			t.Fatalf("read assignee: %v", err)
		}
		if !assigneeID.Valid {
			t.Fatal("assignee_id cleared even though the guarded write matched no row")
		}
	})

	t.Run("UpdateIssueStatusIfCurrentAndInactive", func(t *testing.T) {
		issue, _, agentID, runtimeID := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "GRD", number: 700045}, StatusInProgress)
		createLifecycleTask(t, ctx, pool, agentID, runtimeID, util.UUIDToString(issue.ID), "running")

		_, err := queries.UpdateIssueStatusIfCurrentAndInactive(ctx, db.UpdateIssueStatusIfCurrentAndInactiveParams{
			ID:              issue.ID,
			Status:          StatusTodo,
			WorkspaceID:     issue.WorkspaceID,
			CurrentStatuses: []string{StatusInProgress},
		})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("err = %v, want pgx.ErrNoRows (an active task on the issue must also block the write)", err)
		}
		assertIssueStatus(t, ctx, pool, issue.ID, StatusInProgress)
	})
}
