package service

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// webhookCallbackTokenTTL is the lifetime of the per-task JWT the receiver
// uses on /messages, /usage, /complete, /fail. 60 minutes covers the
// GH Actions 30-min job ceiling with comfortable margin while still rotating
// often enough that a leaked token has bounded value.
const webhookCallbackTokenTTL = 60 * time.Minute

// webhookDispatchTimeout caps each individual POST attempt. DispatchWithRetry
// makes 3 attempts so the total worst-case wait is ~3 × (5s timeout) + backoffs
// ≈ 20s, kept off the request-creating goroutine.
const webhookDispatchTimeout = 5 * time.Second

// webhookCallbackBaseURL is read from env so the URL the receiver calls back
// to can differ from whatever URL Multica was reached on (e.g. external
// hostname when Multica is behind a tunnel). Falls back to MULTICA_PUBLIC_URL
// then a sensible default that's only useful for unit tests.
func webhookCallbackBaseURL() string {
	if v := os.Getenv("MULTICA_WEBHOOK_CALLBACK_URL"); v != "" {
		return v
	}
	if v := os.Getenv("MULTICA_PUBLIC_URL"); v != "" {
		return v + "/api/daemon"
	}
	return "http://localhost:8080/api/daemon"
}

// webhookHTTPClient is the http.Client used for outbound dispatches. Exposed
// as a package variable so tests can swap in an httptest.NewServer-aware
// transport, but immutable in production.
var webhookHTTPClient = &http.Client{Timeout: webhookDispatchTimeout}

// MaybeDispatchToWebhook checks whether the task's runtime is webhook-mode
// and — if so — fires a webhook dispatch in a background goroutine. Returns
// true when a dispatch was started (caller should skip the normal
// notifyTaskAvailable wakeup), false when the runtime is local and the
// existing daemon-claim path should be taken instead.
//
// The dispatch is fire-and-forget at the goroutine level: failures are
// logged but don't propagate back to the request that created the task.
// DispatchWithRetry's exponential backoff handles transient receiver
// outages; permanent failures land in the task's failure_reason via a
// downstream queue-failer (left for Task A10, currently the task just sits
// claimed-but-unstarted until the operator intervenes — same failure mode
// as a daemon that's claimed a task then crashed).
//
// Disabled by default; set MULTICA_WEBHOOK_RUNTIME=1 to enable. With the
// flag off, webhook runtimes can still be registered but dispatch is a
// no-op (the task stays queued forever) — useful for staging where you
// want the schema in place but don't yet want the side-effects.
func (s *TaskService) MaybeDispatchToWebhook(ctx context.Context, task db.AgentTaskQueue) bool {
	if os.Getenv("MULTICA_WEBHOOK_RUNTIME") != "1" {
		return false
	}

	runtime, err := s.Queries.GetAgentRuntime(ctx, task.RuntimeID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("webhook: load runtime", "err", err, "task_id", util.UUIDToString(task.ID))
		}
		return false
	}
	if runtime.RuntimeMode != "webhook" {
		return false
	}
	if !runtime.WebhookUrl.Valid || runtime.WebhookUrl.String == "" {
		slog.Error("webhook: runtime in mode=webhook but no webhook_url", "runtime_id", util.UUIDToString(runtime.ID))
		return false
	}

	taskID := util.UUIDToString(task.ID)
	runtimeID := util.UUIDToString(runtime.ID)

	// Mark the task as dispatched before firing the webhook so the
	// receiver's /start callback (which requires status='dispatched')
	// finds the correct state. If the webhook ultimately fails, the
	// stale-task sweeper will time out the dispatched row.
	dispatched, err := s.Queries.DispatchAgentTask(ctx, task.ID)
	if err != nil {
		slog.Error("webhook: set task dispatched", "err", err, "task_id", taskID)
		return false
	}

	tok, err := auth.IssueCallbackToken(auth.JWTSecret(), taskID, runtimeID, webhookCallbackTokenTTL)
	if err != nil {
		slog.Error("webhook: issue callback token", "err", err, "task_id", taskID)
		return false
	}

	payload := map[string]any{
		"task":     dispatched,
		"callback": map[string]any{"url": webhookCallbackBaseURL(), "token": tok},
	}
	target := DispatchTarget{
		URL:       runtime.WebhookUrl.String,
		Secret:    runtime.WebhookSecret.String,
		RuntimeID: runtimeID,
		EventType: runtime.WebhookEventType.String,
	}

	go func() {
		// Detached context — the originating HTTP request may finish
		// before this dispatch completes; we don't want its cancellation
		// to abort the outbound POST.
		dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		err := DispatchWithRetry(dctx, target, payload, webhookHTTPClient, time.Now,
			RetryPolicy{MaxAttempts: 3, InitialBackoff: 1 * time.Second})
		if err != nil {
			slog.Error("webhook: dispatch failed after retries",
				"err", err, "task_id", taskID, "runtime_id", runtimeID, "url", target.URL)
			return
		}
		slog.Info("webhook: dispatched", "task_id", taskID, "runtime_id", runtimeID)
	}()

	return true
}
