package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ----------------------------------------------------------------------
// Fork-local tests for the ticket-progress-visibility feature: the extended
// task-usage ingest (turns/duration/cost), the task:usage broadcast, and the
// read-only GET /api/issues/{id}/progress projection.
//
// Kept in a dedicated file so the patch survives upstream rebases cleanly —
// the same convention webhook_task_delivery_test.go established.
//
// ⚠️ handler_test.go's TestMain exits 0 when the DB is unreachable, so a
// green `go test` run does NOT prove these executed. Check the -v log names
// the tests.
// ----------------------------------------------------------------------

// postTaskUsage drives ReportTaskUsage with a callback-JWT context, exactly
// as the GitHub Actions runner's streampost does.
func postTaskUsage(t *testing.T, taskID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/api/daemon/tasks/"+taskID+"/usage", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = withCallbackAuth(req, taskID, "some-runtime-id")
	req = withChiTaskID(req, taskID)

	w := httptest.NewRecorder()
	testHandler.ReportTaskUsage(w, req)
	return w
}

type taskUsageRow struct {
	Provider         string
	Model            string
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	NumTurns         int64
	DurationMS       *int64
	DurationAPIMS    *int64
	TotalCostUSD     *float64
}

// readTaskUsageRows reads the stored task_usage rows straight from the pool —
// there is deliberately no read API for them.
func readTaskUsageRows(t *testing.T, taskID string) []taskUsageRow {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `
		SELECT provider, model, input_tokens, output_tokens,
		       cache_read_tokens, cache_write_tokens,
		       num_turns, duration_ms, duration_api_ms, total_cost_usd
		FROM task_usage WHERE task_id = $1 ORDER BY provider, model
	`, taskID)
	if err != nil {
		t.Fatalf("query task_usage: %v", err)
	}
	defer rows.Close()

	var out []taskUsageRow
	for rows.Next() {
		var r taskUsageRow
		if err := rows.Scan(&r.Provider, &r.Model, &r.InputTokens, &r.OutputTokens,
			&r.CacheReadTokens, &r.CacheWriteTokens, &r.NumTurns,
			&r.DurationMS, &r.DurationAPIMS, &r.TotalCostUSD); err != nil {
			t.Fatalf("scan task_usage: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

const (
	usageIncremental = `{"usage":[{"provider":"anthropic","model":"claude-opus-4-6",` +
		`"input_tokens":1234,"output_tokens":567,"cache_read_tokens":890123,` +
		`"cache_write_tokens":4567,"num_turns":3}]}`
	usageFinal = `{"usage":[{"provider":"anthropic","model":"claude-opus-4-6",` +
		`"input_tokens":30100,"output_tokens":8100,"cache_read_tokens":912000,` +
		`"cache_write_tokens":5200,"num_turns":14,"duration_ms":463000,` +
		`"duration_api_ms":401000,"total_cost_usd":1.23}]}`
	usageLegacy = `{"usage":[{"provider":"anthropic","model":"claude-opus-4-6",` +
		`"input_tokens":10,"output_tokens":20,"cache_read_tokens":30,` +
		`"cache_write_tokens":40}]}`
)

func TestReportTaskUsage_IncrementalThenFinalSupersedes(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	taskID := createTestTask(t)

	if w := postTaskUsage(t, taskID, usageIncremental); w.Code != http.StatusOK {
		t.Fatalf("incremental usage POST: got %d: %s", w.Code, w.Body.String())
	}
	if w := postTaskUsage(t, taskID, usageFinal); w.Code != http.StatusOK {
		t.Fatalf("final usage POST: got %d: %s", w.Code, w.Body.String())
	}

	rows := readTaskUsageRows(t, taskID)
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 task_usage row (last-write-wins on (task,provider,model)), got %d", len(rows))
	}
	r := rows[0]
	if r.NumTurns != 14 {
		t.Errorf("num_turns = %d, want 14 (final supersedes)", r.NumTurns)
	}
	if r.DurationMS == nil || *r.DurationMS != 463000 {
		t.Errorf("duration_ms = %v, want 463000", r.DurationMS)
	}
	if r.DurationAPIMS == nil || *r.DurationAPIMS != 401000 {
		t.Errorf("duration_api_ms = %v, want 401000", r.DurationAPIMS)
	}
	if r.TotalCostUSD == nil || *r.TotalCostUSD < 1.229 || *r.TotalCostUSD > 1.231 {
		t.Errorf("total_cost_usd = %v, want ~1.23", r.TotalCostUSD)
	}
	if r.InputTokens != 30100 || r.CacheWriteTokens != 5200 {
		t.Errorf("token counts not superseded: %+v", r)
	}
}

func TestReportTaskUsage_NilDurationsPreserveStored(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	taskID := createTestTask(t)

	if w := postTaskUsage(t, taskID, usageFinal); w.Code != http.StatusOK {
		t.Fatalf("final usage POST: got %d: %s", w.Code, w.Body.String())
	}
	// A late incremental flush must never null out a stored final value.
	if w := postTaskUsage(t, taskID, usageIncremental); w.Code != http.StatusOK {
		t.Fatalf("incremental usage POST: got %d: %s", w.Code, w.Body.String())
	}

	rows := readTaskUsageRows(t, taskID)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	r := rows[0]
	if r.DurationMS == nil || *r.DurationMS != 463000 {
		t.Errorf("duration_ms = %v, want 463000 preserved via COALESCE", r.DurationMS)
	}
	if r.DurationAPIMS == nil || *r.DurationAPIMS != 401000 {
		t.Errorf("duration_api_ms = %v, want 401000 preserved via COALESCE", r.DurationAPIMS)
	}
	if r.TotalCostUSD == nil {
		t.Errorf("total_cost_usd nulled out by a later incremental POST")
	}
	// Turns and tokens DO overwrite — they are monotone and the last POST wins.
	if r.NumTurns != 3 {
		t.Errorf("num_turns = %d, want 3 (overwrite, not COALESCE)", r.NumTurns)
	}
}

func TestReportTaskUsage_LegacyPayloadStillAccepted(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	taskID := createTestTask(t)

	if w := postTaskUsage(t, taskID, usageLegacy); w.Code != http.StatusOK {
		t.Fatalf("legacy usage POST: got %d: %s", w.Code, w.Body.String())
	}

	rows := readTaskUsageRows(t, taskID)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	r := rows[0]
	if r.NumTurns != 0 {
		t.Errorf("num_turns = %d, want 0 for a legacy payload", r.NumTurns)
	}
	if r.DurationMS != nil || r.DurationAPIMS != nil || r.TotalCostUSD != nil {
		t.Errorf("legacy payload should leave durations/cost NULL, got %+v", r)
	}
	if r.InputTokens != 10 || r.OutputTokens != 20 || r.CacheReadTokens != 30 || r.CacheWriteTokens != 40 {
		t.Errorf("legacy token counts wrong: %+v", r)
	}
}

// ----------------------------------------------------------------------
// Task 7 — task:usage broadcast
// ----------------------------------------------------------------------

// seedProgressIssue creates a bare issue in the test workspace.
func seedProgressIssue(t *testing.T, title string) string {
	t.Helper()
	ctx := context.Background()
	var issueID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, creator_type, creator_id, title, number)
		VALUES ($1, 'member', $2, $3, $4) RETURNING id
	`, testWorkspaceID, testUserID, title, nextWorkspaceIssueNumber(t)).Scan(&issueID); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM task_usage WHERE task_id IN (SELECT id FROM agent_task_queue WHERE issue_id = $1)`, issueID)
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
		testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, issueID)
	})
	return issueID
}

