package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Webhook-runtime delivery-receipt tests. Kept in their own file (like
// daemon_webhook_test.go) so the fork's webhook patch stays cleanly separable
// across upstream rebases. They reuse the daemon_test.go harness globals
// (testHandler, testPool, testWorkspaceID, testUserID) and the reconcile
// helpers from comment_reconcile_test.go (pendingTaskCountForAgentIssue).

// registerWebhookRuntimePointingAt registers a webhook runtime whose webhook_url
// is url and returns its id. Cleaned up on test end.
func registerWebhookRuntimePointingAt(t *testing.T, daemonID, url string) string {
	t.Helper()
	w := httptest.NewRecorder()
	req := newDaemonTokenRequest("POST", "/api/daemon/register", map[string]any{
		"workspace_id": testWorkspaceID,
		"daemon_id":    daemonID,
		"device_name":  "gha-runner-pool",
		"runtimes": []map[string]any{{
			"name":               "GitHub Actions",
			"type":               "claude",
			"version":            "1.0.0",
			"status":             "online",
			"runtime_mode":       "webhook",
			"webhook_url":        url,
			"webhook_secret":     "s3cr3t-32b-minimum-length-secret!!",
			"webhook_event_type": "multica-task",
		}},
	}, testWorkspaceID, daemonID)
	testHandler.DaemonRegister(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("register webhook runtime: %d %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	rt := resp["runtimes"].([]any)[0].(map[string]any)
	runtimeID := rt["id"].(string)
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_runtime WHERE id = $1`, runtimeID) })
	return runtimeID
}

// webhookLeakFixture sets up a webhook runtime (POSTing to a 200 stub), an agent
// bound to it and assigned to a fresh issue, a single member trigger comment,
// and a QUEUED comment-triggered task with an EMPTY delivery receipt — the exact
// state a freshly enqueued webhook task starts in. It returns the ids the tests
// assert on.
type webhookLeakFixture struct {
	runtimeID        string
	agentID          string
	issueID          string
	triggerCommentID string
	taskID           string
	posted           chan struct{}
}

func setupWebhookLeakFixture(t *testing.T, issueNumber int32, daemonID string) webhookLeakFixture {
	t.Helper()
	ctx := context.Background()

	posted := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case posted <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	runtimeID := registerWebhookRuntimePointingAt(t, daemonID, srv.URL)

	agentID := createHandlerTestAgent(t, "Webhook Leak Agent "+daemonID, nil)
	// Bind the agent to the webhook runtime. MaybeDispatchToWebhook keys on the
	// runtime row's mode, not the agent's runtime_mode column, so only runtime_id
	// must move for the dispatch path to select the webhook branch.
	if _, err := testPool.Exec(ctx, `UPDATE agent SET runtime_id = $1 WHERE id = $2`, runtimeID, agentID); err != nil {
		t.Fatalf("bind agent to webhook runtime: %v", err)
	}

	var issueID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, title, status, priority, creator_id, creator_type, number, position, assignee_type, assignee_id)
		VALUES ($1, 'webhook-leak fixture', 'in_progress', 'none', $2, 'member', $3, 0, 'agent', $4)
		RETURNING id
	`, testWorkspaceID, testUserID, issueNumber, agentID).Scan(&issueID); err != nil {
		t.Fatalf("create issue: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, issueID) })

	var triggerCommentID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type, created_at)
		VALUES ($1, $2, 'member', $3, 'please handle this issue', 'comment', now() - interval '10 minutes')
		RETURNING id
	`, issueID, testWorkspaceID, testUserID).Scan(&triggerCommentID); err != nil {
		t.Fatalf("create trigger comment: %v", err)
	}

	// Queued comment-triggered task with delivered_comment_ids at its '{}'
	// default — no receipt yet, exactly as a just-enqueued webhook task.
	var taskID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, trigger_comment_id, status, priority, created_at)
		VALUES ($1, $2, $3, $4, 'queued', 0, now() - interval '9 minutes')
		RETURNING id
	`, agentID, runtimeID, issueID, triggerCommentID).Scan(&taskID); err != nil {
		t.Fatalf("create queued task: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID) })

	return webhookLeakFixture{
		runtimeID:        runtimeID,
		agentID:          agentID,
		issueID:          issueID,
		triggerCommentID: triggerCommentID,
		taskID:           taskID,
		posted:           posted,
	}
}

// deliveredCount returns how many rows carry commentID in delivered_comment_ids
// for the given task (0 or 1).
func deliveredCount(t *testing.T, taskID, commentID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM agent_task_queue WHERE id = $1 AND $2 = ANY(delivered_comment_ids)`,
		taskID, commentID).Scan(&n); err != nil {
		t.Fatalf("count delivered: %v", err)
	}
	return n
}

// TestWebhookDispatch_RecordsDeliveredTriggerComment is the root-cause regression
// for the duplicate-task cost leak. The daemon claim path records the comment ids
// it embedded in delivered_comment_ids (via FinalizeTaskClaim); completion
// reconciliation then skips exactly that set. The webhook dispatch path built the
// payload — including the trigger comment — but never recorded the receipt, so the
// reconcile pass saw an empty delivered set and re-fired the task's own trigger
// comment on every completion. This asserts the webhook dispatch now records the
// trigger comment as delivered, mirroring the daemon path.
func TestWebhookDispatch_RecordsDeliveredTriggerComment(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	t.Setenv("MULTICA_WEBHOOK_RUNTIME", "1")
	ctx := context.Background()

	fx := setupWebhookLeakFixture(t, 999401, "wh-delivery-daemon-a")

	task, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(fx.taskID))
	if err != nil {
		t.Fatalf("load queued task: %v", err)
	}

	if dispatched := testHandler.TaskService.MaybeDispatchToWebhook(ctx, task); !dispatched {
		t.Fatalf("MaybeDispatchToWebhook returned false; expected webhook dispatch to be taken")
	}

	if n := deliveredCount(t, fx.taskID, fx.triggerCommentID); n != 1 {
		t.Fatalf("expected trigger comment recorded in delivered_comment_ids after webhook dispatch, got count %d", n)
	}
}

// TestWebhookTask_CompletionDoesNotReenqueueOwnTriggerComment proves the leak is
// closed end to end: a webhook comment-triggered task, once dispatched and
// completed, must NOT spawn a fresh follow-up task from its own trigger comment.
// Before the fix the empty delivery receipt made completion reconciliation replay
// the trigger comment, creating a duplicate task on every completion (an unbounded
// quota-burning loop).
func TestWebhookTask_CompletionDoesNotReenqueueOwnTriggerComment(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	t.Setenv("MULTICA_WEBHOOK_RUNTIME", "1")
	ctx := context.Background()

	fx := setupWebhookLeakFixture(t, 999402, "wh-delivery-daemon-b")

	task, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(fx.taskID))
	if err != nil {
		t.Fatalf("load queued task: %v", err)
	}
	if dispatched := testHandler.TaskService.MaybeDispatchToWebhook(ctx, task); !dispatched {
		t.Fatalf("MaybeDispatchToWebhook returned false; expected webhook dispatch to be taken")
	}

	// Simulate the webhook receiver starting the run (dispatched -> running), so
	// CompleteTask's status='running' CAS matches.
	if _, err := testPool.Exec(ctx, `
		UPDATE agent_task_queue SET status = 'running', started_at = now() - interval '1 minute' WHERE id = $1
	`, fx.taskID); err != nil {
		t.Fatalf("transition task to running: %v", err)
	}

	if w := completeTaskViaHandler(t, fx.taskID, "review complete, no findings"); w.Code != http.StatusOK {
		t.Fatalf("CompleteTask: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// No fresh follow-up task may exist: the trigger comment was the only comment,
	// and it was delivered to the run.
	if n := pendingTaskCountForAgentIssue(t, fx.issueID, fx.agentID); n != 0 {
		t.Fatalf("expected no duplicate follow-up task after completion, got %d", n)
	}

	// Give the best-effort dispatch goroutine a moment so it doesn't outlive the
	// stub server (avoids a noisy post-test connection error in logs).
	select {
	case <-fx.posted:
	case <-time.After(2 * time.Second):
	}
}
