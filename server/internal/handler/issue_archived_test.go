package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// PUT status=archived is an ordinary status write — the handler allow-list
// and the CHECK constraint (migration 208) both accept it — and the card is
// then terminal for every "open" read: open_only omits it while GET by id
// still returns it. open_only is the listing the pipeline's reconciler sweep
// reads, so this exclusion is what makes the sweep blind to archived cards.
func TestArchivedCardIsAcceptedAndOmittedFromOpenOnly(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "Archive Read Agent", nil)
	issueID := createSteerVerbIssueWithStatus(t, "archived is terminal in reads", agentID, "done")

	w := httptest.NewRecorder()
	r := withURLParam(newRequest(http.MethodPut, "/api/issues/"+issueID, map[string]any{"status": "archived"}), "id", issueID)
	testHandler.UpdateIssue(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status=archived: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	status, assigneeType, _ := loadIssueAssigneeAndStatus(t, issueID)
	if status != "archived" {
		t.Fatalf("status after PUT = %q, want archived", status)
	}
	if assigneeType == nil {
		t.Fatalf("assignee cleared by archive; want it kept")
	}

	listIssues := func(query string) map[string]bool {
		t.Helper()
		w := httptest.NewRecorder()
		testHandler.ListIssues(w, newRequest(http.MethodGet, "/api/issues"+query, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("ListIssues%s: expected 200, got %d: %s", query, w.Code, w.Body.String())
		}
		var resp struct {
			Issues []IssueResponse `json:"issues"`
		}
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		out := map[string]bool{}
		for _, issue := range resp.Issues {
			out[issue.ID] = true
		}
		return out
	}
	if listIssues("?open_only=true")[issueID] {
		t.Fatalf("open_only listed the archived card")
	}
	if !listIssues("?status=archived&limit=200")[issueID] {
		t.Fatalf("status=archived filter did not list the card")
	}

	w = httptest.NewRecorder()
	testHandler.GetIssue(w, withURLParam(newRequest(http.MethodGet, "/api/issues/"+issueID, nil), "id", issueID))
	if w.Code != http.StatusOK {
		t.Fatalf("GET archived card: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}
