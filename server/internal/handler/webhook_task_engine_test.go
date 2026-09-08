package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	agentpkg "github.com/multica-ai/multica/server/pkg/agent"
)

// Engine-pairing tests for the webhook dispatch payload. Kept beside the other
// webhook-runtime tests so the fork's patch stays cleanly separable across
// upstream rebases. They reuse the daemon_test.go harness globals and the
// helpers in webhook_task_delivery_test.go.

// startStubEngineProxy stands up a proxy that advertises modelID over the
// OpenAI-shaped /v1/models endpoint, points the engine catalogue at it, and
// clears the memoised catalogue on both sides of the test.
func startStubEngineProxy(t *testing.T, modelID, publicURL string) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"id":"` + modelID + `","object":"model"}]}`))
	}))
	t.Cleanup(srv.Close)

	t.Setenv("LITELLM_INTERNAL_URL", srv.URL)
	t.Setenv("LITELLM_PUBLIC_URL", publicURL)
	t.Setenv("LITELLM_MASTER_KEY", "stub-key")
	agentpkg.ResetEngineCatalogCacheForTest()
	t.Cleanup(agentpkg.ResetEngineCatalogCacheForTest)
}

// dispatchWebhookAndCaptureAgent runs one webhook dispatch for an agent whose
// model is model, and returns the `task.agent` object the receiver was sent.
func dispatchWebhookAndCaptureAgent(t *testing.T, issueNumber int32, daemonID, model string) map[string]any {
	t.Helper()
	ctx := context.Background()

	bodies := make(chan []byte, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		select {
		case bodies <- body:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	runtimeID := registerWebhookRuntimePointingAt(t, daemonID, srv.URL)
	agentID := createHandlerTestAgent(t, "Webhook Engine Agent "+daemonID, nil)
	if _, err := testPool.Exec(ctx,
		`UPDATE agent SET runtime_id = $1, model = $2 WHERE id = $3`, runtimeID, model, agentID); err != nil {
		t.Fatalf("bind agent to webhook runtime: %v", err)
	}

	var issueID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, title, status, priority, creator_id, creator_type, number, position, assignee_type, assignee_id)
		VALUES ($1, 'webhook engine fixture', 'in_progress', 'none', $2, 'member', $3, 0, 'agent', $4)
		RETURNING id
	`, testWorkspaceID, testUserID, issueNumber, agentID).Scan(&issueID); err != nil {
		t.Fatalf("create issue: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, issueID) })

	var taskID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, created_at)
		VALUES ($1, $2, $3, 'queued', 0, now())
		RETURNING id
	`, agentID, runtimeID, issueID).Scan(&taskID); err != nil {
		t.Fatalf("create queued task: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID) })

	task, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(taskID))
	if err != nil {
		t.Fatalf("load queued task: %v", err)
	}
	if !testHandler.TaskService.MaybeDispatchToWebhook(ctx, task) {
		t.Fatal("MaybeDispatchToWebhook returned false; expected the webhook branch")
	}

	var body []byte
	select {
	case body = <-bodies:
	case <-time.After(10 * time.Second):
		t.Fatal("webhook receiver never got a dispatch")
	}

	var payload struct {
		Task struct {
			Agent map[string]any `json:"agent"`
		} `json:"task"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode dispatch payload: %v (body %s)", err, body)
	}
	return payload.Task.Agent
}

func customEnvBaseURL(t *testing.T, agent map[string]any) (string, bool) {
	t.Helper()
	env, ok := agent["custom_env"].(map[string]any)
	if !ok {
		return "", false
	}
	v, ok := env["ANTHROPIC_BASE_URL"]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("ANTHROPIC_BASE_URL is %T, want string", v)
	}
	return s, true
}

// TestWebhookDispatch_ProxiedModelCarriesItsBaseURL is the pairing contract at
// the only place it can be enforced: the payload the runner actually reads.
//
// The user picks a model and nothing else. If the base URL did not follow, the
// run would reach Anthropic carrying a proxy catalogue name and die as "the
// model does not exist" — a message that sends whoever debugs it after the
// model string rather than the endpoint.
func TestWebhookDispatch_ProxiedModelCarriesItsBaseURL(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	t.Setenv("MULTICA_WEBHOOK_RUNTIME", "1")
	startStubEngineProxy(t, "dcc-glm-high", "https://example.test/llm")

	agent := dispatchWebhookAndCaptureAgent(t, 999411, "wh-engine-daemon-a", "dcc-glm-high")

	if got := agent["model"]; got != "dcc-glm-high" {
		t.Errorf("model = %v, want dcc-glm-high", got)
	}
	base, ok := customEnvBaseURL(t, agent)
	if !ok {
		t.Fatalf("ANTHROPIC_BASE_URL missing from custom_env: %v", agent["custom_env"])
	}
	// The PUBLIC URL: the agent runs on a GitHub Actions runner, so the
	// compose-network address the server fetched the catalogue from is
	// unreachable from where the value is used.
	if base != "https://example.test/llm" {
		t.Errorf("ANTHROPIC_BASE_URL = %q, want the public proxy URL", base)
	}
}

// TestWebhookDispatch_AnthropicModelCarriesEmptyBaseURL covers the other half
// of the switch. Empty is Anthropic — deliberately not spelled out as
// https://api.anthropic.com, which is what the runner and the pipeline's
// declared config both expect.
func TestWebhookDispatch_AnthropicModelCarriesEmptyBaseURL(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	t.Setenv("MULTICA_WEBHOOK_RUNTIME", "1")
	startStubEngineProxy(t, "dcc-glm-high", "https://example.test/llm")

	agent := dispatchWebhookAndCaptureAgent(t, 999412, "wh-engine-daemon-b", "claude-sonnet-4-6")

	base, ok := customEnvBaseURL(t, agent)
	if !ok {
		t.Fatalf("ANTHROPIC_BASE_URL missing from custom_env: %v", agent["custom_env"])
	}
	if base != "" {
		t.Errorf("ANTHROPIC_BASE_URL = %q, want empty for an Anthropic model", base)
	}
}
