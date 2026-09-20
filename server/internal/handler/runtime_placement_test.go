package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Runtime placement on the API (dev-command-center design 2026-09-20):
// the cap on PATCH /api/runtimes/:id, PUT /api/runtimes/order, and
// fallback_runtime_ids on PUT /api/agents/:id. DB-backed; skips without.

// placementWebhookRuntime registers a webhook runtime in the test workspace,
// owned by the test user, with a dispatch_order.
func placementWebhookRuntime(t *testing.T, name string, order int32) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_runtime (workspace_id, daemon_id, name, runtime_mode, provider, status, device_info, metadata, last_seen_at, visibility, owner_id, webhook_url, webhook_secret, webhook_event_type, dispatch_order)
		VALUES ($1, $2, $3, 'webhook', 'claude', 'online', 'x', '{}'::jsonb, now(), 'private', $4, 'http://127.0.0.1:1/x', 'sec', 'task.dispatched', $5)
		RETURNING id`, testWorkspaceID, "daemon-"+name, name, testUserID, order).Scan(&id); err != nil {
		t.Fatalf("create runtime %s: %v", name, err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_runtime WHERE id = $1`, id) })
	return id
}

func patchRuntimeRaw(runtimeID, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/runtimes/"+runtimeID, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", testUserID)
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	req = withURLParam(req, "runtimeId", runtimeID)
	testHandler.UpdateAgentRuntime(w, req)
	return w
}

