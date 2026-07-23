package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// LEAK 2 regression tests: a GitHub webhook `ping` (delivered on hook creation)
// must never spawn an autopilot run, and the review pipeline's per-trigger
// pull_request allowlist (event_filters) must admit only opened/synchronize/
// reopened. They reuse the harness + helpers from
// autopilot_webhook_handler_test.go (postWebhook, createWebhookTestAgent,
// createWebhookTestAutopilot, createWebhookTriggerViaHandler,
// createWebhookTriggerWithFilters).

// autopilotRunCount returns how many runs exist for an autopilot.
func autopilotRunCount(t *testing.T, autopilotID string) int {
	t.Helper()
	runs, err := testHandler.Queries.ListAutopilotRuns(context.Background(), db.ListAutopilotRunsParams{
		AutopilotID: parseUUID(autopilotID),
		Limit:       50,
		Offset:      0,
	})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	return len(runs)
}

// TestWebhookHandler_GitHubPingIgnored is the direct LEAK 2 regression: a GitHub
// `ping` delivery (X-GitHub-Event: ping) must record an ignored delivery, return
// 200 (so GitHub does not mark the hook failing), and allocate NO run — even on a
// trigger with no event_filters, which otherwise admits everything.
func TestWebhookHandler_GitHubPingIgnored(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID := createWebhookTestAgent(t, "WebhookPing Agent")
	apID := createWebhookTestAutopilot(t, agentID, "active", "run_only")
	trig := createWebhookTriggerViaHandler(t, apID)

	// GitHub's ping payload shape: zen + hook_id, no repository work.
	w := postWebhook(t, *trig.WebhookToken, map[string]any{
		"zen":     "Non-blocking is better than blocking.",
		"hook_id": 655878302,
	}, map[string]string{"X-GitHub-Event": "ping"})

	if w.Code != http.StatusOK {
		t.Fatalf("ping: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode ping response: %v", err)
	}
	if resp["status"] != "ignored" || resp["reason"] != "github_event_not_actionable" {
		t.Fatalf("expected ignored/github_event_not_actionable, got %#v", resp)
	}
	if resp["github_event"] != "ping" {
		t.Fatalf("expected github_event=ping, got %#v", resp["github_event"])
	}
	if _, ok := resp["run_id"]; ok {
		t.Fatalf("ping response must not include run_id: %#v", resp)
	}
	if n := autopilotRunCount(t, apID); n != 0 {
		t.Fatalf("ping must not create a run, got %d", n)
	}
}

// TestWebhookHandler_NonGitHubDeliveryUnaffectedByPingGuard proves the guard is
// scoped to GitHub deliveries: a generic (no X-GitHub-Event header) delivery whose
// caller-provided event happens to be "ping" is still admitted, since the guard
// only fires on the X-GitHub-Event header.
func TestWebhookHandler_NonGitHubDeliveryUnaffectedByPingGuard(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID := createWebhookTestAgent(t, "WebhookGenericPing Agent")
	apID := createWebhookTestAutopilot(t, agentID, "active", "run_only")
	trig := createWebhookTriggerViaHandler(t, apID)

	// No X-GitHub-Event header → not a GitHub delivery → guard does not apply.
	w := postWebhook(t, *trig.WebhookToken, map[string]any{"event": "ping"}, nil)
	requireAcceptedWebhookResponse(t, w)
}

// TestWebhookHandler_ReviewPipelinePRAllowlist demonstrates the exact LEAK 2
// requirement enforced per trigger via event_filters: only pull_request
// opened/synchronize/reopened admit a run; a GitHub ping and a pull_request
// action outside the allowlist (e.g. closed) are ignored with 200.
func TestWebhookHandler_ReviewPipelinePRAllowlist(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID := createWebhookTestAgent(t, "WebhookPRAllowlist Agent")
	apID := createWebhookTestAutopilot(t, agentID, "active", "run_only")
	trig := createWebhookTriggerWithFilters(t, apID, []WebhookEventFilter{
		{Event: "pull_request", Actions: []string{"opened", "synchronize", "reopened"}},
	})

	// ping → dropped by the universal GitHub guard.
	ping := postWebhook(t, *trig.WebhookToken, map[string]any{"zen": "z", "hook_id": 1}, map[string]string{"X-GitHub-Event": "ping"})
	if ping.Code != http.StatusOK {
		t.Fatalf("ping: expected 200, got %d body=%s", ping.Code, ping.Body.String())
	}
	var pingResp map[string]any
	json.Unmarshal(ping.Body.Bytes(), &pingResp)
	if pingResp["status"] != "ignored" {
		t.Fatalf("ping should be ignored, got %#v", pingResp)
	}

	// pull_request/closed → ignored by event_filters (event_filtered).
	closed := postWebhook(t, *trig.WebhookToken, map[string]any{
		"action":       "closed",
		"pull_request": map[string]any{"number": 7},
	}, map[string]string{"X-GitHub-Event": "pull_request"})
	if closed.Code != http.StatusOK {
		t.Fatalf("pr closed: expected 200, got %d body=%s", closed.Code, closed.Body.String())
	}
	var closedResp map[string]any
	json.Unmarshal(closed.Body.Bytes(), &closedResp)
	if closedResp["status"] != "ignored" || closedResp["reason"] != "event_filtered" {
		t.Fatalf("pr closed should be event_filtered, got %#v", closedResp)
	}

	// No runs yet from the two ignored deliveries.
	if n := autopilotRunCount(t, apID); n != 0 {
		t.Fatalf("ignored deliveries must not create runs, got %d", n)
	}

	// pull_request/opened → admitted.
	opened := postWebhook(t, *trig.WebhookToken, map[string]any{
		"action":       "opened",
		"pull_request": map[string]any{"number": 8},
	}, map[string]string{"X-GitHub-Event": "pull_request"})
	requireAcceptedWebhookResponse(t, opened)

	if n := autopilotRunCount(t, apID); n != 1 {
		t.Fatalf("pull_request/opened must create exactly one run, got %d", n)
	}
}
