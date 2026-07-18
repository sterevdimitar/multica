package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// Webhook-runtime handler tests. Extracted into their own file (rather than
// living in daemon_test.go) so the webhook patch stays cleanly separable
// across upstream rebases. They reuse the daemon_test.go harness globals
// (testHandler, testPool, testWorkspaceID) and newDaemonTokenRequest.

func TestDaemonRegister_WebhookRuntime(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	w := httptest.NewRecorder()
	req := newDaemonTokenRequest("POST", "/api/daemon/register", map[string]any{
		"workspace_id": testWorkspaceID,
		"daemon_id":    "test-daemon-webhook",
		"device_name":  "github-actions-runner-pool",
		"runtimes": []map[string]any{{
			"name":               "GitHub Actions",
			"type":               "claude",
			"version":            "1.0.0",
			"status":             "online",
			"runtime_mode":       "webhook",
			"webhook_url":        "https://translator.example.test/v1/dispatch",
			"webhook_secret":     "s3cr3t-32b-min",
			"webhook_event_type": "multica-task",
		}},
	}, testWorkspaceID, "test-daemon-webhook")

	testHandler.DaemonRegister(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("DaemonRegister webhook: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	runtimes, ok := resp["runtimes"].([]any)
	if !ok || len(runtimes) == 0 {
		t.Fatalf("expected runtimes in response, got %v", resp)
	}
	rt := runtimes[0].(map[string]any)
	runtimeID := rt["id"].(string)
	defer testPool.Exec(context.Background(), `DELETE FROM agent_runtime WHERE id = $1`, runtimeID)

	if got, _ := rt["runtime_mode"].(string); got != "webhook" {
		t.Errorf("response runtime_mode = %q, want %q", got, "webhook")
	}

	// DB row carries the webhook config exactly as registered.
	var (
		runtimeMode      string
		webhookURL       *string
		webhookSecret    *string
		webhookEventType *string
	)
	err := testPool.QueryRow(context.Background(),
		`SELECT runtime_mode, webhook_url, webhook_secret, webhook_event_type FROM agent_runtime WHERE id = $1`,
		runtimeID,
	).Scan(&runtimeMode, &webhookURL, &webhookSecret, &webhookEventType)
	if err != nil {
		t.Fatalf("load runtime row: %v", err)
	}
	if runtimeMode != "webhook" {
		t.Errorf("DB runtime_mode = %q, want %q", runtimeMode, "webhook")
	}
	if webhookURL == nil || *webhookURL != "https://translator.example.test/v1/dispatch" {
		t.Errorf("DB webhook_url = %v, want https://translator.example.test/v1/dispatch", webhookURL)
	}
	if webhookSecret == nil || *webhookSecret != "s3cr3t-32b-min" {
		t.Errorf("DB webhook_secret = %v, want s3cr3t-32b-min", webhookSecret)
	}
	if webhookEventType == nil || *webhookEventType != "multica-task" {
		t.Errorf("DB webhook_event_type = %v, want multica-task", webhookEventType)
	}
}

func TestDaemonRegister_WebhookRuntime_MissingURLRejected(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	w := httptest.NewRecorder()
	req := newDaemonTokenRequest("POST", "/api/daemon/register", map[string]any{
		"workspace_id": testWorkspaceID,
		"daemon_id":    "test-daemon-bad-webhook",
		"device_name":  "test",
		"runtimes": []map[string]any{{
			"name":         "no-url",
			"type":         "claude",
			"version":      "1.0.0",
			"status":       "online",
			"runtime_mode": "webhook",
			// webhook_url intentionally missing
		}},
	}, testWorkspaceID, "test-daemon-bad-webhook")

	testHandler.DaemonRegister(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDaemonRegister_InvalidRuntimeModeRejected(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	w := httptest.NewRecorder()
	req := newDaemonTokenRequest("POST", "/api/daemon/register", map[string]any{
		"workspace_id": testWorkspaceID,
		"daemon_id":    "test-daemon-bad-mode",
		"device_name":  "test",
		"runtimes": []map[string]any{{
			"name":         "bad-mode",
			"type":         "claude",
			"version":      "1.0.0",
			"status":       "online",
			"runtime_mode": "lambda", // not in allowed set
		}},
	}, testWorkspaceID, "test-daemon-bad-mode")

	testHandler.DaemonRegister(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestClaimTaskByRuntime_RejectsWebhookRuntime(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	// Register a webhook runtime via the public path so we exercise the
	// same row a real caller would create.
	wReg := httptest.NewRecorder()
	regBody := map[string]any{
		"workspace_id": testWorkspaceID,
		"daemon_id":    "test-daemon-claim-405",
		"device_name":  "gha",
		"runtimes": []map[string]any{{
			"name":         "GHA",
			"type":         "claude",
			"version":      "1.0.0",
			"status":       "online",
			"runtime_mode": "webhook",
			"webhook_url":  "https://x.test/dispatch",
		}},
	}
	regReq := newDaemonTokenRequest("POST", "/api/daemon/register", regBody, testWorkspaceID, "test-daemon-claim-405")
	testHandler.DaemonRegister(wReg, regReq)
	if wReg.Code != http.StatusOK {
		t.Fatalf("setup register: %d %s", wReg.Code, wReg.Body.String())
	}
	var regResp map[string]any
	json.NewDecoder(wReg.Body).Decode(&regResp)
	runtimeID := regResp["runtimes"].([]any)[0].(map[string]any)["id"].(string)
	defer testPool.Exec(context.Background(), `DELETE FROM agent_runtime WHERE id = $1`, runtimeID)

	// Now hit /claim — must be rejected with 405.
	w := httptest.NewRecorder()
	req := newDaemonTokenRequest("POST", "/api/daemon/runtimes/"+runtimeID+"/tasks/claim", nil, testWorkspaceID, "test-daemon-claim-405")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("runtimeId", runtimeID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	testHandler.ClaimTaskByRuntime(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d: %s", w.Code, w.Body.String())
	}
}
