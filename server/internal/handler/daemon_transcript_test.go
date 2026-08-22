package handler

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// createTranscriptTask inserts a dispatched task bound to a REAL issue (the
// archive keys on (agent_id, issue_id), so an issue-less fixture cannot
// exercise it) and returns the task and issue ids. Rows clean up via
// t.Cleanup.
func createTranscriptTask(t *testing.T, issueID string) (taskID, gotIssueID, agentID string) {
	t.Helper()
	ctx := context.Background()

	var runtimeID string
	if err := testPool.QueryRow(ctx, `
		SELECT id, runtime_id FROM agent WHERE workspace_id = $1 LIMIT 1
	`, testWorkspaceID).Scan(&agentID, &runtimeID); err != nil {
		t.Fatalf("setup: get agent: %v", err)
	}

	if issueID == "" {
		suffix := time.Now().UnixNano()
		if err := testPool.QueryRow(ctx, `
			INSERT INTO issue (workspace_id, title, description, status, creator_type, creator_id, number)
			VALUES ($1, $2, '', 'in_progress', 'member', $3,
			        (SELECT COALESCE(MAX(number), 0) + 1 FROM issue WHERE workspace_id = $1))
			RETURNING id
		`, testWorkspaceID, fmt.Sprintf("transcript-test-%d", suffix), testUserID).Scan(&issueID); err != nil {
			t.Fatalf("setup: create issue: %v", err)
		}
		issueToClean := issueID
		t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, issueToClean) })
	}

	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority)
		VALUES ($1, $2, $3, 'dispatched', 0)
		RETURNING id
	`, agentID, runtimeID, issueID).Scan(&taskID); err != nil {
		t.Fatalf("setup: create task: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM agent_session_transcript WHERE task_id = $1`, taskID)
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE id = $1`, taskID)
	})
	return taskID, issueID, agentID
}

func putTranscript(t *testing.T, taskID, claimTaskID, sessionID string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/daemon/tasks/"+taskID+"/transcript", bytes.NewReader(body))
	if sessionID != "" {
		req.Header.Set("X-Session-Id", sessionID)
	}
	req = withCallbackAuth(req, claimTaskID, "rt")
	req = withChiTaskID(req, taskID)
	w := httptest.NewRecorder()
	testHandler.UploadTaskTranscript(w, req)
	return w
}

func getResumeTranscript(t *testing.T, taskID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/daemon/tasks/"+taskID+"/resume-transcript", nil)
	req = withCallbackAuth(req, taskID, "rt")
	req = withChiTaskID(req, taskID)
	w := httptest.NewRecorder()
	testHandler.GetTaskResumeTranscript(w, req)
	return w
}

func TestTranscriptRoundTripReturnsIdenticalBytes(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	taskID, _, _ := createTranscriptTask(t, "")
	// Opaque bytes on purpose: the server never decompresses, so the test
	// asserts byte identity, not gzip validity.
	payload := []byte{0x1f, 0x8b, 0x08, 0x00, 0xde, 0xad, 0xbe, 0xef}

	if w := putTranscript(t, taskID, taskID, "sess-abc", payload); w.Code != http.StatusOK {
		t.Fatalf("PUT: got %d: %s", w.Code, w.Body.String())
	}

	w := getResumeTranscript(t, taskID)
	if w.Code != http.StatusOK {
		t.Fatalf("GET: got %d: %s", w.Code, w.Body.String())
	}
	if !bytes.Equal(w.Body.Bytes(), payload) {
		t.Errorf("body = %x, want %x", w.Body.Bytes(), payload)
	}
	if got := w.Header().Get("X-Session-Id"); got != "sess-abc" {
		t.Errorf("X-Session-Id = %q, want sess-abc", got)
	}
	if got := w.Header().Get("X-Archived-At"); got == "" {
		t.Error("X-Archived-At is empty")
	} else if _, err := time.Parse(time.RFC3339, got); err != nil {
		t.Errorf("X-Archived-At %q is not RFC3339: %v", got, err)
	}
}

func TestTranscriptGetReturnsNewestForThePair(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	// Two different tasks on the SAME (agent, issue) pair — the shape a
	// second turn of the same chain produces.
	taskA, issueID, _ := createTranscriptTask(t, "")
	if w := putTranscript(t, taskA, taskA, "sess-old", []byte("old")); w.Code != http.StatusOK {
		t.Fatalf("PUT A: %d %s", w.Code, w.Body.String())
	}
	// A partial unique index permits only one pending task per (issue,
	// agent), which is exactly why turn 2 exists only after turn 1 ends.
	if _, err := testPool.Exec(context.Background(),
		`UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, taskA); err != nil {
		t.Fatalf("setup: complete task A: %v", err)
	}
	// created_at defaults to now(); force a distinct, later timestamp so
	// the ordering assertion cannot pass by luck of row order.
	time.Sleep(10 * time.Millisecond)
	taskB, _, _ := createTranscriptTask(t, issueID)
	if w := putTranscript(t, taskB, taskB, "sess-new", []byte("new")); w.Code != http.StatusOK {
		t.Fatalf("PUT B: %d %s", w.Code, w.Body.String())
	}

	w := getResumeTranscript(t, taskB)
	if w.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Session-Id"); got != "sess-new" {
		t.Errorf("X-Session-Id = %q, want sess-new — resume must read the newest row", got)
	}
	if w.Body.String() != "new" {
		t.Errorf("body = %q, want %q", w.Body.String(), "new")
	}
}

