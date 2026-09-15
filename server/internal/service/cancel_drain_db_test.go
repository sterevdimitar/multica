package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A cancel frees the same capacity a completion or a failure does, so it must
// drain the webhook queue the same way. Card 239 (2026-09-14): the
// conflict-agent (max_concurrent_tasks = 1) was busy on card 240 when card
// 239's summon arrived, so that task was parked `queued`; seven seconds later
// the run on 240 was cancelled by a push restart, the agent went idle, and the
// queued task sat until the two-hour sweeper expired it. Only CompleteTask and
// FailTask drained; CancelTaskWithResult did not.
// Needs DATABASE_URL (port 5439 on this machine); skips without it.

func TestCancelTaskDrainsTheWebhookQueue(t *testing.T) {
	pool := newPushRestartPool(t)
	ctx := context.Background()
	t.Setenv("MULTICA_WEBHOOK_RUNTIME", "1")

	// The runtime's webhook target: records every task id it is asked to run.
	dispatched := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		dispatched <- string(body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	f := createPushRestartFixture(t, ctx, pool)
	if _, err := pool.Exec(ctx, `UPDATE agent_runtime SET runtime_mode = 'webhook', webhook_url = $2, webhook_secret = 'test', webhook_event_type = 'task' WHERE id = $1`,
		f.runtimeID, srv.URL); err != nil {
		t.Fatal(err)
	}
	svc := pushRestartServices(pool).TaskSvc

	ahead := f.addTask(t, ctx, pool, "review-agent", "running")
	behind := f.addTask(t, ctx, pool, "review-agent", "queued")

	if _, err := svc.CancelTaskWithResult(ctx, ahead, CancelTaskOptions{}); err != nil {
		t.Fatalf("CancelTaskWithResult: %v", err)
	}

	if s := taskStatus(t, ctx, pool, ahead); s != "cancelled" {
		t.Fatalf("cancelled task status = %q, want cancelled", s)
	}
	if s := taskStatus(t, ctx, pool, behind); s != "dispatched" {
		t.Fatalf("queued task behind the cancelled one: status = %q, want dispatched", s)
	}
	if !webhookSaw(t, dispatched, uuidString(t, behind)) {
		t.Fatalf("the runtime was never asked to run the queued task")
	}
}

// webhookSaw waits for a webhook body carrying the given task id.
func webhookSaw(t *testing.T, bodies <-chan string, taskID string) bool {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case b := <-bodies:
			if strings.Contains(b, taskID) {
				return true
			}
		case <-deadline:
			return false
		}
	}
}
