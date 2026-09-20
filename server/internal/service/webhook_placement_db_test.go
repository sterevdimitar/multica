package service

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Runtime placement, against a real database. Needs DATABASE_URL (port 5439
// on this machine); skips without it. Migration 211 must be applied.

// addWebhookRuntime registers a second webhook runtime in the fixture's
// workspace, with a dispatch_order, and cleans it up.
func addWebhookRuntime(t *testing.T, ctx context.Context, pool *pgxpool.Pool, fromAgentID, name string, order int32) string {
	t.Helper()
	var workspaceID, ownerID string
	if err := pool.QueryRow(ctx, `SELECT workspace_id, owner_id FROM agent WHERE id = $1`, fromAgentID).Scan(&workspaceID, &ownerID); err != nil {
		t.Fatalf("load agent workspace: %v", err)
	}
	var id string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_runtime (workspace_id, daemon_id, name, runtime_mode, provider, status, device_info, metadata, last_seen_at, visibility, owner_id, webhook_url, webhook_secret, webhook_event_type, dispatch_order)
		VALUES ($1, $2, $3, 'webhook', 'claude', 'online', 'x', '{}'::jsonb, now(), 'private', $4, 'http://127.0.0.1:1/unreachable', 'sec', 'task.dispatched', $5)
		RETURNING id`, workspaceID, "daemon-"+name+"-"+util.UUIDToString(util.MustParseUUID(fromAgentID))[:8], name, ownerID, order).Scan(&id); err != nil {
		t.Fatalf("create runtime %s: %v", name, err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM agent_runtime WHERE id = $1`, id) })
	return id
}

func setFallback(t *testing.T, ctx context.Context, pool *pgxpool.Pool, agentID string, runtimeIDs ...string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE agent SET fallback_runtime_ids = $2::uuid[] WHERE id = $1`, agentID, runtimeIDs); err != nil {
		t.Fatalf("set fallback: %v", err)
	}
}

func setDown(t *testing.T, ctx context.Context, pool *pgxpool.Pool, runtimeID, reason string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE agent_runtime SET down_until = now() + interval '5 minutes', down_reason = $2 WHERE id = $1`, runtimeID, reason); err != nil {
		t.Fatalf("set down: %v", err)
	}
}

