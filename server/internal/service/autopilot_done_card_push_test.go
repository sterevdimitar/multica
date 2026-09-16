package service

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"errors"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/dispatch"
	"github.com/multica-ai/multica/server/internal/issueguard"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A push (synchronize) to a pull request whose card is already at `done`
// dispatches nothing: the merge reconciler owns an approved branch and will
// merge the new head on that card. The two halves pinned here are the
// action gate (pure) and the `done` lookup it consults (database).

func TestPushOnDoneCard(t *testing.T) {
	cases := []struct {
		name   string
		action string
		want   bool
	}{
		{"synchronize is a push", "synchronize", true},
		{"opened is new life, not a push", "opened", false},
		{"reopened is new life, not a push", "reopened", false},
		{"ready_for_review is not a push", "ready_for_review", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := strings.Replace(testPRPayload, `"action":"opened"`, fmt.Sprintf(`"action":%q`, tc.action), 1)
			if got := pushOnDoneCard(envelopeRun(t, "github.pull_request", payload)); got != tc.want {
				t.Fatalf("pushOnDoneCard(%s) = %v, want %v", tc.action, got, tc.want)
			}
		})
	}
	t.Run("no pull request at all", func(t *testing.T) {
		if pushOnDoneCard(envelopeRun(t, "github.pull_request", `{"action":"synchronize"}`)) {
			t.Fatal("an envelope with no pull_request object is not a push to a card")
		}
		if pushOnDoneCard(db.AutopilotRun{Source: "manual"}) {
			t.Fatal("a manual run is not a push")
		}
	})
}

