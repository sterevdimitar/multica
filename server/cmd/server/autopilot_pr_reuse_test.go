package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// newPRReuseTestAutopilot creates a create_issue autopilot for the Task 4
// reuse tests below, and registers cleanup for both the autopilot row and
// every issue it creates.
func newPRReuseTestAutopilot(t *testing.T, ctx context.Context, queries *db.Queries, name string) (db.Autopilot, string) {
	t.Helper()

	var agentID string
	if err := testPool.QueryRow(ctx,
		`SELECT id::text FROM agent WHERE workspace_id = $1 ORDER BY created_at ASC LIMIT 1`,
		testWorkspaceID,
	).Scan(&agentID); err != nil {
		t.Fatalf("load fixture agent: %v", err)
	}

	ap, err := queries.CreateAutopilot(ctx, db.CreateAutopilotParams{
		WorkspaceID:        parseUUID(testWorkspaceID),
		Title:              name,
		Description:        pgtype.Text{String: name, Valid: true},
		AssigneeType:       "agent",
		AssigneeID:         parseUUID(agentID),
		Status:             "active",
		ExecutionMode:      "create_issue",
		IssueTitleTemplate: pgtype.Text{},
		CreatedByType:      "member",
		CreatedByID:        parseUUID(testUserID),
	})
	if err != nil {
		t.Fatalf("CreateAutopilot: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = testPool.Exec(bg, `DELETE FROM issue WHERE origin_type = 'autopilot' AND origin_id = $1`, ap.ID)
		_, _ = testPool.Exec(bg, `DELETE FROM autopilot WHERE id = $1`, ap.ID)
	})
	return ap, agentID
}

