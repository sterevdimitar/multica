package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A card that changes status lands at the top of its new column.
//
// The board sorts each column by position ASC. CreateIssue already places a
// new card at MIN(position)-1 of its column (issue_create_position_test.go),
// but until the status-writing queries in issue.sql learned the same rule a
// status change kept the position the card had in its OLD column, so a card
// reaching `done` landed anywhere. These tests pin the rule on every status
// writer: the HTTP update (every pipeline PUT and every UI edit) and the six
// fixed-status queries the fork's internal writers use. They assert on what
// was written (a direct SELECT), and on the RETURNING row too, because every
// caller emits that row on issue:updated and the board re-slots the card from
// it.

// positionOf reads issue.position straight from the DB — never from a
// response body, so the assertion is on what was written, not what was echoed.
func positionOf(t *testing.T, id pgtype.UUID) float64 {
	t.Helper()
	var pos float64
	if err := testPool.QueryRow(context.Background(),
		`SELECT position FROM issue WHERE id = $1`, id).Scan(&pos); err != nil {
		t.Fatalf("read position: %v", err)
	}
	return pos
}

// setPosition simulates a drag: writes issue.position directly, as the UI's
// PUT {position} would, without touching anything else.
func setPosition(t *testing.T, id pgtype.UUID, pos float64) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`UPDATE issue SET position = $2 WHERE id = $1`, id, pos); err != nil {
		t.Fatalf("set position: %v", err)
	}
}

// minPositionIn returns COALESCE(MIN(position), 0) for (workspace, status) —
// the pre-write "top" the HTTP tests compute their expectation from, because
// the shared test workspace may hold other tests' leftovers in `done`.
func minPositionIn(t *testing.T, workspaceID pgtype.UUID, status string) float64 {
	t.Helper()
	var min float64
	if err := testPool.QueryRow(context.Background(),
		`SELECT COALESCE(MIN(position), 0) FROM issue WHERE workspace_id = $1 AND status = $2`,
		workspaceID, status).Scan(&min); err != nil {
		t.Fatalf("min position: %v", err)
	}
	return min
}

// newIsolatedWorkspace inserts a workspace with a unique slug, registers a
// cascade DELETE in t.Cleanup, and returns its id. The direct-query tests use
// it so their columns are fully known and the expectations are exact numbers.
func newIsolatedWorkspace(t *testing.T) pgtype.UUID {
	t.Helper()
	ctx := context.Background()
	slug := fmt.Sprintf("status-pos-%d", time.Now().UnixNano())
	var id pgtype.UUID
	if err := testPool.QueryRow(ctx, `
		INSERT INTO workspace (name, slug, description, issue_prefix)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`, "status position "+slug, slug, "isolated workspace for issue_status_position_test", "SPT").Scan(&id); err != nil {
		t.Fatalf("create isolated workspace: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, id)
	})
	return id
}

// seedIssue inserts one issue with Queries.CreateIssue at an explicit
// status/position/number (creator_type 'member', creator_id testUserID).
func seedIssue(t *testing.T, workspaceID pgtype.UUID, status string, position float64, number int32) db.Issue {
	t.Helper()
	issue, err := testHandler.Queries.CreateIssue(context.Background(), db.CreateIssueParams{
		WorkspaceID: workspaceID,
		Title:       fmt.Sprintf("status-pos %s #%d", status, number),
		Status:      status,
		Priority:    "low",
		CreatorType: "member",
		CreatorID:   parseUUID(testUserID),
		Position:    position,
		Number:      number,
	})
	if err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	return issue
}

// createCardAt7 creates a card in the shared workspace via the API at `todo`
// and parks its position at 7 — a value no top-of-column write produces, so
// "position unchanged" is distinguishable from "landed at the top".
func createCardAt7(t *testing.T) (string, pgtype.UUID) {
	t.Helper()
	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":    "status-position card",
		"status":   "todo",
		"priority": "low",
	})
	testHandler.CreateIssue(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateIssue: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp IssueResponse
	json.NewDecoder(w.Body).Decode(&resp)
	t.Cleanup(func() { deleteTestIssue(t, resp.ID) })
	id := parseUUID(resp.ID)
	setPosition(t, id, 7)
	return resp.ID, id
}

