package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// The `done` carve-out from MUL-4465.
//
// MUL-4465 removed the coupling between an issue status change and its
// in-flight agent runs, on the reasoning that a user clicking "cancel" has no
// expectation of interrupting a run. That reasoning holds for every status but
// one. In the dev-command-center pipeline `done` does not mean "tidied away",
// it means MERGE THIS PULL REQUEST — the merge reconciler acts on it within 60
// seconds — and merging a branch while an agent is still editing it is the
// failure this carve-out removes.
//
// So: `done` cancels, every other status does not. The two tests in
// issue_cancel_status_no_cancel_test.go are the other half of this contract and
// must stay green unmodified; if a change here makes one of them fail, this arm
// has grown too wide.
//
// This file deliberately reuses that file's helpers (activeTaskStatuses,
// insertIssueTaskWithStatus) rather than restating them — the two behaviours
// are one decision seen from two sides, and they should fail together if the
// fixture drifts.

// TestUpdateIssueDoneStatusCancelsActiveTasks drives every active task state
// through a single-issue PUT to `done` and requires all of them to end
// cancelled.
//
// The unrelated mention-triggered run for a SECOND agent is included on
// purpose, and its expectation is the opposite of the reassignment and
// `cancelled` tests: the sweep is issue-scoped, so a `done` card stops every
// agent working it, not only its assignee. A future reader must be able to see
// that this is intended rather than an oversight.
func TestUpdateIssueDoneStatusCancelsActiveTasks(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	ownerAgent := createHandlerTestAgent(t, "DoneStatusCancelsOwner", []byte("[]"))
	mentionAgent := createHandlerTestAgent(t, "DoneStatusCancelsMention", []byte("[]"))

	for i, status := range activeTaskStatuses {
		t.Run(status, func(t *testing.T) {
			issueID := insertAgentAssignedIssue(t, ownerAgent, 92150+i, "done-status-cancels-"+status)
			ownerTask := insertIssueTaskWithStatus(t, ownerAgent, issueID, status)
			mentionTask := insertRunningIssueTask(t, mentionAgent, issueID)

			w := httptest.NewRecorder()
			req := newRequest("PUT", "/api/issues/"+issueID, map[string]any{
				"status": "done",
			})
			req = withURLParam(req, "id", issueID)
			testHandler.UpdateIssue(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("UpdateIssue done: expected 200, got %d: %s", w.Code, w.Body.String())
			}

			if got := taskStatus(t, ownerTask); got != "cancelled" {
				t.Fatalf("assignee's %s task must be cancelled by issue → done, got status %q", status, got)
			}
			if got := taskStatus(t, mentionTask); got != "cancelled" {
				t.Fatalf("unrelated agent's task must be cancelled by issue → done, got status %q", got)
			}
		})
	}
}

// TestBatchUpdateIssueDoneStatusCancelsActiveTasks is the batch-path mirror.
// BatchUpdateIssues is a separate write path with its own copy of the
// status-changed bookkeeping, so it needs its own coverage — a carve-out
// applied to only one of the two handlers is a card that stops its runs when
// dragged singly and does not when dragged in a multi-select.
func TestBatchUpdateIssueDoneStatusCancelsActiveTasks(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	ownerAgent := createHandlerTestAgent(t, "BatchDoneStatusCancelsOwner", []byte("[]"))

	issueIDs := make([]string, 0, len(activeTaskStatuses))
	taskByStatus := make(map[string]string, len(activeTaskStatuses))
	for i, status := range activeTaskStatuses {
		issueID := insertAgentAssignedIssue(t, ownerAgent, 92160+i, "batch-done-status-cancels-"+status)
		taskByStatus[status] = insertIssueTaskWithStatus(t, ownerAgent, issueID, status)
		issueIDs = append(issueIDs, issueID)
	}

	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/issues/batch-update", map[string]any{
		"issue_ids": issueIDs,
		"updates": map[string]any{
			"status": "done",
		},
	})
	testHandler.BatchUpdateIssues(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("BatchUpdateIssues done: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	for status, taskID := range taskByStatus {
		if got := taskStatus(t, taskID); got != "cancelled" {
			t.Fatalf("%s task must be cancelled by batch issue → done, got status %q", status, got)
		}
	}
}

// TestAdvanceIssueToDoneCancelsActiveTasks covers the THIRD writer of `done`.
//
// The two tests above drive the HTTP write paths — a human dragging a card, or
// a multi-select batch. This one drives the path that had no coverage and, on
// 2026-09-10, let a real card through: Multica's GitHub App integration moves a
// card to `done` when a human merges a pull request carrying "Closes MUL-N",
// and it does that through advanceIssueToDone rather than UpdateIssue. Card
// MUL-183 was merged that way with three agent runs live; the activity log
// recorded `system` / `github_pr_merged` writing `done` and nothing stopped
// them.
//
// advanceIssueToDone needs no status guard of its own: its single caller
// already skips issues that are `done` or `cancelled`, so reaching here IS the
// transition. If a second caller is ever added, it must preserve that.
func TestAdvanceIssueToDoneCancelsActiveTasks(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	ownerAgent := createHandlerTestAgent(t, "AdvanceDoneCancelsOwner", []byte("[]"))

	for i, status := range activeTaskStatuses {
		t.Run(status, func(t *testing.T) {
			issueID := insertAgentAssignedIssue(t, ownerAgent, 92170+i, "advance-done-cancels-"+status)
			task := insertIssueTaskWithStatus(t, ownerAgent, issueID, status)

			var id pgtype.UUID
			if err := id.Scan(issueID); err != nil {
				t.Fatalf("parse issue id: %v", err)
			}
			issue, err := testHandler.Queries.GetIssue(context.Background(), id)
			if err != nil {
				t.Fatalf("load issue: %v", err)
			}

			testHandler.advanceIssueToDone(context.Background(), issue, testWorkspaceID)

			if got := taskStatus(t, task); got != "cancelled" {
				t.Fatalf("%s task must be cancelled by a merged PR advancing the card to done, got status %q", status, got)
			}
		})
	}
}