// createIssueBackedTask mirrors createTestTask but links the task to an
// issue — the usage broadcast carries issue_id, so the issue-less fixture
// cannot exercise it.
func createIssueBackedTask(t *testing.T, issueID, status string) string {
	t.Helper()
	ctx := context.Background()

	var agentID, runtimeID string
	if err := testPool.QueryRow(ctx, `
		SELECT id, runtime_id FROM agent WHERE workspace_id = $1 LIMIT 1
	`, testWorkspaceID).Scan(&agentID, &runtimeID); err != nil {
		t.Fatalf("setup: get agent: %v", err)
	}

	var taskID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority)
		VALUES ($1, $2, $3, $4, 0) RETURNING id
	`, agentID, runtimeID, issueID, status).Scan(&taskID); err != nil {
		t.Fatalf("setup: create issue-backed task: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM task_usage WHERE task_id = $1`, taskID)
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE id = $1`, taskID)
	})
	return taskID
}

func TestReportTaskUsage_BroadcastsTaskUsage(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	issueID := seedProgressIssue(t, "progress broadcast issue")
	taskID := createIssueBackedTask(t, issueID, "running")

	var mu sync.Mutex
	var got []events.Event
	testHandler.Bus.Subscribe(protocol.EventTaskUsage, func(e events.Event) {
		p, ok := e.Payload.(protocol.TaskUsageEventPayload)
		if !ok || p.TaskID != taskID {
			return // another test's task — this bus has no unsubscribe
		}
		mu.Lock()
		got = append(got, e)
		mu.Unlock()
	})

	// Two entries in one request must produce exactly ONE event with the
	// entries summed.
	body := `{"usage":[` +
		`{"provider":"anthropic","model":"m1","input_tokens":10,"output_tokens":5,` +
		`"cache_read_tokens":100,"cache_write_tokens":7,"num_turns":3},` +
		`{"provider":"anthropic","model":"m2","input_tokens":2,"output_tokens":1,` +
		`"cache_read_tokens":300,"cache_write_tokens":2,"num_turns":4}]}`
	if w := postTaskUsage(t, taskID, body); w.Code != http.StatusOK {
		t.Fatalf("usage POST: got %d: %s", w.Code, w.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 task:usage event, got %d", len(got))
	}
	e := got[0]
	if e.Type != protocol.EventTaskUsage {
		t.Errorf("Type = %q, want task:usage", e.Type)
	}
	if e.WorkspaceID == "" {
		t.Errorf("WorkspaceID empty — the realtime bridge drops non-daemon events with no workspace")
	}
	p := e.Payload.(protocol.TaskUsageEventPayload)
	if p.IssueID != issueID {
		t.Errorf("IssueID = %q, want %q", p.IssueID, issueID)
	}
	if p.Tokens.Input != 12 || p.Tokens.Output != 6 {
		t.Errorf("tokens not summed across entries: %+v", p.Tokens)
	}
	if p.Tokens.CacheRead != 400 || p.Tokens.CacheCreation != 9 {
		t.Errorf("cache tokens wrong (cache_write → cache_creation): %+v", p.Tokens)
	}
	if p.Turns != 7 {
		t.Errorf("Turns = %d, want 7 (summed)", p.Turns)
	}
}

// ----------------------------------------------------------------------
// Task 8 — GET /api/issues/{id}/progress
// ----------------------------------------------------------------------

// seedProgressAgent creates an agent with an explicit custom_env blob. The
// chain derivation reads NEXT_AGENT out of that blob server-side; the
// response must expose resolved NAMES only.
func seedProgressAgent(t *testing.T, name, customEnvJSON string) string {
	t.Helper()
	ctx := context.Background()
	if customEnvJSON == "" {
		customEnvJSON = "{}"
	}
	var agentID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent (
			workspace_id, name, description, runtime_mode, runtime_config,
			runtime_id, visibility, permission_mode, max_concurrent_tasks,
			owner_id, instructions, custom_env, custom_args, mcp_config
		)
		VALUES ($1, $2, '', 'cloud', '{}'::jsonb, $3, 'workspace', 'public_to', 1,
		        $4, '', $5::jsonb, '[]'::jsonb, '{}'::jsonb)
		RETURNING id
	`, testWorkspaceID, name, handlerTestRuntimeID(t), testUserID, customEnvJSON).Scan(&agentID); err != nil {
		t.Fatalf("seed agent %q: %v", name, err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM agent WHERE id = $1`, agentID) })
	return agentID
}

// seedProgressAutopilotRun creates an autopilot assigned to assigneeAgentID
// plus one run, and returns the run id for linking onto tasks.
func seedProgressAutopilotRun(t *testing.T, assigneeAgentID string) string {
	t.Helper()
	ctx := context.Background()

	var autopilotID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO autopilot (workspace_id, title, assignee_type, assignee_id,
		                       execution_mode, created_by_type, created_by_id)
		VALUES ($1, 'progress-test', 'agent', $2, 'run_only', 'member', $3)
		RETURNING id
	`, testWorkspaceID, assigneeAgentID, testUserID).Scan(&autopilotID); err != nil {
		t.Fatalf("seed autopilot: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM autopilot WHERE id = $1`, autopilotID) })

	var runID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO autopilot_run (autopilot_id, source, status)
		VALUES ($1, 'manual', 'running') RETURNING id
	`, autopilotID).Scan(&runID); err != nil {
		t.Fatalf("seed autopilot_run: %v", err)
	}
	return runID
}

type progressTaskSpec struct {
	AgentID        string
	Status         string
	CreatedAt      string // RFC3339; "" → now()
	StartedAt      string // RFC3339; "" → NULL
	CompletedAt    string // RFC3339; "" → NULL
	AutopilotRunID string // "" → NULL
}

func createProgressTask(t *testing.T, issueID string, spec progressTaskSpec) string {
	t.Helper()
	ctx := context.Background()

	var taskID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (
			agent_id, runtime_id, issue_id, status, priority,
			created_at, started_at, completed_at, autopilot_run_id
		)
		VALUES (
			$1, $2, $3, $4, 0,
			COALESCE(NULLIF($5, '')::timestamptz, now()),
			NULLIF($6, '')::timestamptz,
			NULLIF($7, '')::timestamptz,
			NULLIF($8, '')::uuid
		)
		RETURNING id
	`, spec.AgentID, handlerTestRuntimeID(t), issueID, spec.Status,
		spec.CreatedAt, spec.StartedAt, spec.CompletedAt, spec.AutopilotRunID).Scan(&taskID); err != nil {
		t.Fatalf("create progress task: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM task_usage WHERE task_id = $1`, taskID)
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE id = $1`, taskID)
	})
	return taskID
}

func seedTaskUsageRow(t *testing.T, taskID, model string, in, out, cacheRead, cacheWrite, turns int64) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO task_usage (task_id, provider, model, input_tokens, output_tokens,
		                        cache_read_tokens, cache_write_tokens, num_turns)
		VALUES ($1, 'anthropic', $2, $3, $4, $5, $6, $7)
	`, taskID, model, in, out, cacheRead, cacheWrite, turns); err != nil {
		t.Fatalf("seed task_usage: %v", err)
	}
}

// progressResponse mirrors the wire contract exactly — decoding through this
// struct is itself an assertion about field names.
type progressResponse struct {
	Tasks []struct {
		TaskID      string  `json:"task_id"`
		AgentName   string  `json:"agent_name"`
		Status      string  `json:"status"`
		QueuedAt    string  `json:"queued_at"`
		StartedAt   *string `json:"started_at"`
		CompletedAt *string `json:"completed_at"`
		Tokens      struct {
			Input         int64 `json:"input"`
			Output        int64 `json:"output"`
			CacheCreation int64 `json:"cache_creation"`
			CacheRead     int64 `json:"cache_read"`
		} `json:"tokens"`
		Turns  int64 `json:"turns"`
		IsLive bool  `json:"is_live"`
	} `json:"tasks"`
	ExpectedSteps []string `json:"expected_steps"`
	ServerNow     string   `json:"server_now"`
}

func getIssueProgress(t *testing.T, issueID string) (*httptest.ResponseRecorder, progressResponse) {
	t.Helper()
	req := newRequest("GET", "/api/issues/"+issueID+"/progress", nil)
	req = withURLParam(req, "id", issueID)
	w := httptest.NewRecorder()
	testHandler.GetIssueProgress(w, req)

	var body progressResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode progress body: %v\n%s", err, w.Body.String())
		}
	}
	return w, body
}

