package service

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// The archive sweeper moves a card that has sat in done/cancelled for longer
// than olderThan to archived, keeps its assignee, and publishes one
// issue:updated per moved card with the real previous status. Nothing else
// moves: a younger terminal card, an open card of any age, a card already
// archived.
//
// The sweeper is server-wide (it reads across workspaces), and this test runs
// against a shared database, so the threshold is years and the fixture rows
// are backdated a decade: real cards on the same database are days old and
// never match.
func TestArchiveStaleTerminalIssues(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	bus := events.New()
	svc := NewTaskService(db.New(pool), pool, nil, bus)

	anchor, _, agentID, _ := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "ARC", number: 700101, assign: true}, StatusDone)
	wsID := util.UUIDToString(anchor.WorkspaceID)

	// The fixture issue is the "done, old" card. Backdate it, then add the
	// other four beside it in the same workspace.
	const decade = "10 years"
	const yearAgo = "1 year"
	backdate := func(id, age string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE issue SET updated_at = now() - $2::interval WHERE id = $1`, id, age); err != nil {
			t.Fatalf("backdate %s: %v", id, err)
		}
	}
	create := func(title, status, age string, number int32) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, `
			INSERT INTO issue (workspace_id, title, status, priority, creator_id, creator_type, number, position, assignee_type, assignee_id)
			VALUES ($1, $2, $3, 'none', $4, 'member', $5, 0, 'agent', $6)
			RETURNING id`, wsID, title, status, anchor.CreatorID, number, agentID).Scan(&id); err != nil {
			t.Fatalf("create %s: %v", title, err)
		}
		t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, id) })
		backdate(id, age)
		return id
	}
	doneOld := util.UUIDToString(anchor.ID)
	backdate(doneOld, decade)
	doneYoung := create("ARC done young", StatusDone, yearAgo, 700102)
	cancelledOld := create("ARC cancelled old", StatusCancelled, decade, 700103)
	todoOld := create("ARC todo old", StatusTodo, decade, 700104)
	archivedOld := create("ARC archived old", StatusArchived, decade, 700105)

	var mu sync.Mutex
	var prevByID = map[string]string{}
	bus.Subscribe(protocol.EventIssueUpdated, func(e events.Event) {
		payload, _ := e.Payload.(map[string]any)
		changed, _ := payload["status_changed"].(bool)
		issue, _ := payload["issue"].(map[string]any)
		id, _ := issue["id"].(string)
		prev, _ := payload["prev_status"].(string)
		mu.Lock()
		defer mu.Unlock()
		if changed {
			prevByID[id] = prev
		}
	})

	moved := svc.ArchiveStaleTerminalIssues(ctx, 5*365*24*time.Hour)
	if moved != 2 {
		t.Fatalf("moved = %d, want 2", moved)
	}

	status := func(id string) (string, bool) {
		t.Helper()
		var st string
		var assigned bool
		if err := pool.QueryRow(ctx, `SELECT status, assignee_id IS NOT NULL FROM issue WHERE id = $1`, id).Scan(&st, &assigned); err != nil {
			t.Fatalf("load %s: %v", id, err)
		}
		return st, assigned
	}
	for _, tc := range []struct{ id, want string }{
		{doneOld, StatusArchived}, {cancelledOld, StatusArchived},
		{doneYoung, StatusDone}, {todoOld, StatusTodo}, {archivedOld, StatusArchived},
	} {
		got, assigned := status(tc.id)
		if got != tc.want {
			t.Errorf("%s: status = %q, want %q", tc.id, got, tc.want)
		}
		if !assigned {
			t.Errorf("%s: assignee cleared; want it kept", tc.id)
		}
	}

	mu.Lock()
	gotIDs := make([]string, 0, len(prevByID))
	for id := range prevByID {
		gotIDs = append(gotIDs, id)
	}
	sort.Strings(gotIDs)
	wantIDs := []string{doneOld, cancelledOld}
	sort.Strings(wantIDs)
	if fmt.Sprint(gotIDs) != fmt.Sprint(wantIDs) {
		t.Errorf("issue:updated for %v, want %v", gotIDs, wantIDs)
	}
	if prevByID[doneOld] != StatusDone || prevByID[cancelledOld] != StatusCancelled {
		t.Errorf("prev_status = %q / %q, want done / cancelled", prevByID[doneOld], prevByID[cancelledOld])
	}
	mu.Unlock()

	// Idempotent: a second sweep finds nothing and publishes nothing.
	before := len(prevByID)
	if again := svc.ArchiveStaleTerminalIssues(ctx, 5*365*24*time.Hour); again != 0 {
		t.Errorf("second sweep moved %d, want 0", again)
	}
	mu.Lock()
	if len(prevByID) != before {
		t.Errorf("second sweep published %d more events, want 0", len(prevByID)-before)
	}
	mu.Unlock()
}
