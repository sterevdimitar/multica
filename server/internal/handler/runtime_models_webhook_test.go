package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	agentpkg "github.com/multica-ai/multica/server/pkg/agent"
)

// createWebhookTestRuntime seeds a webhook-mode runtime in the handler test
// workspace. status is a parameter because the point of several of these
// tests is that a webhook runtime's status must not gate model discovery.
func createWebhookTestRuntime(t *testing.T, status string) string {
	t.Helper()

	var runtimeID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO agent_runtime (
			workspace_id, daemon_id, name, runtime_mode, provider, status,
			device_info, metadata, owner_id, webhook_url, last_seen_at
		)
		VALUES ($1, NULL, $2, 'webhook', $3, $4, 'Webhook test runtime', '{}'::jsonb, $5, $6, now())
		RETURNING id
	`, testWorkspaceID, "Webhook Models Runtime "+status, "webhook_models_test_"+status, status,
		testUserID, "https://example.test/dispatch").Scan(&runtimeID); err != nil {
		t.Fatalf("failed to create webhook test runtime: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_runtime WHERE id = $1`, runtimeID)
	})
	return runtimeID
}

func initiateModels(t *testing.T, runtimeID string) (*httptest.ResponseRecorder, ModelListRequest) {
	t.Helper()

	req := withURLParam(newRequest(http.MethodPost, "/api/runtimes/"+runtimeID+"/models", nil), "runtimeId", runtimeID)
	rec := httptest.NewRecorder()
	testHandler.InitiateListModels(rec, req)

	var out ModelListRequest
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode response: %v (body %s)", err, rec.Body.String())
		}
	}
	return rec, out
}

// TestInitiateListModels_WebhookRuntimeCompletesInline is the regression
// test for the empty model picker.
//
// A webhook runtime has no daemon, so nothing ever popped the pending request
// this endpoint used to create: it aged into `timeout` after 30 seconds and
// the picker rendered "No models available" — for every provider, Anthropic
// included, which is why the symptom looked like a model-list bug rather than
// a runtime-mode one. The answer must be terminal on the first response.
func TestInitiateListModels_WebhookRuntimeCompletesInline(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentpkg.ResetEngineCatalogCacheForTest()
	t.Cleanup(agentpkg.ResetEngineCatalogCacheForTest)
	// No proxy on this host: the catalogue degrades to Anthropic-only, which
	// is precisely the failure mode that must still produce a usable picker.
	t.Setenv("LITELLM_INTERNAL_URL", "http://127.0.0.1:1")

	rec, out := initiateModels(t, createWebhookTestRuntime(t, "online"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if out.Status != ModelListCompleted {
		t.Fatalf("status = %q, want %q — a pending reply is never claimed for a webhook runtime",
			out.Status, ModelListCompleted)
	}
	if !out.Supported {
		t.Error("supported = false; the runner passes task.agent.model to the CLI verbatim")
	}
	if len(out.Models) == 0 {
		t.Fatal("no models returned; an empty picker must never be the failure mode")
	}
	var sawAnthropic bool
	for _, m := range out.Models {
		if m.Provider == "anthropic" {
			sawAnthropic = true
		}
	}
	if !sawAnthropic {
		t.Error("Anthropic models missing with the proxy unreachable")
	}
}

// TestInitiateListModels_WebhookRuntimeIgnoresStatus pins the placement of
// the webhook branch ahead of the online check. A webhook runtime's status is
// not evidence about model discovery — the catalogue is assembled
// server-side — and gating on it would reintroduce the empty picker for the
// one runtime kind whose liveness the server cannot observe.
func TestInitiateListModels_WebhookRuntimeIgnoresStatus(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentpkg.ResetEngineCatalogCacheForTest()
	t.Cleanup(agentpkg.ResetEngineCatalogCacheForTest)
	t.Setenv("LITELLM_INTERNAL_URL", "http://127.0.0.1:1")

	rec, out := initiateModels(t, createWebhookTestRuntime(t, "offline"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for an offline webhook runtime (body %s)", rec.Code, rec.Body.String())
	}
	if out.Status != ModelListCompleted || len(out.Models) == 0 {
		t.Fatalf("got status=%q models=%d, want a completed non-empty result", out.Status, len(out.Models))
	}
}

// TestInitiateListModels_DaemonRuntimeStillEnqueues guards the untouched
// half: a daemon-backed runtime must still get a pending request that its
// next heartbeat claims. This fork is also run with a local Claude daemon.
func TestInitiateListModels_DaemonRuntimeStillEnqueues(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	rec, out := initiateModels(t, handlerTestRuntimeID(t))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if out.Status != ModelListPending {
		t.Fatalf("status = %q, want %q for a daemon-backed runtime", out.Status, ModelListPending)
	}
	if len(out.Models) != 0 {
		t.Errorf("daemon path returned %d models inline; discovery belongs to the daemon", len(out.Models))
	}
	stored, err := testHandler.ModelListStore.Get(context.Background(), out.ID)
	if err != nil || stored == nil {
		t.Fatalf("request not in the store: %+v err=%v", stored, err)
	}
}

// TestInitiateListModels_WebhookRuntimeDoesNotEnqueue confirms the webhook
// path leaves nothing behind to time out. A stored request would age into
// `timeout` and, if the UI ever polled it, resurrect the original bug.
func TestInitiateListModels_WebhookRuntimeDoesNotEnqueue(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentpkg.ResetEngineCatalogCacheForTest()
	t.Cleanup(agentpkg.ResetEngineCatalogCacheForTest)
	t.Setenv("LITELLM_INTERNAL_URL", "http://127.0.0.1:1")

	runtimeID := createWebhookTestRuntime(t, "online")
	_, out := initiateModels(t, runtimeID)

	stored, err := testHandler.ModelListStore.Get(context.Background(), out.ID)
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	if stored != nil {
		t.Errorf("webhook request was persisted (%+v); it has no daemon to claim it", stored)
	}
	has, err := testHandler.ModelListStore.HasPending(context.Background(), runtimeID)
	if err != nil {
		t.Fatalf("has pending: %v", err)
	}
	if has {
		t.Error("webhook runtime left a pending request behind")
	}
}