func TestUpdateAgentRuntime_MaxConcurrentTasksTriState(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	rt := placementWebhookRuntime(t, "cap-rt", 0)

	for _, bad := range []string{`{"max_concurrent_tasks": -1}`, `{"max_concurrent_tasks": 1.5}`, `{"max_concurrent_tasks": "3"}`} {
		if w := patchRuntimeRaw(rt, bad); w.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d: %s", bad, w.Code, w.Body.String())
		}
	}

	w := patchRuntimeRaw(rt, `{"max_concurrent_tasks": 3}`)
	if w.Code != http.StatusOK {
		t.Fatalf("set 3: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp AgentRuntimeResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.MaxConcurrentTasks == nil || *resp.MaxConcurrentTasks != 3 {
		t.Fatalf("max_concurrent_tasks = %v, want 3", resp.MaxConcurrentTasks)
	}

	// Omitted leaves it alone.
	w = patchRuntimeRaw(rt, `{"custom_name": "Cap RT"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("rename: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp = AgentRuntimeResponse{}
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp.MaxConcurrentTasks == nil || *resp.MaxConcurrentTasks != 3 {
		t.Fatalf("after an unrelated PATCH max_concurrent_tasks = %v, want 3 (omitted = unchanged)", resp.MaxConcurrentTasks)
	}

	// 0 is out of the rotation; null is no limit.
	w = patchRuntimeRaw(rt, `{"max_concurrent_tasks": 0}`)
	resp = AgentRuntimeResponse{}
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if w.Code != http.StatusOK || resp.MaxConcurrentTasks == nil || *resp.MaxConcurrentTasks != 0 {
		t.Fatalf("set 0: code %d, max_concurrent_tasks = %v", w.Code, resp.MaxConcurrentTasks)
	}
	w = patchRuntimeRaw(rt, `{"max_concurrent_tasks": null}`)
	resp = AgentRuntimeResponse{}
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if w.Code != http.StatusOK || resp.MaxConcurrentTasks != nil {
		t.Fatalf("clear: code %d, max_concurrent_tasks = %v, want null", w.Code, resp.MaxConcurrentTasks)
	}
}

func putOrder(ids []string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := newRequest(http.MethodPut, "/api/runtimes/order", map[string]any{"workspace_id": testWorkspaceID, "runtime_ids": ids})
	testHandler.ReorderAgentRuntimes(w, req)
	return w
}

func TestReorderAgentRuntimes(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	a := placementWebhookRuntime(t, "order-a", 0)
	b := placementWebhookRuntime(t, "order-b", 1)
	c := placementWebhookRuntime(t, "order-c", 2)

	if w := putOrder([]string{a, b}); w.Code != http.StatusBadRequest {
		t.Errorf("partial list: expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if w := putOrder([]string{a, b, c, testRuntimeID}); w.Code != http.StatusBadRequest {
		t.Errorf("foreign (daemon) id: expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if w := putOrder([]string{a, a, b}); w.Code != http.StatusBadRequest {
		t.Errorf("duplicate: expected 400, got %d: %s", w.Code, w.Body.String())
	}

	w := putOrder([]string{c, b, a})
	if w.Code != http.StatusOK {
		t.Fatalf("reverse: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var out []AgentRuntimeResponse
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 || out[0].ID != c || out[1].ID != b || out[2].ID != a {
		t.Fatalf("order after reverse: %v", out)
	}
	for i, r := range out {
		if r.DispatchOrder != int32(i) {
			t.Errorf("%s dispatch_order = %d, want %d", r.Name, r.DispatchOrder, i)
		}
	}
}

func TestUpdateAgent_FallbackRuntimeIDs(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	fb := placementWebhookRuntime(t, "fallback-rt", 0)
	var agentID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent (workspace_id, name, description, runtime_mode, runtime_config, runtime_id, visibility, max_concurrent_tasks, owner_id, instructions, custom_env, custom_args)
		VALUES ($1, 'fallback-agent', '', 'cloud', '{}'::jsonb, $2, 'private', 1, $3, '', '{}'::jsonb, '[]'::jsonb)
		RETURNING id`, testWorkspaceID, testRuntimeID, testUserID).Scan(&agentID); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent WHERE id = $1`, agentID) })

	put := func(body map[string]any) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := newRequest(http.MethodPut, "/api/agents/"+agentID, body)
		req = withURLParam(req, "id", agentID)
		testHandler.UpdateAgent(w, req)
		return w
	}

	// The test runtime is a daemon (cloud) runtime: not a valid fallback.
	if w := put(map[string]any{"fallback_runtime_ids": []string{testRuntimeID}}); w.Code != http.StatusBadRequest {
		t.Errorf("daemon runtime as fallback: expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if w := put(map[string]any{"fallback_runtime_ids": []string{"not-a-uuid"}}); w.Code != http.StatusBadRequest {
		t.Errorf("bad uuid: expected 400, got %d: %s", w.Code, w.Body.String())
	}

	w := put(map[string]any{"fallback_runtime_ids": []string{fb}})
	if w.Code != http.StatusOK {
		t.Fatalf("set: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp AgentResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.FallbackRuntimeIDs) != 1 || resp.FallbackRuntimeIDs[0] != fb {
		t.Fatalf("fallback_runtime_ids = %v, want [%s]", resp.FallbackRuntimeIDs, fb)
	}

	// Omitted leaves it alone.
	w = put(map[string]any{"description": "x"})
	resp = AgentResponse{}
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if w.Code != http.StatusOK || len(resp.FallbackRuntimeIDs) != 1 {
		t.Fatalf("after an unrelated PUT: code %d, fallback_runtime_ids = %v", w.Code, resp.FallbackRuntimeIDs)
	}

	// [] pins; the wire value is [] and never null.
	w = put(map[string]any{"fallback_runtime_ids": []string{}})
	if w.Code != http.StatusOK {
		t.Fatalf("pin: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"fallback_runtime_ids":[]`) {
		t.Errorf("pinned agent must serialise [] — body: %s", w.Body.String())
	}
}

func TestProgressTaskFromRow_RuntimeAndOffHome(t *testing.T) {
	home := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	other := pgtype.UUID{Bytes: [16]byte{2}, Valid: true}
	at := progressTaskFromRow(db.ListTaskProgressByIssueRow{RuntimeID: home, AgentRuntimeID: home, RuntimeName: "Local PC"})
	if at.OffHome || at.RuntimeName != "Local PC" {
		t.Errorf("at home: %+v", at)
	}
	off := progressTaskFromRow(db.ListTaskProgressByIssueRow{RuntimeID: other, AgentRuntimeID: home, RuntimeName: "CircleCI"})
	if !off.OffHome || off.RuntimeName != "CircleCI" {
		t.Errorf("off home: %+v", off)
	}
	none := progressTaskFromRow(db.ListTaskProgressByIssueRow{RuntimeID: other, RuntimeName: ""})
	if none.OffHome {
		t.Error("an agent with no runtime is not off home")
	}
}