func putIssue(t *testing.T, id string, body map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	req := newRequest("PUT", "/api/issues/"+id+"?workspace_id="+testWorkspaceID, body)
	req = withURLParam(req, "id", id)
	testHandler.UpdateIssue(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateIssue %v: expected 200, got %d: %s", body, w.Code, w.Body.String())
	}
}

func TestUpdateIssue_StatusChangeLandsAtTopOfColumn(t *testing.T) {
	requireDB(t)
	idStr, id := createCardAt7(t)
	want := minPositionIn(t, parseUUID(testWorkspaceID), "done") - 1

	putIssue(t, idStr, map[string]any{"status": "done"})

	if got := positionOf(t, id); got != want {
		t.Errorf("position after status change = %v, want top of done column %v", got, want)
	}
}

func TestUpdateIssue_ExplicitPositionWinsOnStatusChange(t *testing.T) {
	requireDB(t)
	idStr, id := createCardAt7(t)

	putIssue(t, idStr, map[string]any{"status": "done", "position": -4})

	if got := positionOf(t, id); got != -4 {
		t.Errorf("position = %v, want the explicit -4 (a drag lands where dropped)", got)
	}
}

func TestUpdateIssue_NonStatusEditKeepsPosition(t *testing.T) {
	requireDB(t)
	idStr, id := createCardAt7(t)

	putIssue(t, idStr, map[string]any{"title": "renamed"})

	if got := positionOf(t, id); got != 7 {
		t.Errorf("position after title edit = %v, want 7 (unchanged)", got)
	}
}

func TestUpdateIssue_SameStatusKeepsPosition(t *testing.T) {
	requireDB(t)
	idStr, id := createCardAt7(t)

	putIssue(t, idStr, map[string]any{"status": "todo"})

	if got := positionOf(t, id); got != 7 {
		t.Errorf("position after same-status write = %v, want 7 (unchanged)", got)
	}
}

// seedColumns builds the isolated fixture: two cards in `done` at -5 and -3,
// and the card under test in `todo` at 7. Top of `done` is therefore -6.
func seedColumns(t *testing.T) (pgtype.UUID, db.Issue) {
	t.Helper()
	ws := newIsolatedWorkspace(t)
	seedIssue(t, ws, "done", -5, 1)
	seedIssue(t, ws, "done", -3, 2)
	card := seedIssue(t, ws, "todo", 7, 3)
	return ws, card
}

func TestStatusQueries_LandAtTopOfColumn(t *testing.T) {
	requireDB(t)
	q := testHandler.Queries
	member := parseUUID(testUserID)

	cases := []struct {
		name string
		run  func(ctx context.Context, ws pgtype.UUID, issue db.Issue) (db.Issue, error)
	}{
		{"UpdateIssueStatus", func(ctx context.Context, ws pgtype.UUID, issue db.Issue) (db.Issue, error) {
			return q.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: issue.ID, Status: "done", WorkspaceID: ws})
		}},
		{"UpdateIssueStatusAndUnassign", func(ctx context.Context, ws pgtype.UUID, issue db.Issue) (db.Issue, error) {
			return q.UpdateIssueStatusAndUnassign(ctx, db.UpdateIssueStatusAndUnassignParams{ID: issue.ID, Status: "done", WorkspaceID: ws})
		}},
		{"UpdateIssueStatusAndAssign", func(ctx context.Context, ws pgtype.UUID, issue db.Issue) (db.Issue, error) {
			return q.UpdateIssueStatusAndAssign(ctx, db.UpdateIssueStatusAndAssignParams{
				ID: issue.ID, Status: "done",
				AssigneeType: pgtype.Text{String: "member", Valid: true}, AssigneeID: member,
				WorkspaceID: ws,
			})
		}},
		{"UpdateIssueStatusIfCurrent", func(ctx context.Context, ws pgtype.UUID, issue db.Issue) (db.Issue, error) {
			return q.UpdateIssueStatusIfCurrent(ctx, db.UpdateIssueStatusIfCurrentParams{
				ID: issue.ID, Status: "done", WorkspaceID: ws, CurrentStatuses: []string{"todo"},
			})
		}},
		{"UpdateIssueStatusAndUnassignIfCurrent", func(ctx context.Context, ws pgtype.UUID, issue db.Issue) (db.Issue, error) {
			return q.UpdateIssueStatusAndUnassignIfCurrent(ctx, db.UpdateIssueStatusAndUnassignIfCurrentParams{
				ID: issue.ID, Status: "done", WorkspaceID: ws, CurrentStatuses: []string{"todo"},
			})
		}},
		{"UpdateIssueStatusIfCurrentAndInactive", func(ctx context.Context, ws pgtype.UUID, issue db.Issue) (db.Issue, error) {
			return q.UpdateIssueStatusIfCurrentAndInactive(ctx, db.UpdateIssueStatusIfCurrentAndInactiveParams{
				ID: issue.ID, Status: "done", WorkspaceID: ws, CurrentStatuses: []string{"todo"},
			})
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws, card := seedColumns(t)

			updated, err := tc.run(context.Background(), ws, card)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if updated.Status != "done" {
				t.Fatalf("returned status = %q, want done", updated.Status)
			}
			if got := positionOf(t, card.ID); got != -6 {
				t.Errorf("written position = %v, want -6 (top of done: MIN -5, minus 1)", got)
			}
			if updated.Position != -6 {
				t.Errorf("RETURNING position = %v, want -6 (the board re-slots the card from this row)", updated.Position)
			}
		})
	}
}

