package service

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// An archived card is inert: no issue write starts a run on it. The service
// here has NO queries wired, so any case that reaches the agent lookup
// panics — that is the invariant this test pins, not just the false: the
// archived check comes before every lookup, which is what makes keeping the
// assignee on archive safe.
func TestWillEnqueueRunRefusesArchivedCardBeforeAnyLookup(t *testing.T) {
	svc := &IssueService{} // Queries nil on purpose: a lookup would nil-deref
	agentIssue := func(status string) db.Issue {
		return db.Issue{
			ID:           pgtype.UUID{Bytes: [16]byte{1}, Valid: true},
			WorkspaceID:  pgtype.UUID{Bytes: [16]byte{2}, Valid: true},
			Status:       status,
			AssigneeType: pgtype.Text{String: "agent", Valid: true},
			AssigneeID:   pgtype.UUID{Bytes: [16]byte{3}, Valid: true},
		}
	}
	cases := []struct {
		name string
		in   IssueTriggerInput
	}{
		{"create straight into archived", IssueTriggerInput{Issue: agentIssue(StatusArchived), IsCreate: true}},
		{"assign an agent to an archived card", IssueTriggerInput{Issue: agentIssue(StatusArchived), AssigneeChanged: true}},
		{"backlog -> archived", IssueTriggerInput{Issue: agentIssue(StatusArchived), PrevStatus: StatusBacklog, StatusChanged: true}},
		{"todo -> archived", IssueTriggerInput{Issue: agentIssue(StatusArchived), PrevStatus: StatusTodo, StatusChanged: true}},
		// Leaving archived for an active status does not dispatch either —
		// the status source fires only on leaving backlog, exactly as it
		// does for done -> todo today. Re-assigning afterwards does.
		{"archived -> todo", IssueTriggerInput{Issue: agentIssue(StatusTodo), PrevStatus: StatusArchived, StatusChanged: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			trigger, ok := svc.WillEnqueueRun(context.Background(), tc.in, IssueTriggerProbe{})
			if ok {
				t.Fatalf("WillEnqueueRun = %+v, true; want no run", trigger)
			}
		})
	}
}