func setCap(t *testing.T, ctx context.Context, pool *pgxpool.Pool, runtimeID string, n int32) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE agent_runtime SET max_concurrent_tasks = $2 WHERE id = $1`, runtimeID, n); err != nil {
		t.Fatalf("set cap: %v", err)
	}
}

func withWebhookRuntimeFlag(t *testing.T) {
	t.Helper()
	prev, had := os.LookupEnv("MULTICA_WEBHOOK_RUNTIME")
	os.Setenv("MULTICA_WEBHOOK_RUNTIME", "1")
	t.Cleanup(func() {
		if had {
			os.Setenv("MULTICA_WEBHOOK_RUNTIME", prev)
		} else {
			os.Unsetenv("MULTICA_WEBHOOK_RUNTIME")
		}
	})
}

// P5: two callers race PlaceAgentTask on one queued row; exactly one wins.
func TestPlaceAgentTaskCASUnderTwoCallers(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	queries := db.New(pool)
	issue, _, agentID, runtimeID := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "PCAS", number: 720001, webhook: true}, StatusTodo)
	taskID := createLifecycleTask(t, ctx, pool, agentID, runtimeID, util.UUIDToString(issue.ID), "queued")
	other := addWebhookRuntime(t, ctx, pool, agentID, "PCAS other", 1)

	var wg sync.WaitGroup
	results := make([]error, 2)
	for i, target := range []string{runtimeID, other} {
		wg.Add(1)
		go func(i int, target string) {
			defer wg.Done()
			_, results[i] = queries.PlaceAgentTask(ctx, db.PlaceAgentTaskParams{ID: util.MustParseUUID(taskID), RuntimeID: util.MustParseUUID(target)})
		}(i, target)
	}
	wg.Wait()
	wins, losses := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, pgx.ErrNoRows):
			losses++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 || losses != 1 {
		t.Fatalf("wins=%d losses=%d, want exactly one of each", wins, losses)
	}
	row := taskRow(t, ctx, queries, taskID)
	if row.Status != "dispatched" || !row.DispatchedAt.Valid {
		t.Errorf("row after placement: status=%s dispatched_at valid=%v", row.Status, row.DispatchedAt.Valid)
	}
}

// Home down, fallback set = {F}: the task lands on F with one ↪ note on the
// card, and the note dispatches nothing (P13).
func TestPlacementNoteOnFailover(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	withWebhookRuntimeFlag(t)
	svc := NewTaskService(db.New(pool), pool, nil, events.New())
	issue, _, agentID, homeID := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "PNOTE", number: 720002, webhook: true}, StatusTodo)
	issueID := util.UUIDToString(issue.ID)
	fallback := addWebhookRuntime(t, ctx, pool, agentID, "PNOTE Circle", 1)
	setFallback(t, ctx, pool, agentID, fallback)
	setDown(t, ctx, pool, homeID, `no online runner carries "pnote"`)
	taskID := createLifecycleTask(t, ctx, pool, agentID, homeID, issueID, "queued")

	task, err := svc.Queries.GetAgentTask(ctx, util.MustParseUUID(taskID))
	if err != nil {
		t.Fatalf("load task: %v", err)
	}
	if !svc.MaybeDispatchToWebhook(ctx, task) {
		t.Fatal("MaybeDispatchToWebhook returned false for a webhook home")
	}
	row := taskRow(t, ctx, svc.Queries, taskID)
	if row.Status != "dispatched" || util.UUIDToString(row.RuntimeID) != fallback {
		t.Fatalf("task status=%s runtime=%s, want dispatched on %s", row.Status, util.UUIDToString(row.RuntimeID), fallback)
	}
	var notes int
	var content string
	if err := pool.QueryRow(ctx, `SELECT count(*), max(content) FROM comment WHERE issue_id = $1 AND content LIKE '↪%'`, issueID).Scan(&notes, &content); err != nil {
		t.Fatalf("count notes: %v", err)
	}
	if notes != 1 {
		t.Fatalf("notes = %d, want 1", notes)
	}
	if want := `↪ PNOTE RT is down (no online runner carries "pnote") — running on PNOTE Circle`; content != want {
		t.Errorf("note = %q\nwant %q", content, want)
	}
	var tasks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID).Scan(&tasks); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if tasks != 1 {
		t.Errorf("tasks on the issue = %d, want 1: the note must dispatch nothing", tasks)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM comment WHERE issue_id = $1`, issueID) })
}

// Home up but full is a wait (P2): the task stays queued, no note, even
// with a free fallback.
func TestPlacementWaitsWhenHomeIsFull(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	withWebhookRuntimeFlag(t)
	svc := NewTaskService(db.New(pool), pool, nil, events.New())
	issue, _, agentID, homeID := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "PFULL", number: 720003, webhook: true}, StatusTodo)
	issueID := util.UUIDToString(issue.ID)
	fallback := addWebhookRuntime(t, ctx, pool, agentID, "PFULL Circle", 1)
	setFallback(t, ctx, pool, agentID, fallback)
	setCap(t, ctx, pool, homeID, 1)
	// One run already on the home, on another issue so the per-issue
	// serialisation does not confound the cap.
	other, _, _, _ := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "PFULLB", number: 720004, webhook: true}, StatusTodo)
	_ = createLifecycleTask(t, ctx, pool, agentID, homeID, util.UUIDToString(other.ID), "running")
	taskID := createLifecycleTask(t, ctx, pool, agentID, homeID, issueID, "queued")

	task, _ := svc.Queries.GetAgentTask(ctx, util.MustParseUUID(taskID))
	if !svc.MaybeDispatchToWebhook(ctx, task) {
		t.Fatal("expected true (webhook home, waiting)")
	}
	row := taskRow(t, ctx, svc.Queries, taskID)
	if row.Status != "queued" || row.DispatchedAt.Valid {
		t.Fatalf("task status=%s dispatched_at valid=%v, want queued with no dispatched_at (P4)", row.Status, row.DispatchedAt.Valid)
	}
	var notes int
	pool.QueryRow(ctx, `SELECT count(*) FROM comment WHERE issue_id = $1`, issueID).Scan(&notes)
	if notes != 0 {
		t.Errorf("a wait must leave no note; found %d", notes)
	}
}

