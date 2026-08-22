package service

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Cancel propagation to webhook runtimes.
//
// On a daemon runtime, cancelling a task kills the child process. On a
// webhook runtime the DB row flips and the remote run — a GitHub Actions job,
// say — keeps executing to its budget. This closes that gap by firing a
// `task.cancelled` event at the runtime's webhook_url, which the receiver
// turns into whatever "kill the run" means on its substrate.
//
// Invariants that do not surface by running the code:
//
//   - DB FIRST, event best-effort. Multica marks the task cancelled before
//     this is called and that row is the source of truth. A cancel event that
//     never lands costs at most one bounded run completing uselessly, which
//     is the status quo. Errors are logged, never returned.
//   - Three attempts and stop, the same policy as dispatch. A cancel storm is
//     worse than a wasted run: never escalate, never re-queue.
//   - The event rides the SAME HMAC signer and dispatcher as dispatch. An
//     unsigned cancel path is a denial-of-service primitive against the
//     pipeline.
//   - The dispatch envelope is unchanged and gains no field. ABSENCE of
//     "event" means dispatch; receivers branch on its presence. Older
//     translators reject an unknown body with a 4xx that the 3-attempt retry
//     absorbs — acceptable during the deploy window, which is why the fork
//     may ship before the translator.

// CancelEventType is the discriminator carried in the envelope's "event"
// field. The receiver keys on this exact string.
const CancelEventType = "task.cancelled"

// cancelEnvelope is the whole body. Deliberately minimal: the receiver
// resolves the run from the task id, and anything else would be a second
// source of truth for state the DB already owns.
type cancelEnvelope struct {
	Event string          `json:"event"`
	Task  cancelEventTask `json:"task"`
}

type cancelEventTask struct {
	ID        string `json:"id"`
	RuntimeID string `json:"runtime_id"`
}

// shouldDispatchCancel is the gate, split out so it is testable without a
// database. It answers: is there a remote run to kill?
//
// dispatchedAt, not status: the task row this receives has ALREADY been
// flipped to "cancelled", so its status says nothing about whether it ever
// reached a runtime. A queued task was never dispatched and has no run.
func shouldDispatchCancel(flagOn bool, runtimeMode, webhookURL string, dispatched bool) bool {
	return flagOn && runtimeMode == "webhook" && webhookURL != "" && dispatched
}

// MaybeDispatchCancelToWebhook fires a task.cancelled event at the task's
// runtime webhook_url. Fire-and-forget: errors are logged, never returned.
//
// No-op unless the MULTICA_WEBHOOK_RUNTIME flag is on, the runtime is
// webhook-mode with a URL, and the task had been dispatched.
func (s *TaskService) MaybeDispatchCancelToWebhook(ctx context.Context, task db.AgentTaskQueue) {
	if os.Getenv("MULTICA_WEBHOOK_RUNTIME") != "1" {
		return
	}
	if !task.DispatchedAt.Valid {
		return
	}
	runtime, err := s.Queries.GetAgentRuntime(ctx, task.RuntimeID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("webhook cancel: load runtime", "err", err, "task_id", util.UUIDToString(task.ID))
		}
		return
	}
	if !shouldDispatchCancel(true, runtime.RuntimeMode, runtime.WebhookUrl.String, task.DispatchedAt.Valid) {
		return
	}

	target := DispatchTarget{
		URL:       runtime.WebhookUrl.String,
		Secret:    runtime.WebhookSecret.String,
		RuntimeID: util.UUIDToString(runtime.ID),
		EventType: runtime.WebhookEventType.String,
	}
	dispatchCancelEvent(target, util.UUIDToString(task.ID), webhookHTTPClient)
}

// dispatchCancelEvent POSTs the envelope in a background goroutine. Split
// from MaybeDispatchCancelToWebhook so the wire format and retry behaviour
// can be tested against an httptest receiver without a database.
//
// It returns as soon as the goroutine is started: the caller is a cancel
// path a human is waiting on, and the remote kill is cleanup, not part of
// the transaction.
func dispatchCancelEvent(target DispatchTarget, taskID string, client *http.Client) {
	payload := cancelEnvelope{
		Event: CancelEventType,
		Task:  cancelEventTask{ID: taskID, RuntimeID: target.RuntimeID},
	}
	go func() {
		dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := DispatchWithRetry(dctx, target, payload, client, time.Now,
			RetryPolicy{MaxAttempts: 3, InitialBackoff: 1 * time.Second}); err != nil {
			// Logged and dropped. Retrying past this point risks a cancel
			// storm against a receiver that is already unwell, to save a run
			// that is already bounded.
			slog.Error("webhook cancel: dispatch failed after retries",
				"err", err, "task_id", taskID, "runtime_id", target.RuntimeID, "url", target.URL)
			return
		}
		slog.Info("webhook cancel: dispatched", "task_id", taskID, "runtime_id", target.RuntimeID)
	}()
}
