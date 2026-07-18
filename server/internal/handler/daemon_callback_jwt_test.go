package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/middleware"
)

// withCallbackAuth wraps `req` so the handler sees the request as having
// passed through DaemonAuth's callback-JWT branch. Used by handler-level
// tests for the URL ↔ claim enforcement (the middleware-level test in
// middleware/daemon_auth_callback_test.go covers the JWT-parsing path).
func withCallbackAuth(req *http.Request, claimTaskID, claimRuntimeID string) *http.Request {
	ctx := middleware.WithCallbackContext(req.Context(), claimTaskID, claimRuntimeID)
	return req.WithContext(ctx)
}

func withChiTaskID(req *http.Request, taskID string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("taskId", taskID)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// createTestTask inserts a dispatched, issue-less task linked to a fresh
// autopilot_run into the test workspace and returns its UUID. The task
// is in 'dispatched' state so StartTask (which transitions
// dispatched→running) succeeds; issue_id is NULL so each call doesn't
// collide on the (workspace_id, issue_number) unique constraint. All
// rows clean themselves up via t.Cleanup.
//
// Matches the fixture shape used by TestStartTask_AutopilotRunOnlyTask_
// ResolvesWorkspace — the precedent for issue-less task fixtures.
func createTestTask(t *testing.T) string {
	t.Helper()
	ctx := context.Background()

	var agentID, runtimeID string
	if err := testPool.QueryRow(ctx, `
		SELECT id, runtime_id FROM agent WHERE workspace_id = $1 LIMIT 1
	`, testWorkspaceID).Scan(&agentID, &runtimeID); err != nil {
		t.Fatalf("setup: get agent: %v", err)
	}

	var autopilotID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO autopilot (
			workspace_id, title, assignee_id, execution_mode,
			created_by_type, created_by_id
		)
		VALUES ($1, 'callback-jwt-test', $2, 'run_only', 'member', $3)
		RETURNING id
	`, testWorkspaceID, agentID, testUserID).Scan(&autopilotID); err != nil {
		t.Fatalf("setup: create autopilot: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM autopilot WHERE id = $1`, autopilotID) })

	var runID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO autopilot_run (autopilot_id, source, status)
		VALUES ($1, 'manual', 'running')
		RETURNING id
	`, autopilotID).Scan(&runID); err != nil {
		t.Fatalf("setup: create autopilot_run: %v", err)
	}

	var taskID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (
			agent_id, runtime_id, issue_id, status, priority, autopilot_run_id
		)
		VALUES ($1, $2, NULL, 'dispatched', 0, $3)
		RETURNING id
	`, agentID, runtimeID, runID).Scan(&taskID); err != nil {
		t.Fatalf("setup: create task: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE id = $1`, taskID) })

	return taskID
}

// TestStartTask_AcceptsCallbackJWTForMatchingTaskID is the happy-path
// integration check for Task A12: a request arriving with the callback-
// JWT context and a URL taskId that matches the JWT's task_id claim
// authenticates successfully through requireDaemonTaskAccess and reaches
// the handler body (returns 200).
func TestStartTask_AcceptsCallbackJWTForMatchingTaskID(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	taskID := createTestTask(t)

	req := httptest.NewRequest(http.MethodPost, "/api/daemon/tasks/"+taskID+"/start", nil)
	req = withCallbackAuth(req, taskID, "some-runtime-id")
	req = withChiTaskID(req, taskID)

	w := httptest.NewRecorder()
	testHandler.StartTask(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("StartTask via callback JWT: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// TestStartTask_RejectsCallbackJWTForDifferentTaskID is the auth check
// that prevents a stolen / mis-used callback token from being used
// against a task it wasn't issued for. JWT claims task A; URL is task
// B; expect 403.
func TestStartTask_RejectsCallbackJWTForDifferentTaskID(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	realTaskID := createTestTask(t)
	attackedTaskID := createTestTask(t)

	req := httptest.NewRequest(http.MethodPost, "/api/daemon/tasks/"+attackedTaskID+"/start", nil)
	req = withCallbackAuth(req, realTaskID /* JWT claim */, "rt")
	req = withChiTaskID(req, attackedTaskID /* URL */)

	w := httptest.NewRecorder()
	testHandler.StartTask(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 on task_id mismatch, got %d: %s", w.Code, w.Body.String())
	}
}

// TestCompleteTask_AcceptsCallbackJWT covers /complete, exercising the
// same requireDaemonTaskAccess path on a different endpoint. The
// workflow's finalize step is the most common caller, so this catches
// the case where StartTask works but the terminal POST fails.
func TestCompleteTask_AcceptsCallbackJWT(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	taskID := createTestTask(t)

	body := `{"output":"done","session_id":"sess_xyz","work_dir":"/runner/work"}`
	req := httptest.NewRequest(http.MethodPost, "/api/daemon/tasks/"+taskID+"/complete",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = withCallbackAuth(req, taskID, "rt")
	req = withChiTaskID(req, taskID)

	// /complete requires the task to be in a state that allows completion;
	// queued may or may not be acceptable depending on the service-layer
	// transition rules. The test's narrow contract is just that the AUTH
	// check passes — we accept any non-403 / non-401 status as proof.
	w := httptest.NewRecorder()
	testHandler.CompleteTask(w, req)

	if w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized {
		t.Fatalf("CompleteTask rejected callback JWT auth: %d %s", w.Code, w.Body.String())
	}
}

// TestStartTask_RejectsCallbackJWTForNonExistentTask covers the case
// where the URL task ID is well-formed and matches the JWT claim, but
// the task row was deleted (or never existed). Should 404, not 200
// (the auth check passes but the task lookup fails).
func TestStartTask_RejectsCallbackJWTForNonExistentTask(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	bogusID := "00000000-0000-0000-0000-000000000000"

	req := httptest.NewRequest(http.MethodPost, "/api/daemon/tasks/"+bogusID+"/start", nil)
	req = withCallbackAuth(req, bogusID, "rt")
	req = withChiTaskID(req, bogusID)

	w := httptest.NewRecorder()
	testHandler.StartTask(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for non-existent task, got %d: %s", w.Code, w.Body.String())
	}
}

