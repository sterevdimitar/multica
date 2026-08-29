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

// pullRequestWebhookPayload builds the {event, eventPayload} envelope bytes
// pullRequestSlug reads, for a synthetic GitHub pull_request webhook
// delivery. Mirrors internal/service's testPRPayload shape.
func pullRequestWebhookPayload(repo string, number int, title string) []byte {
	return []byte(fmt.Sprintf(
		`{"event":"github.pull_request","eventPayload":{"action":"opened","number":%d,`+
			`"pull_request":{"html_url":"https://github.com/%s/pull/%d","title":%q,`+
			`"user":{"login":"u"},"head":{"ref":"h"},"base":{"ref":"main"}},`+
			`"repository":{"full_name":%q}}}`,
		number, repo, number, title, repo,
	))
}

// TestDispatchCreateIssueRecordsPullRequestMetadata is the Task 3 spec test:
// a PR-triggered dispatchCreateIssue must write issue.metadata.pull_request
// as "owner/repo#N" (§3.1 of the design).
func TestDispatchCreateIssueRecordsPullRequestMetadata(t *testing.T) {
	ctx := context.Background()
	queries := db.New(testPool)
	bus := events.New()
	taskSvc := service.NewTaskService(queries, testPool, nil, bus)
	autopilotSvc := service.NewAutopilotService(queries, testPool, bus, taskSvc)

	var agentID string
	if err := testPool.QueryRow(ctx,
		`SELECT id::text FROM agent WHERE workspace_id = $1 ORDER BY created_at ASC LIMIT 1`,
		testWorkspaceID,
	).Scan(&agentID); err != nil {
		t.Fatalf("load fixture agent: %v", err)
	}

	ap, err := queries.CreateAutopilot(ctx, db.CreateAutopilotParams{
		WorkspaceID:        parseUUID(testWorkspaceID),
		Title:              "PR metadata test",
		Description:        pgtype.Text{String: "PR metadata test", Valid: true},
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

	payload := pullRequestWebhookPayload("acme/widgets", 42, "PR title "+time.Now().UTC().Format("150405.000000000"))
	run, err := autopilotSvc.DispatchAutopilot(ctx, ap, pgtype.UUID{}, "webhook", payload)
	if err != nil {
		t.Fatalf("DispatchAutopilot: %v", err)
	}
	if run == nil || run.Status != "issue_created" || !run.IssueID.Valid {
		t.Fatalf("PR-triggered dispatch = %+v, want issue_created with issue_id", run)
	}

	var slug string
	if err := testPool.QueryRow(ctx,
		`SELECT coalesce(metadata ->> 'pull_request', '') FROM issue WHERE id = $1`, run.IssueID,
	).Scan(&slug); err != nil {
		t.Fatalf("read issue metadata: %v", err)
	}
	if slug != "acme/widgets#42" {
		t.Fatalf("issue metadata pull_request = %q, want acme/widgets#42", slug)
	}
}

// TestDispatchCreateIssueNonPRRunHasNoPullRequestKey pins I3: a non-PR run
// must write NO pull_request metadata key at all, not an empty string.
func TestDispatchCreateIssueNonPRRunHasNoPullRequestKey(t *testing.T) {
	ctx := context.Background()
	queries := db.New(testPool)
	bus := events.New()
	taskSvc := service.NewTaskService(queries, testPool, nil, bus)
	autopilotSvc := service.NewAutopilotService(queries, testPool, bus, taskSvc)

	var agentID string
	if err := testPool.QueryRow(ctx,
		`SELECT id::text FROM agent WHERE workspace_id = $1 ORDER BY created_at ASC LIMIT 1`,
		testWorkspaceID,
	).Scan(&agentID); err != nil {
		t.Fatalf("load fixture agent: %v", err)
	}

	title := "Non-PR metadata test " + time.Now().UTC().Format("150405.000000000")
	ap, err := queries.CreateAutopilot(ctx, db.CreateAutopilotParams{
		WorkspaceID:        parseUUID(testWorkspaceID),
		Title:              "Non-PR metadata test autopilot",
		Description:        pgtype.Text{String: "Non-PR metadata test", Valid: true},
		AssigneeType:       "agent",
		AssigneeID:         parseUUID(agentID),
		Status:             "active",
		ExecutionMode:      "create_issue",
		IssueTitleTemplate: pgtype.Text{String: title, Valid: true},
		CreatedByType:      "member",
		CreatedByID:        parseUUID(testUserID),
	})
	if err != nil {
		t.Fatalf("CreateAutopilot: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = testPool.Exec(bg, `DELETE FROM issue WHERE workspace_id = $1 AND title = $2`, testWorkspaceID, title)
		_, _ = testPool.Exec(bg, `DELETE FROM autopilot WHERE id = $1`, ap.ID)
	})

	run, err := autopilotSvc.DispatchAutopilot(ctx, ap, pgtype.UUID{}, "manual", nil)
	if err != nil {
		t.Fatalf("DispatchAutopilot: %v", err)
	}
	if run == nil || run.Status != "issue_created" || !run.IssueID.Valid {
		t.Fatalf("dispatch = %+v, want issue_created with issue_id", run)
	}

	var hasKey bool
	if err := testPool.QueryRow(ctx,
		`SELECT metadata ? 'pull_request' FROM issue WHERE id = $1`, run.IssueID,
	).Scan(&hasKey); err != nil {
		t.Fatalf("read issue metadata: %v", err)
	}
	if hasKey {
		t.Fatal("non-PR run must write NO pull_request metadata key (I3)")
	}
}
