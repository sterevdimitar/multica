package service

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The thresholds the production sweeper passes (cmd/server/runtime_sweeper.go).
// Duplicated as test-local values rather than exported from package main so the
// queries can be exercised from here; the point of each assertion is the
// query's behaviour at a boundary, not the constant's value.
const (
	testWebhookDispatchTimeoutSecs = 1200.0
	testWebhookRunningTimeoutSecs  = 2400.0
	testDaemonDispatchTimeoutSecs  = 300.0
	testDaemonRunningTimeoutSecs   = 9000.0
	testRuntimeStaleSecs           = 150.0
)

// ageTask backdates a task's dispatched_at / started_at so a wall-clock
// deadline can be crossed without sleeping.
func ageTask(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID string, seconds int) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		UPDATE agent_task_queue
		SET dispatched_at = now() - make_interval(secs => $2::double precision),
		    started_at = CASE WHEN started_at IS NULL THEN NULL
		                      ELSE now() - make_interval(secs => $2::double precision) END
		WHERE id = $1`, taskID, seconds); err != nil {
		t.Fatalf("age task: %v", err)
	}
}

// markTaskStarted moves a dispatched row to running with a started_at, the
// state the receiver's /start callback produces.
func markTaskStarted(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID string) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, taskID); err != nil {
		t.Fatalf("mark task started: %v", err)
	}
}

func failStaleWebhook(t *testing.T, ctx context.Context, queries *db.Queries) []db.AgentTaskQueue {
	t.Helper()
	rows, err := queries.FailStaleWebhookTasks(ctx, db.FailStaleWebhookTasksParams{
		DispatchTimeoutSecs: testWebhookDispatchTimeoutSecs,
		RunningTimeoutSecs:  testWebhookRunningTimeoutSecs,
	})
	if err != nil {
		t.Fatalf("FailStaleWebhookTasks: %v", err)
	}
	return rows
}

func failStaleDaemon(t *testing.T, ctx context.Context, queries *db.Queries) []db.AgentTaskQueue {
	t.Helper()
	rows, err := queries.FailStaleTasks(ctx, db.FailStaleTasksParams{
		DispatchTimeoutSecs: testDaemonDispatchTimeoutSecs,
		RunningTimeoutSecs:  testDaemonRunningTimeoutSecs,
		RuntimeStaleSecs:    testRuntimeStaleSecs,
	})
	if err != nil {
		t.Fatalf("FailStaleTasks: %v", err)
	}
	return rows
}

func containsTask(rows []db.AgentTaskQueue, taskID string) bool {
	for _, r := range rows {
		if util.UUIDToString(r.ID) == taskID {
			return true
		}
	}
	return false
}

func taskRow(t *testing.T, ctx context.Context, queries *db.Queries, taskID string) db.AgentTaskQueue {
	t.Helper()
	row, err := queries.GetAgentTask(ctx, util.MustParseUUID(taskID))
	if err != nil {
		t.Fatalf("load task %s: %v", taskID, err)
	}
	return row
}

// TestWebhookDispatchedTaskSurvivesTheDaemonDeadline is the regression this
// whole change exists for. A webhook task carries no prepare lease — only the
// daemon's startTaskPrepareLeaseExtender writes that column — so before the
// exclusion it matched FailStaleTasks' dispatched branch unconditionally and
// was killed 300s after dispatch. Task 4d74e5fe died exactly this way in
// production at 314s, having never started.
func TestWebhookDispatchedTaskSurvivesTheDaemonDeadline(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	queries := db.New(pool)

	issue, _, agentID, runtimeID := newLifecycleFixture(t, ctx, pool,
		lifecycleFixtureOpts{prefix: "WDT", number: 710001, webhook: true}, StatusInProgress)
	taskID := createLifecycleTask(t, ctx, pool, agentID, runtimeID, util.UUIDToString(issue.ID), "dispatched")

	// Well past the daemon's 300s deadline, well inside the webhook one.
	ageTask(t, ctx, pool, taskID, 600)

	if containsTask(failStaleDaemon(t, ctx, queries), taskID) {
		t.Fatal("FailStaleTasks killed a webhook task; webhook rows must be excluded from both its branches")
	}
	if containsTask(failStaleWebhook(t, ctx, queries), taskID) {
		t.Fatal("FailStaleWebhookTasks fired at 600s; the webhook dispatch deadline is 1200s")
	}
	if got := taskRow(t, ctx, queries, taskID).Status; got != "dispatched" {
		t.Fatalf("task status = %q, want dispatched", got)
	}
}

// TestWebhookRunningTaskSurvivesTheDaemonDeadline is the running-branch half.
// A webhook runtime never bumps last_seen_at, so the NOT EXISTS liveness guard
// that protects healthy long daemon runs is permanently satisfied for webhook
// rows — the exemption simply does not apply to them. Production task
// c4a02334 was swept by this branch at 9003s.
func TestWebhookRunningTaskSurvivesTheDaemonDeadline(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	queries := db.New(pool)

	issue, _, agentID, runtimeID := newLifecycleFixture(t, ctx, pool,
		lifecycleFixtureOpts{prefix: "WRT", number: 710002, webhook: true}, StatusInProgress)
	taskID := createLifecycleTask(t, ctx, pool, agentID, runtimeID, util.UUIDToString(issue.ID), "dispatched")
	markTaskStarted(t, ctx, pool, taskID)

	// Past the daemon's 9000s running deadline. The webhook cap is 2400s, so
	// this proves the exclusion holds rather than that the row survives.
	ageTask(t, ctx, pool, taskID, 9600)

	if containsTask(failStaleDaemon(t, ctx, queries), taskID) {
		t.Fatal("FailStaleTasks killed a running webhook task; webhook rows must be excluded from both its branches")
	}
	if got := taskRow(t, ctx, queries, taskID).Status; got != "running" {
		t.Fatalf("task status = %q, want running", got)
	}
}

// TestWebhookDispatchTimeoutFiresPastItsDeadline covers the arm that closes the
// reported gap: a dispatch the receiver never acted on becomes a real failure
// with its own diagnosable reason, instead of sitting at 'dispatched' forever.
func TestWebhookDispatchTimeoutFiresPastItsDeadline(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	queries := db.New(pool)

	issue, _, agentID, runtimeID := newLifecycleFixture(t, ctx, pool,
		lifecycleFixtureOpts{prefix: "WDF", number: 710003, webhook: true}, StatusInProgress)
	taskID := createLifecycleTask(t, ctx, pool, agentID, runtimeID, util.UUIDToString(issue.ID), "dispatched")
	ageTask(t, ctx, pool, taskID, 1500)

	if !containsTask(failStaleWebhook(t, ctx, queries), taskID) {
		t.Fatal("FailStaleWebhookTasks did not fail a dispatched task aged past 1200s")
	}
	row := taskRow(t, ctx, queries, taskID)
	if row.Status != "failed" {
		t.Fatalf("task status = %q, want failed", row.Status)
	}
	if row.FailureReason.String != "dispatch_timeout" {
		t.Fatalf("failure_reason = %q, want dispatch_timeout", row.FailureReason.String)
	}
	if row.StartedAt.Valid {
		t.Fatal("started_at set on a task that was never started")
	}
}

// TestWebhookRunTimeoutFiresPastItsDeadline covers the orphaned-row arm: /start
// arrived, no terminal callback ever did. It keeps the generic 'timeout'
// reason, which is what distinguishes it from the never-started case above —
// the two are different operational problems and the sweeper is the only thing
// that can tell them apart.
func TestWebhookRunTimeoutFiresPastItsDeadline(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	queries := db.New(pool)

	issue, _, agentID, runtimeID := newLifecycleFixture(t, ctx, pool,
		lifecycleFixtureOpts{prefix: "WRF", number: 710004, webhook: true}, StatusInProgress)
	taskID := createLifecycleTask(t, ctx, pool, agentID, runtimeID, util.UUIDToString(issue.ID), "dispatched")
	markTaskStarted(t, ctx, pool, taskID)
	ageTask(t, ctx, pool, taskID, 2700)

	if !containsTask(failStaleWebhook(t, ctx, queries), taskID) {
		t.Fatal("FailStaleWebhookTasks did not fail a running task aged past 2400s")
	}
	row := taskRow(t, ctx, queries, taskID)
	if row.FailureReason.String != "timeout" {
		t.Fatalf("failure_reason = %q, want timeout", row.FailureReason.String)
	}
}

// TestWebhookSweepLeavesAFreshlyStartedTaskAlone is the idempotency/race
// assertion. A task that posted /start a moment before the sweep must not be
// failed by the dispatch branch: the guard is the row's own status, evaluated
// inside the UPDATE, so a row that moved to 'running' between selection and
// apply no longer matches the dispatched predicate.
func TestWebhookSweepLeavesAFreshlyStartedTaskAlone(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	queries := db.New(pool)

	issue, _, agentID, runtimeID := newLifecycleFixture(t, ctx, pool,
		lifecycleFixtureOpts{prefix: "WFS", number: 710005, webhook: true}, StatusInProgress)
	taskID := createLifecycleTask(t, ctx, pool, agentID, runtimeID, util.UUIDToString(issue.ID), "dispatched")

	// Dispatched long ago, but /start has just landed: dispatched_at is past
	// the deadline while started_at is now.
	ageTask(t, ctx, pool, taskID, 1500)
	markTaskStarted(t, ctx, pool, taskID)

	if containsTask(failStaleWebhook(t, ctx, queries), taskID) {
		t.Fatal("sweeper failed a task that had just started; the dispatch branch must key on current status")
	}
	if got := taskRow(t, ctx, queries, taskID).Status; got != "running" {
		t.Fatalf("task status = %q, want running", got)
	}
}

// TestHandleFailedWebhookTasksParksTheCard is defect C: the sweeper's terminal
// path used to reset the card to todo, leaving it indistinguishable from one
// nobody had touched. Nothing re-claims a webhook card on its own, so it must
// park at blocked instead.
func TestHandleFailedWebhookTasksParksTheCard(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	queries := db.New(pool)
	svc := NewTaskService(queries, pool, nil, events.New())

	issue, _, agentID, runtimeID := newLifecycleFixture(t, ctx, pool,
		lifecycleFixtureOpts{prefix: "WPB", number: 710006, assign: true, webhook: true}, StatusInProgress)
	taskID := createLifecycleTask(t, ctx, pool, agentID, runtimeID, util.UUIDToString(issue.ID), "dispatched")
	ageTask(t, ctx, pool, taskID, 1500)

	failed := failStaleWebhook(t, ctx, queries)
	if !containsTask(failed, taskID) {
		t.Fatal("sweep did not fail the task under test")
	}

	// Exhaust the retry budget first: with attempts remaining the card must
	// deliberately stay put (see the retried-guard test below), so parking is
	// only observable on a final failure.
	exhaustRetryBudget(t, ctx, pool, taskID)
	svc.HandleFailedWebhookTasks(ctx, []db.AgentTaskQueue{taskRow(t, ctx, queries, taskID)})

	got, err := queries.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("reload issue: %v", err)
	}
	if got.Status != StatusBlocked {
		t.Fatalf("issue status = %q, want %q", got.Status, StatusBlocked)
	}
	if got.AssigneeID.Valid {
		t.Fatal("blocked card kept its agent assignee; MarkIssueBlocked must clear it")
	}
	if n := countCommentsForIssue(t, ctx, pool, util.UUIDToString(issue.ID), "dispatch_timeout"); n != 1 {
		t.Fatalf("comments naming dispatch_timeout = %d, want 1", n)
	}
}

// exhaustRetryBudget pins attempt to max_attempts so retryEligible refuses,
// making the next failure terminal.
func exhaustRetryBudget(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID string) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`UPDATE agent_task_queue SET attempt = max_attempts WHERE id = $1`, taskID); err != nil {
		t.Fatalf("exhaust retry budget: %v", err)
	}
}

// TestHandleFailedWebhookTasksLeavesARetriedCardAlone is invariant 2. When an
// auto-retry is pending the card must stay exactly where the original dispatch
// left it — blocking a card that is about to re-run is board-visible spam on
// every transient hiccup, which is the same reasoning FailTask's retried==nil
// guard already encodes.
func TestHandleFailedWebhookTasksLeavesARetriedCardAlone(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	queries := db.New(pool)
	svc := NewTaskService(queries, pool, nil, events.New())

	issue, _, agentID, runtimeID := newLifecycleFixture(t, ctx, pool,
		lifecycleFixtureOpts{prefix: "WRG", number: 710007, assign: true, webhook: true}, StatusInProgress)
	taskID := createLifecycleTask(t, ctx, pool, agentID, runtimeID, util.UUIDToString(issue.ID), "dispatched")
	ageTask(t, ctx, pool, taskID, 1500)
	failStaleWebhook(t, ctx, queries)

	// attempt=1, max_attempts=2 from the fixture: a retry is available, so
	// dispatch_timeout being in retryableReasons makes this failure non-final.
	row := taskRow(t, ctx, queries, taskID)
	if row.Attempt >= row.MaxAttempts {
		t.Fatalf("fixture task has no retry budget (attempt=%d max=%d)", row.Attempt, row.MaxAttempts)
	}
	svc.HandleFailedWebhookTasks(ctx, []db.AgentTaskQueue{row})

	got, err := queries.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("reload issue: %v", err)
	}
	if got.Status != StatusInProgress {
		t.Fatalf("issue status = %q, want %q — a card with a pending retry must not be parked", got.Status, StatusInProgress)
	}
}

// TestDispatchTimeoutIsRetryable pins the policy decision. It is only a real
// recovery because MaybeRetryFailedTask now dispatches webhook children; before
// that the retry parked a child in 'queued' for two hours and expired it.
func TestDispatchTimeoutIsRetryable(t *testing.T) {
	if !retryableReasons["dispatch_timeout"] {
		t.Fatal("dispatch_timeout must be retryable: the agent never ran, so there is no agent verdict to respect")
	}
}