// The drain: with the home capped at 1 only the oldest queued task goes;
// raising the cap lets the next one through, while a task queued behind a
// running sibling on the same (issue, agent) stays serialised. (Two
// PENDING — queued or dispatched — tasks per (issue, agent) are already
// impossible, the unique index idx_one_pending_task_per_issue_agent, so the
// sibling is queued once the first one is running.)
func TestDrainPlacesOldestEligibleAndSerialises(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	withWebhookRuntimeFlag(t)
	svc := NewTaskService(db.New(pool), pool, nil, events.New())
	issueA, _, agentID, homeID := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "PDRN", number: 720005, webhook: true}, StatusTodo)
	issueB, _, _, _ := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "PDRNB", number: 720006, webhook: true}, StatusTodo)
	setCap(t, ctx, pool, homeID, 1)
	a1 := createLifecycleTask(t, ctx, pool, agentID, homeID, util.UUIDToString(issueA.ID), "queued")
	time.Sleep(5 * time.Millisecond)
	b1 := createLifecycleTask(t, ctx, pool, agentID, homeID, util.UUIDToString(issueB.ID), "queued")

	if n := svc.DrainWebhookQueue(ctx); n != 1 {
		t.Fatalf("first drain placed %d, want 1 (cap 1)", n)
	}
	if got := taskRow(t, ctx, svc.Queries, a1).Status; got != "dispatched" {
		t.Fatalf("oldest task a1 = %s, want dispatched", got)
	}
	if got := taskRow(t, ctx, svc.Queries, b1).Status; got != "queued" {
		t.Errorf("b1 = %s, want queued (cap)", got)
	}

	// a1 starts; a second task on issue A queues behind it.
	markTaskStarted(t, ctx, pool, a1)
	a2 := createLifecycleTask(t, ctx, pool, agentID, homeID, util.UUIDToString(issueA.ID), "queued")

	// Raise the cap: the next drain takes b1; a2 stays serialised behind a1.
	setCap(t, ctx, pool, homeID, 5)
	if n := svc.DrainWebhookQueue(ctx); n != 1 {
		t.Fatalf("second drain placed %d, want 1", n)
	}
	if got := taskRow(t, ctx, svc.Queries, b1).Status; got != "dispatched" {
		t.Errorf("b1 = %s, want dispatched", got)
	}
	if got := taskRow(t, ctx, svc.Queries, a2).Status; got != "queued" {
		t.Errorf("a2 = %s, want still queued behind a1", got)
	}
}

// The third writer: dispatch_timeout marks the task's runtime down for the
// cool-down; any other reason leaves it alone.
func TestDispatchTimeoutMarksRuntimeDown(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	svc := NewTaskService(db.New(pool), pool, nil, events.New())
	issue, _, agentID, runtimeID := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "PDTO", number: 720007, webhook: true}, StatusTodo)
	taskID := createLifecycleTask(t, ctx, pool, agentID, runtimeID, util.UUIDToString(issue.ID), "failed")

	task := taskRow(t, ctx, svc.Queries, taskID)
	task.FailureReason = pgtype.Text{String: "timeout", Valid: true}
	svc.markRuntimeDownForTask(ctx, task)
	r, _ := svc.Queries.GetAgentRuntime(ctx, util.MustParseUUID(runtimeID))
	if r.DownUntil.Valid {
		t.Fatal("timeout must not mark the runtime down")
	}

	task.FailureReason = pgtype.Text{String: "dispatch_timeout", Valid: true}
	svc.markRuntimeDownForTask(ctx, task)
	r, _ = svc.Queries.GetAgentRuntime(ctx, util.MustParseUUID(runtimeID))
	if !r.DownUntil.Valid {
		t.Fatal("dispatch_timeout must mark the runtime down")
	}
	if until := time.Until(r.DownUntil.Time); until < 4*time.Minute || until > 6*time.Minute {
		t.Errorf("down_until in %s, want ~5 min", until)
	}
	if r.DownReason.String != dispatchTimeoutDownReason {
		t.Errorf("down_reason = %q", r.DownReason.String)
	}
	if runtimeAvailable(r, time.Now()) {
		t.Error("a runtime in cool-down must read as unavailable")
	}
}