func countIssuesForPR(t *testing.T, ctx context.Context, autopilotID pgtype.UUID, slug string) int {
	t.Helper()
	var count int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM issue WHERE origin_type = 'autopilot' AND origin_id = $1 AND metadata ->> 'pull_request' = $2`,
		autopilotID, slug,
	).Scan(&count); err != nil {
		t.Fatalf("count issues for PR: %v", err)
	}
	return count
}

// simulateTaskClaimed transitions every queued/dispatched task on an issue to
// running, standing in for the daemon's real claim cycle (FOR UPDATE SKIP
// LOCKED, polling every few seconds) which nothing in this test process
// drives. Without it, a second delivery arriving before any claim would
// collide with idx_one_pending_task_per_issue_agent (one pending task per
// (issue, agent)) — a real, pre-existing constraint unrelated to PR reuse,
// which in production is normally cleared within seconds by the daemon
// before a second webhook delivery lands.
func simulateTaskClaimed(t *testing.T, ctx context.Context, issueID pgtype.UUID) {
	t.Helper()
	if _, err := testPool.Exec(ctx,
		`UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE issue_id = $1 AND status IN ('queued', 'dispatched')`,
		issueID,
	); err != nil {
		t.Fatalf("simulate task claimed: %v", err)
	}
}

func countAgentTasksForIssue(t *testing.T, ctx context.Context, issueID pgtype.UUID) int {
	t.Helper()
	var count int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID,
	).Scan(&count); err != nil {
		t.Fatalf("count agent tasks for issue: %v", err)
	}
	return count
}

// TestDispatchReusesCardForSamePR: two deliveries for the same PR must
// produce exactly one card, with the second run's issue_id equal to the
// first's.
func TestDispatchReusesCardForSamePR(t *testing.T) {
	ctx := context.Background()
	queries := db.New(testPool)
	bus := events.New()
	taskSvc := service.NewTaskService(queries, testPool, nil, bus)
	autopilotSvc := service.NewAutopilotService(queries, testPool, bus, taskSvc)

	ap, _ := newPRReuseTestAutopilot(t, ctx, queries, "PR reuse test "+time.Now().UTC().Format("150405.000000000"))

	repo := "acme/reuse"
	number := int(time.Now().UnixNano() % 100000)
	slug := fmt.Sprintf("%s#%d", repo, number)

	first, err := autopilotSvc.DispatchAutopilot(ctx, ap, pgtype.UUID{}, "webhook",
		pullRequestWebhookPayload(repo, number, "same PR, first delivery"))
	if err != nil {
		t.Fatalf("first DispatchAutopilot: %v", err)
	}
	if first == nil || first.Status != "issue_created" || !first.IssueID.Valid {
		t.Fatalf("first dispatch = %+v, want issue_created with issue_id", first)
	}
	simulateTaskClaimed(t, ctx, first.IssueID)

	second, err := autopilotSvc.DispatchAutopilot(ctx, ap, pgtype.UUID{}, "webhook",
		pullRequestWebhookPayload(repo, number, "same PR, second delivery"))
	if err != nil {
		t.Fatalf("second DispatchAutopilot: %v", err)
	}
	if second == nil || !second.IssueID.Valid {
		t.Fatalf("second dispatch = %+v, want a linked issue_id", second)
	}
	if second.IssueID != first.IssueID {
		t.Fatalf("second run issue_id = %s, want the first run's issue_id %s",
			second.IssueID, first.IssueID)
	}

	if count := countIssuesForPR(t, ctx, ap.ID, slug); count != 1 {
		t.Fatalf("expected exactly 1 card for %s, got %d", slug, count)
	}
}

// TestDispatchReuseStillEnqueues is I1, the single most important test in
// the plan: the reuse path must enqueue a task on the existing card, not
// silently skip. The old behaviour marked the run "skipped" and left the
// pull request unreviewed.
func TestDispatchReuseStillEnqueues(t *testing.T) {
	ctx := context.Background()
	queries := db.New(testPool)
	bus := events.New()
	taskSvc := service.NewTaskService(queries, testPool, nil, bus)
	autopilotSvc := service.NewAutopilotService(queries, testPool, bus, taskSvc)

	ap, _ := newPRReuseTestAutopilot(t, ctx, queries, "PR reuse enqueue test "+time.Now().UTC().Format("150405.000000000"))

	repo := "acme/reuse-enqueue"
	number := int(time.Now().UnixNano() % 100000)

	first, err := autopilotSvc.DispatchAutopilot(ctx, ap, pgtype.UUID{}, "webhook",
		pullRequestWebhookPayload(repo, number, "enqueue test, first delivery"))
	if err != nil {
		t.Fatalf("first DispatchAutopilot: %v", err)
	}
	if first == nil || !first.IssueID.Valid {
		t.Fatalf("first dispatch = %+v, want a linked issue_id", first)
	}
	tasksAfterFirst := countAgentTasksForIssue(t, ctx, first.IssueID)
	if tasksAfterFirst == 0 {
		t.Fatalf("first delivery enqueued no task at all — test setup is broken")
	}
	simulateTaskClaimed(t, ctx, first.IssueID)

	second, err := autopilotSvc.DispatchAutopilot(ctx, ap, pgtype.UUID{}, "webhook",
		pullRequestWebhookPayload(repo, number, "enqueue test, second delivery"))
	if err != nil {
		t.Fatalf("second DispatchAutopilot: %v", err)
	}
	if second == nil || second.Status == "skipped" {
		t.Fatalf("second dispatch = %+v, want the run ATTACHED and enqueued, not skipped (I1)", second)
	}

	tasksAfterSecond := countAgentTasksForIssue(t, ctx, first.IssueID)
	if tasksAfterSecond <= tasksAfterFirst {
		t.Fatalf("I1: reuse must enqueue a NEW task on the existing card — tasks before=%d after=%d",
			tasksAfterFirst, tasksAfterSecond)
	}
}

// TestDispatchDoesNotReuseFinishedCard is I2: a done/cancelled card must
// never receive a new run — that would hand the merge reconciler a second
// merge trigger for one card. A fresh card is created instead.
func TestDispatchDoesNotReuseFinishedCard(t *testing.T) {
	ctx := context.Background()
	queries := db.New(testPool)
	bus := events.New()
	taskSvc := service.NewTaskService(queries, testPool, nil, bus)
	autopilotSvc := service.NewAutopilotService(queries, testPool, bus, taskSvc)

	ap, _ := newPRReuseTestAutopilot(t, ctx, queries, "PR reuse finished-card test "+time.Now().UTC().Format("150405.000000000"))

	repo := "acme/reuse-finished"
	number := int(time.Now().UnixNano() % 100000)
	slug := fmt.Sprintf("%s#%d", repo, number)

	first, err := autopilotSvc.DispatchAutopilot(ctx, ap, pgtype.UUID{}, "webhook",
		pullRequestWebhookPayload(repo, number, "finished card test, first delivery"))
	if err != nil {
		t.Fatalf("first DispatchAutopilot: %v", err)
	}
	if first == nil || !first.IssueID.Valid {
		t.Fatalf("first dispatch = %+v, want a linked issue_id", first)
	}

	if _, err := testPool.Exec(ctx, `UPDATE issue SET status = 'done' WHERE id = $1`, first.IssueID); err != nil {
		t.Fatalf("mark first card done: %v", err)
	}

	second, err := autopilotSvc.DispatchAutopilot(ctx, ap, pgtype.UUID{}, "webhook",
		pullRequestWebhookPayload(repo, number, "finished card test, second delivery"))
	if err != nil {
		t.Fatalf("second DispatchAutopilot: %v", err)
	}
	if second == nil || !second.IssueID.Valid {
		t.Fatalf("second dispatch = %+v, want a NEW linked issue_id", second)
	}
	if second.IssueID == first.IssueID {
		t.Fatalf("I2: must not attach to a done card — got the same issue_id %s", first.IssueID)
	}

	if count := countIssuesForPR(t, ctx, ap.ID, slug); count != 2 {
		t.Fatalf("expected 2 cards for %s (one done, one fresh), got %d", slug, count)
	}
}

// TestDispatchReusesAcrossPRTitleChange is I6: dedupe keys on
// metadata.pull_request, never on the title. A PR whose title changes
// between deliveries must still reuse its card.
func TestDispatchReusesAcrossPRTitleChange(t *testing.T) {
	ctx := context.Background()
	queries := db.New(testPool)
	bus := events.New()
	taskSvc := service.NewTaskService(queries, testPool, nil, bus)
	autopilotSvc := service.NewAutopilotService(queries, testPool, bus, taskSvc)

	ap, _ := newPRReuseTestAutopilot(t, ctx, queries, "PR reuse title-change test "+time.Now().UTC().Format("150405.000000000"))

	repo := "acme/reuse-title-change"
	number := int(time.Now().UnixNano() % 100000)
	slug := fmt.Sprintf("%s#%d", repo, number)

	first, err := autopilotSvc.DispatchAutopilot(ctx, ap, pgtype.UUID{}, "webhook",
		pullRequestWebhookPayload(repo, number, "original title"))
	if err != nil {
		t.Fatalf("first DispatchAutopilot: %v", err)
	}
	if first == nil || !first.IssueID.Valid {
		t.Fatalf("first dispatch = %+v, want a linked issue_id", first)
	}
	simulateTaskClaimed(t, ctx, first.IssueID)

	second, err := autopilotSvc.DispatchAutopilot(ctx, ap, pgtype.UUID{}, "webhook",
		pullRequestWebhookPayload(repo, number, "a completely different, edited title"))
	if err != nil {
		t.Fatalf("second DispatchAutopilot: %v", err)
	}
	if second == nil || second.IssueID != first.IssueID {
		t.Fatalf("I6: a title change must not defeat reuse — first=%+v second=%+v", first, second)
	}

	if count := countIssuesForPR(t, ctx, ap.ID, slug); count != 1 {
		t.Fatalf("expected exactly 1 card despite the title change, got %d", count)
	}
}