// Needs DATABASE_URL (port 5439 on this machine); skips without it.
func TestFindDonePullRequestIssue(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Skipf("database unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("database unreachable: %v", err)
	}
	t.Cleanup(pool.Close)

	suffix := time.Now().UnixNano()
	var userID string
	if err := pool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id`,
		"Done Card Push Test", fmt.Sprintf("done-card-push-%d@multica.ai", suffix)).Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	var workspaceID string
	if err := pool.QueryRow(ctx, `INSERT INTO workspace (name, slug, description, issue_prefix) VALUES ($1, $2, $3, $4) RETURNING id`,
		"Done Card Push Test", fmt.Sprintf("done-card-push-%d", suffix), "temporary done-card-push test workspace", "DCP").Scan(&workspaceID); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		_, _ = pool.Exec(cctx, `DELETE FROM issue WHERE workspace_id = $1`, workspaceID)
		_, _ = pool.Exec(cctx, `DELETE FROM workspace WHERE id = $1`, workspaceID)
		_, _ = pool.Exec(cctx, `DELETE FROM "user" WHERE id = $1`, userID)
	})
	wsID := mustUUID(t, workspaceID)
	autopilot := mustUUID(t, "11111111-1111-4111-8111-111111111111")
	otherAutopilot := mustUUID(t, "22222222-2222-4222-8222-222222222222")
	const slug = "sterevdimitar/dev-command-center#136"

	// Rows are inserted oldest first; the `done` card is the one a
	// rebase-push must find, and everything else is a decoy the query
	// must not match: another status, another pull request, another
	// autopilot, a cancelled twin.
	insert := func(status, prSlug string, origin [16]byte, number int, at string) string {
		var id string
		if err := pool.QueryRow(ctx, `
			INSERT INTO issue (workspace_id, title, status, priority, creator_type, creator_id, number, position,
			                   origin_type, origin_id, metadata, created_at)
			VALUES ($1, $2, $3, 'none', 'member', $8, $4, 0, 'autopilot', $5, jsonb_build_object('pull_request', $6::text), $7::timestamptz)
			RETURNING id`, workspaceID, "card "+status, status, number, origin, prSlug, at, userID).Scan(&id); err != nil {
			t.Fatalf("insert %s card: %v", status, err)
		}
		return id
	}
	insert("in_review", slug, autopilot.Bytes, 970001, "2026-09-16T06:38:00Z")
	doneID := insert("done", slug, autopilot.Bytes, 970002, "2026-09-16T07:45:00Z")
	insert("cancelled", slug, autopilot.Bytes, 970003, "2026-09-16T07:46:00Z")
	insert("done", "sterevdimitar/dev-command-center#137", autopilot.Bytes, 970004, "2026-09-16T07:47:00Z")
	insert("done", slug, otherAutopilot.Bytes, 970005, "2026-09-16T07:48:00Z")

	q := db.New(pool)
	got, ok, err := issueguard.FindDonePullRequestIssue(ctx, q, wsID, autopilot, slug)
	if err != nil {
		t.Fatalf("FindDonePullRequestIssue: %v", err)
	}
	if !ok {
		t.Fatal("expected the done card to be found")
	}
	if got.ID.Bytes != mustUUID(t, doneID).Bytes {
		t.Fatalf("found card %x, want the done card %s", got.ID.Bytes, doneID)
	}

	if _, ok, err := issueguard.FindDonePullRequestIssue(ctx, q, wsID, autopilot, "sterevdimitar/dev-command-center#999"); err != nil || ok {
		t.Fatalf("a pull request with no done card: ok=%v err=%v, want a miss", ok, err)
	}
	if _, ok, err := issueguard.FindDonePullRequestIssue(ctx, q, wsID, mustUUID(t, "33333333-3333-4333-8333-333333333333"), slug); err != nil || ok {
		t.Fatalf("another autopilot's cards must not match: ok=%v err=%v", ok, err)
	}
	if _, ok, err := issueguard.FindDonePullRequestIssue(ctx, q, wsID, autopilot, ""); err != nil || ok {
		t.Fatalf("an empty slug never matches: ok=%v err=%v", ok, err)
	}
}

// The wiring: dispatchCreateIssue, handed a synchronize for a pull request
// whose only card is at `done`, returns the skip sentinel with the
// already_active code and writes no issue row. This is the row-level
// statement of "a push to an approved branch dispatches nothing".
// Needs DATABASE_URL (port 5439 on this machine); skips without it.
func TestDispatchCreateIssueSkipsPushToDoneCard(t *testing.T) {
	pool := newPushRestartPool(t)
	ctx := context.Background()
	f := createPushRestartFixture(t, ctx, pool)
	svc := pushRestartServices(pool)

	var userID pgtype.UUID
	if err := pool.QueryRow(ctx, `SELECT creator_id FROM issue WHERE id = $1`, f.issueID).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	var apID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO autopilot (workspace_id, title, assignee_type, assignee_id, status, execution_mode, created_by_type, created_by_id)
		VALUES ($1, 'PR Review', 'agent', $2, 'active', 'create_issue', 'member', $3)
		RETURNING id`, f.workspaceID, f.agents["review-agent"], userID).Scan(&apID); err != nil {
		t.Fatalf("create autopilot: %v", err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		_, _ = pool.Exec(cctx, `DELETE FROM autopilot WHERE id = $1`, apID)
	})
	ap, err := svc.Queries.GetAutopilotInWorkspace(ctx, db.GetAutopilotInWorkspaceParams{ID: mustUUID(t, apID), WorkspaceID: f.workspaceID})
	if err != nil {
		t.Fatalf("load autopilot: %v", err)
	}

	// The fixture's card becomes the pull request's `done` card: the shape
	// card 255 was in when the reconciler rebased PR #136 on 2026-09-16.
	const slug = "sterevdimitar/dev-command-center#26" // testPRPayload's number and repository
	if _, err := pool.Exec(ctx, `
		UPDATE issue SET status = 'done', origin_type = 'autopilot', origin_id = $2,
		                 metadata = jsonb_build_object('pull_request', $3::text)
		WHERE id = $1`, f.issueID, ap.ID, slug); err != nil {
		t.Fatal(err)
	}
	before := issueCount(t, ctx, pool, f.workspaceID)

	push := envelopeRun(t, "github.pull_request", strings.Replace(testPRPayload, `"action":"opened"`, `"action":"synchronize"`, 1))
	err = svc.dispatchCreateIssue(ctx, ap, &push, "UTC", pgtype.UUID{})

	var skip *errDispatchSkipped
	if !errors.As(err, &skip) {
		t.Fatalf("dispatchCreateIssue = %v, want the skip sentinel", err)
	}
	if skip.code != dispatch.ReasonAlreadyActive {
		t.Fatalf("skip code = %q, want %q", skip.code, dispatch.ReasonAlreadyActive)
	}
	if !strings.Contains(skip.reason, slug) || !strings.Contains(skip.reason, "done") {
		t.Fatalf("skip reason should name the pull request and the done card: %q", skip.reason)
	}
	if after := issueCount(t, ctx, pool, f.workspaceID); after != before {
		t.Fatalf("issue rows went %d -> %d; a push to a done card must create no card", before, after)
	}
	if status, _ := issueState(t, ctx, pool, f.issueID); status != "done" {
		t.Fatalf("the done card was written to (%s); the skip must leave it alone", status)
	}
}

func issueCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workspaceID pgtype.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM issue WHERE workspace_id = $1`, workspaceID).Scan(&n); err != nil {
		t.Fatalf("count issues: %v", err)
	}
	return n
}
