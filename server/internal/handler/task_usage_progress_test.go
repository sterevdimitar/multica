package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
