package main

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
)

// A create_issue run whose card is archived while the run is still open is
// finalized the way a cancelled card finalizes it: failed, reason
// "issue archived". An archived card is finished work; a run left open on
// it would sit in issue_created forever.
func TestSyncRunFromIssueArchivedFailsTheRun(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	fx := dispatchCreateIssueAutopilot(t, "archived finalizes the run")
	bus := events.New()
	autopilotSvc := service.NewAutopilotService(fx.queries, testPool, bus, fx.taskSvc)

	if _, err := testPool.Exec(ctx, `UPDATE issue SET status = 'archived' WHERE id = $1`, fx.run.IssueID); err != nil {
		t.Fatalf("archive issue: %v", err)
	}
	issue, err := fx.queries.GetIssue(ctx, fx.run.IssueID)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}

	autopilotSvc.SyncRunFromIssue(ctx, issue)

	updated, err := fx.queries.GetAutopilotRun(ctx, fx.run.ID)
	if err != nil {
		t.Fatalf("GetAutopilotRun: %v", err)
	}
	if updated.Status != "failed" {
		t.Fatalf("run status = %q, want failed", updated.Status)
	}
	if !updated.FailureReason.Valid || updated.FailureReason.String != "issue archived" {
		t.Fatalf("failure reason = %v %q, want \"issue archived\"", updated.FailureReason.Valid, updated.FailureReason.String)
	}
}