func TestTranscriptAppendsRatherThanReplaces(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	taskID, _, _ := createTranscriptTask(t, "")
	for _, s := range []string{"first", "second"} {
		if w := putTranscript(t, taskID, taskID, "sess-"+s, []byte(s)); w.Code != http.StatusOK {
			t.Fatalf("PUT %s: %d", s, w.Code)
		}
	}
	var count int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM agent_session_transcript WHERE task_id = $1`, taskID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Errorf("rows = %d, want 2 — the archive is append-only", count)
	}
}

func TestTranscriptRejectsMismatchedTaskJWT(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	mine, _, _ := createTranscriptTask(t, "")
	theirs, _, _ := createTranscriptTask(t, "")

	// A task can never read or write another pair's conversation.
	if w := putTranscript(t, theirs, mine, "sess", []byte("x")); w.Code != http.StatusForbidden {
		t.Errorf("PUT with a foreign JWT: got %d, want 403", w.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/daemon/tasks/"+theirs+"/resume-transcript", nil)
	req = withCallbackAuth(req, mine, "rt")
	req = withChiTaskID(req, theirs)
	w := httptest.NewRecorder()
	testHandler.GetTaskResumeTranscript(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("GET with a foreign JWT: got %d, want 403", w.Code)
	}
}

func TestTranscriptRequiresSessionHeader(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	taskID, _, _ := createTranscriptTask(t, "")
	if w := putTranscript(t, taskID, taskID, "", []byte("x")); w.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400 — a transcript whose session it cannot name is unusable for resume", w.Code)
	}
}

func TestTranscriptRejectsEmptyBody(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	taskID, _, _ := createTranscriptTask(t, "")
	if w := putTranscript(t, taskID, taskID, "sess", nil); w.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400", w.Code)
	}
}

func TestTranscriptRejectsOversizeBody(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	taskID, _, _ := createTranscriptTask(t, "")
	oversize := make([]byte, maxTranscriptBytes+1)
	if w := putTranscript(t, taskID, taskID, "sess", oversize); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("got %d, want 413", w.Code)
	}
}

func TestTranscriptGetReturns404ForEmptyPair(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	taskID, _, _ := createTranscriptTask(t, "")
	// No archive yet: the runner treats this as "start fresh", never as an
	// error, so it must be a clean 404 rather than a 500.
	if w := getResumeTranscript(t, taskID); w.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404", w.Code)
	}
}
