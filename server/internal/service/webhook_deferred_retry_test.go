package service

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// deferTask turns a task row into a deferred retry firing `secondsFromNow`
// seconds from now (negative = already due). This is the state
// CreateRetryTask leaves a backoff-armed child in.
func deferTask(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID string, secondsFromNow int) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		UPDATE agent_task_queue
		SET status = 'deferred',
		    fire_at = now() + make_interval(secs => $2::double precision)
		WHERE id = $1`, taskID, secondsFromNow); err != nil {
		t.Fatalf("defer task: %v", err)
	}
}

func promoteDueDeferredWebhook(t *testing.T, ctx context.Context, queries *db.Queries) []db.AgentTaskQueue {
	t.Helper()
	rows, err := queries.PromoteDueDeferredWebhookTasks(ctx)
	if err != nil {
		t.Fatalf("PromoteDueDeferredWebhookTasks: %v", err)
	}
	return rows
}

// A deferred retry on a webhook runtime has exactly one clock: this query,
// run from the sweeper tick. The daemon's PromoteDueDeferredTasksForRuntime
// runs only inside a claim poll, and a webhook runtime never claims — so
// without this arm every 5/10-minute retry is a `deferred` row forever
// (design 2026-09-13-github-unreachable-retry, invariant 4).
func TestDueDeferredWebhookTaskIsPromoted(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	queries := db.New(pool)

	issue, _, agentID, runtimeID := newLifecycleFixture(t, ctx, pool,
		lifecycleFixtureOpts{prefix: "WDR", number: 720001, webhook: true}, StatusInProgress)
	taskID := createLifecycleTask(t, ctx, pool, agentID, runtimeID, util.UUIDToString(issue.ID), "queued")
	deferTask(t, ctx, pool, taskID, -1)

	if !containsTask(promoteDueDeferredWebhook(t, ctx, queries), taskID) {
		t.Fatal("a due deferred webhook task was not promoted")
	}
	if got := taskRow(t, ctx, queries, taskID).Status; got != "queued" {
		t.Fatalf("task status = %q, want queued", got)
	}
}

func TestFutureDeferredWebhookTaskWaits(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	queries := db.New(pool)

	issue, _, agentID, runtimeID := newLifecycleFixture(t, ctx, pool,
		lifecycleFixtureOpts{prefix: "WDR", number: 720002, webhook: true}, StatusInProgress)
	taskID := createLifecycleTask(t, ctx, pool, agentID, runtimeID, util.UUIDToString(issue.ID), "queued")
	deferTask(t, ctx, pool, taskID, 600)

	if containsTask(promoteDueDeferredWebhook(t, ctx, queries), taskID) {
		t.Fatal("a deferred webhook task was promoted 10 minutes early")
	}
	if got := taskRow(t, ctx, queries, taskID).Status; got != "deferred" {
		t.Fatalf("task status = %q, want deferred", got)
	}
}

// The daemon path owns its own deferred rows (promoted at claim time, with
// the daemon wakeup that follows). Promoting them here would emit a queued
// event with no wakeup behind it and race the claim poll for the same row.
func TestDeferredDaemonTaskIsNotPromotedByTheWebhookSweeper(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	queries := db.New(pool)

	issue, _, agentID, runtimeID := newLifecycleFixture(t, ctx, pool,
		lifecycleFixtureOpts{prefix: "WDR", number: 720003, webhook: false}, StatusInProgress)
	taskID := createLifecycleTask(t, ctx, pool, agentID, runtimeID, util.UUIDToString(issue.ID), "queued")
	deferTask(t, ctx, pool, taskID, -1)

	if containsTask(promoteDueDeferredWebhook(t, ctx, queries), taskID) {
		t.Fatal("the webhook sweeper promoted a daemon runtime's deferred task")
	}
	if got := taskRow(t, ctx, queries, taskID).Status; got != "deferred" {
		t.Fatalf("task status = %q, want deferred", got)
	}
}

// The service wrapper is what the sweeper calls: it promotes, announces and
// hands each row to the dispatcher. With MULTICA_WEBHOOK_RUNTIME unset the
// dispatcher declines and the row stays `queued` — which is also the
// no-capacity outcome, and drains later like any queued task.
func TestPromoteDueDeferredWebhookTasksPromotesAndReportsTheCount(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	queries := db.New(pool)
	svc := NewTaskService(queries, pool, nil, events.New())

	issue, _, agentID, runtimeID := newLifecycleFixture(t, ctx, pool,
		lifecycleFixtureOpts{prefix: "WDR", number: 720004, webhook: true}, StatusInProgress)
	due := createLifecycleTask(t, ctx, pool, agentID, runtimeID, util.UUIDToString(issue.ID), "queued")
	deferTask(t, ctx, pool, due, -1)
	later := createLifecycleTask(t, ctx, pool, agentID, runtimeID, util.UUIDToString(issue.ID), "queued")
	deferTask(t, ctx, pool, later, 300)

	if got := svc.PromoteDueDeferredWebhookTasks(ctx); got != 1 {
		t.Fatalf("promoted %d, want 1", got)
	}
	if got := taskRow(t, ctx, queries, due).Status; got != "queued" {
		t.Fatalf("due task status = %q, want queued", got)
	}
	if got := taskRow(t, ctx, queries, later).Status; got != "deferred" {
		t.Fatalf("future task status = %q, want deferred", got)
	}
	if got := svc.PromoteDueDeferredWebhookTasks(ctx); got != 0 {
		t.Fatalf("second pass promoted %d, want 0 — promotion is not idempotent", got)
	}
}