func TestGetIssueProgress_EmptyIssue(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	issueID := seedProgressIssue(t, "progress empty issue")

	w, body := getIssueProgress(t, issueID)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"tasks":[]`) {
		t.Errorf("tasks must serialize as [] not null: %s", w.Body.String())
	}
	if len(body.Tasks) != 0 {
		t.Errorf("expected no tasks, got %d", len(body.Tasks))
	}
	now, err := time.Parse(time.RFC3339Nano, body.ServerNow)
	if err != nil {
		t.Fatalf("server_now %q not RFC3339: %v", body.ServerNow, err)
	}
	if d := time.Since(now); d > 5*time.Second || d < -5*time.Second {
		t.Errorf("server_now off by %v", d)
	}
}

func TestGetIssueProgress_TasksWithUsage(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	issueID := seedProgressIssue(t, "progress usage issue")
	agentA := seedProgressAgent(t, "progress-alpha", "")
	agentB := seedProgressAgent(t, "progress-beta", "")

	// Deliberately inserted out of order: the response must be sorted by
	// COALESCE(started_at, created_at).
	second := createProgressTask(t, issueID, progressTaskSpec{
		AgentID: agentB, Status: "completed",
		CreatedAt: "2026-08-22T10:05:00Z", StartedAt: "2026-08-22T10:06:00Z",
		CompletedAt: "2026-08-22T10:09:00Z",
	})
	first := createProgressTask(t, issueID, progressTaskSpec{
		AgentID: agentA, Status: "completed",
		CreatedAt: "2026-08-22T10:00:00Z", StartedAt: "2026-08-22T10:01:00Z",
		CompletedAt: "2026-08-22T10:04:00Z",
	})

	// Two models on the first task: tokens sum, turns take the MAX.
	seedTaskUsageRow(t, first, "model-1", 100, 50, 900, 25, 6)
	seedTaskUsageRow(t, first, "model-2", 20, 10, 100, 5, 9)

	w, body := getIssueProgress(t, issueID)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(body.Tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(body.Tasks))
	}
	if body.Tasks[0].TaskID != first || body.Tasks[1].TaskID != second {
		t.Fatalf("tasks not ordered by COALESCE(started_at, created_at): %+v", body.Tasks)
	}
	if body.Tasks[0].AgentName != "progress-alpha" {
		t.Errorf("agent_name = %q, want progress-alpha", body.Tasks[0].AgentName)
	}
	tk := body.Tasks[0].Tokens
	if tk.Input != 120 || tk.Output != 60 || tk.CacheRead != 1000 {
		t.Errorf("tokens not summed across models: %+v", tk)
	}
	if tk.CacheCreation != 30 {
		t.Errorf("cache_creation = %d, want 30 (from cache_write_tokens)", tk.CacheCreation)
	}
	if body.Tasks[0].Turns != 9 {
		t.Errorf("turns = %d, want 9 (MAX across models)", body.Tasks[0].Turns)
	}
	// A task with no usage rows reports zeros, not an error.
	if body.Tasks[1].Turns != 0 || body.Tasks[1].Tokens.Input != 0 {
		t.Errorf("usage-less task should report zeros: %+v", body.Tasks[1])
	}
	if body.Tasks[0].StartedAt == nil || body.Tasks[0].CompletedAt == nil {
		t.Errorf("timestamps missing: %+v", body.Tasks[0])
	}
}

func TestGetIssueProgress_LiveFlag(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	issueID := seedProgressIssue(t, "progress live issue")
	agentA := seedProgressAgent(t, "progress-live-agent", "")

	want := []struct {
		status string
		live   bool
	}{
		{"running", true},
		{"dispatched", true},
		{"completed", false},
		{"failed", false},
	}
	for i, c := range want {
		createProgressTask(t, issueID, progressTaskSpec{
			AgentID: agentA, Status: c.status,
			CreatedAt: time.Date(2026, 8, 22, 10, i, 0, 0, time.UTC).Format(time.RFC3339),
		})
	}

	w, body := getIssueProgress(t, issueID)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(body.Tasks) != len(want) {
		t.Fatalf("expected %d tasks, got %d", len(want), len(body.Tasks))
	}
	for i, c := range want {
		if body.Tasks[i].Status != c.status {
			t.Fatalf("task[%d] status = %q, want %q", i, body.Tasks[i].Status, c.status)
		}
		if body.Tasks[i].IsLive != c.live {
			t.Errorf("task[%d] (%s) is_live = %v, want %v", i, c.status, body.Tasks[i].IsLive, c.live)
		}
	}
}

func TestGetIssueProgress_ExpectedStepsChain(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	issueID := seedProgressIssue(t, "progress chain issue")

	review := seedProgressAgent(t, "review-agent", `{"NEXT_AGENT":"review-validator-agent"}`)
	seedProgressAgent(t, "review-validator-agent", `{"NEXT_AGENT":"fixer"}`)
	seedProgressAgent(t, "fixer", `{"NEXT_AGENT":""}`)
	seedProgressAgent(t, "readiness-agent", "{}")

	runID := seedProgressAutopilotRun(t, review)
	createProgressTask(t, issueID, progressTaskSpec{
		AgentID: review, Status: "completed", AutopilotRunID: runID,
	})

	w, body := getIssueProgress(t, issueID)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	wantSteps := []string{"review-agent", "review-validator-agent", "fixer", "readiness-agent"}
	if len(body.ExpectedSteps) != len(wantSteps) {
		t.Fatalf("expected_steps = %v, want %v", body.ExpectedSteps, wantSteps)
	}
	for i := range wantSteps {
		if body.ExpectedSteps[i] != wantSteps[i] {
			t.Fatalf("expected_steps = %v, want %v", body.ExpectedSteps, wantSteps)
		}
	}
}

func TestGetIssueProgress_ExpectedStepsNilOnCycle(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	issueID := seedProgressIssue(t, "progress cycle issue")

	cycleA := seedProgressAgent(t, "progress-cycle-a", `{"NEXT_AGENT":"progress-cycle-b"}`)
	seedProgressAgent(t, "progress-cycle-b", `{"NEXT_AGENT":"progress-cycle-a"}`)

	runID := seedProgressAutopilotRun(t, cycleA)
	createProgressTask(t, issueID, progressTaskSpec{
		AgentID: cycleA, Status: "completed", AutopilotRunID: runID,
	})

	w, body := getIssueProgress(t, issueID)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if body.ExpectedSteps != nil {
		t.Errorf("expected_steps = %v, want JSON null on a cycle", body.ExpectedSteps)
	}
	if !strings.Contains(w.Body.String(), `"expected_steps":null`) {
		t.Errorf("expected_steps must serialize as null: %s", w.Body.String())
	}
}

func TestGetIssueProgress_ExpectedStepsNilWithoutAutopilot(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	issueID := seedProgressIssue(t, "progress no-autopilot issue")
	agentA := seedProgressAgent(t, "progress-solo-agent", `{"NEXT_AGENT":"nobody"}`)
	createProgressTask(t, issueID, progressTaskSpec{AgentID: agentA, Status: "completed"})

	w, body := getIssueProgress(t, issueID)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if body.ExpectedSteps != nil {
		t.Errorf("expected_steps = %v, want null without an autopilot run", body.ExpectedSteps)
	}
	if len(body.Tasks) != 1 {
		t.Errorf("tasks should still be reported, got %d", len(body.Tasks))
	}
}

func TestGetIssueProgress_WrongWorkspace404(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	foreignIssueID, _ := setupForeignWorkspaceFixture(t)

	w, _ := getIssueProgress(t, foreignIssueID)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a cross-workspace issue, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetIssueProgress_NoEnvLeak(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	issueID := seedProgressIssue(t, "progress env-leak issue")

	head := seedProgressAgent(t, "progress-leak-head",
		`{"NEXT_AGENT":"progress-leak-tail","SECRET_X":"hush"}`)
	seedProgressAgent(t, "progress-leak-tail", `{"SECRET_Y":"also-hush"}`)

	runID := seedProgressAutopilotRun(t, head)
	createProgressTask(t, issueID, progressTaskSpec{
		AgentID: head, Status: "completed", AutopilotRunID: runID,
	})

	w, body := getIssueProgress(t, issueID)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	raw := w.Body.String()
	for _, forbidden := range []string{"SECRET_X", "hush", "SECRET_Y", "also-hush", "NEXT_AGENT"} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("response leaks custom_env material %q: %s", forbidden, raw)
		}
	}
	// The derivation still worked — names only.
	if len(body.ExpectedSteps) != 2 ||
		body.ExpectedSteps[0] != "progress-leak-head" ||
		body.ExpectedSteps[1] != "progress-leak-tail" {
		t.Errorf("expected_steps = %v, want the two resolved names", body.ExpectedSteps)
	}
}