func TestUpdateIssueStatus_SameStatusKeepsPosition(t *testing.T) {
	requireDB(t)
	ws, card := seedColumns(t)

	updated, err := testHandler.Queries.UpdateIssueStatus(context.Background(), db.UpdateIssueStatusParams{
		ID: card.ID, Status: "todo", WorkspaceID: ws,
	})
	if err != nil {
		t.Fatalf("UpdateIssueStatus: %v", err)
	}
	if got := positionOf(t, card.ID); got != 7 {
		t.Errorf("position after same-status write = %v, want 7 (unchanged)", got)
	}
	if updated.Position != 7 {
		t.Errorf("RETURNING position = %v, want 7", updated.Position)
	}
}

func TestUpdateIssueStatus_EmptyColumnLandsAtMinusOne(t *testing.T) {
	requireDB(t)
	ws := newIsolatedWorkspace(t)
	card := seedIssue(t, ws, "todo", 7, 1)

	if _, err := testHandler.Queries.UpdateIssueStatus(context.Background(), db.UpdateIssueStatusParams{
		ID: card.ID, Status: "in_review", WorkspaceID: ws,
	}); err != nil {
		t.Fatalf("UpdateIssueStatus: %v", err)
	}
	if got := positionOf(t, card.ID); got != -1 {
		t.Errorf("position into an empty column = %v, want -1 (COALESCE(MIN, 0) - 1)", got)
	}
}

// The archive sweeper's write (ArchiveIssuesIfStatus) is a status writer too:
// a card it moves to `archived` lands at the top of that column.
func TestArchiveIssuesIfStatus_LandsAtTopOfColumn(t *testing.T) {
	requireDB(t)
	ws := newIsolatedWorkspace(t)
	seedIssue(t, ws, "archived", -5, 1)
	seedIssue(t, ws, "archived", -3, 2)
	card := seedIssue(t, ws, "done", 7, 3)

	rows, err := testHandler.Queries.ArchiveIssuesIfStatus(context.Background(), db.ArchiveIssuesIfStatusParams{
		Ids: []pgtype.UUID{card.ID}, CurrentStatus: "done",
	})
	if err != nil {
		t.Fatalf("ArchiveIssuesIfStatus: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != "archived" {
		t.Fatalf("archived rows = %+v, want the one card at archived", rows)
	}
	if got := positionOf(t, card.ID); got != -6 {
		t.Errorf("written position = %v, want -6 (top of archived: MIN -5, minus 1)", got)
	}
	if rows[0].Position != -6 {
		t.Errorf("RETURNING position = %v, want -6", rows[0].Position)
	}
}
