package service

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The executor half of the push-restart design: given the plan the pure
// tests pin, restartChainOnPush must cancel the right rows, hand the card
// back to the entry agent, and leave exactly one mention-free comment.
// Needs DATABASE_URL (port 5439 on this machine); skips without it.

func newPushRestartPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
	return pool
}

type pushRestartFixture struct {
	workspaceID pgtype.UUID
	runtimeID   pgtype.UUID
	issueID     pgtype.UUID
	agents      map[string]pgtype.UUID // by name: review-agent, review-validator-agent, fixer
}

func mustUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		t.Fatalf("scan uuid %q: %v", s, err)
	}
	return u
}

func createPushRestartFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) pushRestartFixture {
	t.Helper()
	suffix := time.Now().UnixNano()

	var userID string
	if err := pool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id`,
		"Push Restart Test", fmt.Sprintf("push-restart-%d@multica.ai", suffix)).Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	var workspaceID string
	if err := pool.QueryRow(ctx, `INSERT INTO workspace (name, slug, description, issue_prefix) VALUES ($1, $2, $3, $4) RETURNING id`,
		"Push Restart Test", fmt.Sprintf("push-restart-%d", suffix), "temporary push-restart test workspace", "PRT").Scan(&workspaceID); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		_, _ = pool.Exec(cctx, `DELETE FROM comment WHERE workspace_id = $1`, workspaceID)
		_, _ = pool.Exec(cctx, `DELETE FROM agent_task_queue WHERE issue_id IN (SELECT id FROM issue WHERE workspace_id = $1)`, workspaceID)
		_, _ = pool.Exec(cctx, `DELETE FROM issue WHERE workspace_id = $1`, workspaceID)
		_, _ = pool.Exec(cctx, `DELETE FROM agent WHERE workspace_id = $1`, workspaceID)
		_, _ = pool.Exec(cctx, `DELETE FROM agent_runtime WHERE workspace_id = $1`, workspaceID)
		_, _ = pool.Exec(cctx, `DELETE FROM member WHERE workspace_id = $1`, workspaceID)
		_, _ = pool.Exec(cctx, `DELETE FROM workspace WHERE id = $1`, workspaceID)
		_, _ = pool.Exec(cctx, `DELETE FROM "user" WHERE id = $1`, userID)
	})
	if _, err := pool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`, workspaceID, userID); err != nil {
		t.Fatalf("create member: %v", err)
	}
	var runtimeID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_runtime (workspace_id, daemon_id, name, runtime_mode, provider, status, device_info, metadata, last_seen_at, visibility, owner_id)
		VALUES ($1, NULL, 'Push Restart Runtime', 'cloud', 'push_restart_test', 'online', 'test runtime', '{}'::jsonb, now(), 'private', $2)
		RETURNING id`, workspaceID, userID).Scan(&runtimeID); err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	agents := map[string]pgtype.UUID{}
	for _, name := range []string{"review-agent", "review-validator-agent", "fixer"} {
		var id string
		if err := pool.QueryRow(ctx, `
			INSERT INTO agent (workspace_id, name, description, runtime_mode, runtime_config, runtime_id, visibility, max_concurrent_tasks, owner_id)
			VALUES ($1, $2, '', 'cloud', '{}'::jsonb, $3, 'private', 1, $4)
			RETURNING id`, workspaceID, name, runtimeID, userID).Scan(&id); err != nil {
			t.Fatalf("create agent %s: %v", name, err)
		}
		agents[name] = mustUUID(t, id)
	}
	var issueID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, title, status, priority, creator_id, creator_type, number, position, assignee_type, assignee_id)
		VALUES ($1, 'push restart issue', 'in_progress', 'none', $2, 'member', $3, 0, 'agent', $4)
		RETURNING id`, workspaceID, userID, 980000+int(suffix%1000), agents["review-agent"]).Scan(&issueID); err != nil {
		t.Fatalf("create issue: %v", err)
	}
	return pushRestartFixture{
		workspaceID: mustUUID(t, workspaceID),
		runtimeID:   mustUUID(t, runtimeID),
		issueID:     mustUUID(t, issueID),
		agents:      agents,
	}
}

func (f pushRestartFixture) addTask(t *testing.T, ctx context.Context, pool *pgxpool.Pool, agent, status string) pgtype.UUID {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, issue_id, runtime_id, status) VALUES ($1, $2, $3, $4) RETURNING id`,
		f.agents[agent], f.issueID, f.runtimeID, status).Scan(&id); err != nil {
		t.Fatalf("add task %s/%s: %v", agent, status, err)
	}
	return mustUUID(t, id)
}

func taskStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id pgtype.UUID) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(ctx, `SELECT status FROM agent_task_queue WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatalf("task status: %v", err)
	}
	return s
}

func issueComments(t *testing.T, ctx context.Context, pool *pgxpool.Pool, issueID pgtype.UUID) []struct {
	author  pgtype.UUID
	content string
} {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT author_id, content FROM comment WHERE issue_id = $1 ORDER BY created_at`, issueID)
	if err != nil {
		t.Fatalf("list comments: %v", err)
	}
	defer rows.Close()
	var out []struct {
		author  pgtype.UUID
		content string
	}
	for rows.Next() {
		var c struct {
			author  pgtype.UUID
			content string
		}
		if err := rows.Scan(&c.author, &c.content); err != nil {
			t.Fatalf("scan comment: %v", err)
		}
		out = append(out, c)
	}
	return out
}

func pushRestartServices(pool *pgxpool.Pool) *AutopilotService {
	q := db.New(pool)
	return &AutopilotService{
		Queries:   q,
		TxStarter: pool,
		Bus:       events.New(),
		TaskSvc:   &TaskService{Queries: q, TxStarter: pool, Bus: events.New()},
	}
}

// issueState reads the two columns the restart writes together.
func issueState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id pgtype.UUID) (status string, assignee pgtype.UUID) {
	t.Helper()
	if err := pool.QueryRow(ctx, `SELECT status, assignee_id FROM issue WHERE id = $1`, id).Scan(&status, &assignee); err != nil {
		t.Fatalf("issue state: %v", err)
	}
	return status, assignee
}

// park puts the fixture card into the unassigned shape under test — the
// exact rows the fork's /park (UpdateIssueStatusAndUnassign), the readiness
// gate's needs-human PUT, and MarkIssueBlocked each write.
func (f pushRestartFixture) park(t *testing.T, ctx context.Context, pool *pgxpool.Pool, status string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE issue SET assignee_type = NULL, assignee_id = NULL, status = $2 WHERE id = $1`, f.issueID, status); err != nil {
		t.Fatal(err)
	}
}

func uuidString(t *testing.T, u pgtype.UUID) string {
	t.Helper()
	v, err := u.Value()
	if err != nil {
		t.Fatalf("uuid value: %v", err)
	}
	return v.(string)
}

func TestRestartChainOnPush(t *testing.T) {
	pool := newPushRestartPool(t)
	ctx := context.Background()
	t.Setenv("MULTICA_PUSH_EXEMPT_AGENTS", "fixer")
	run := envelopeRun(t, "github.pull_request", testPushPayload)

	t.Run("reviewer running: cancelled, card stays with the reviewer, one comment", func(t *testing.T) {
		f := createPushRestartFixture(t, ctx, pool)
		svc := pushRestartServices(pool)
		reviewer := f.addTask(t, ctx, pool, "review-agent", "running")
		issue, err := svc.Queries.GetIssue(ctx, f.issueID)
		if err != nil {
			t.Fatal(err)
		}
		ap := db.Autopilot{AssigneeType: "agent", AssigneeID: f.agents["review-agent"]}

		got, err := svc.restartChainOnPush(ctx, ap, run, issue)
		if err != nil {
			t.Fatalf("restartChainOnPush: %v", err)
		}
		if s := taskStatus(t, ctx, pool, reviewer); s != "cancelled" {
			t.Fatalf("reviewer task status = %q, want cancelled", s)
		}
		if got.AssigneeID != f.agents["review-agent"] || got.Status != StatusInProgress {
			t.Fatalf("card = %s/%v, want in_progress/reviewer", got.Status, got.AssigneeID)
		}
		cs := issueComments(t, ctx, pool, f.issueID)
		if len(cs) != 1 {
			t.Fatalf("comments = %d, want 1", len(cs))
		}
		if cs[0].author != f.agents["review-agent"] {
			t.Fatalf("comment author = %v, want the entry agent", cs[0].author)
		}
		if !strings.Contains(cs[0].content, "`d5a9163`") || !strings.Contains(cs[0].content, "review-agent (running)") || strings.Contains(cs[0].content, "no assignee") {
			t.Fatalf("comment = %q", cs[0].content)
		}
		if strings.Contains(cs[0].content, "mention://") {
			t.Fatalf("P2 violated: %q", cs[0].content)
		}
	})

	t.Run("card re-assigned by hand: run cancelled, card handed back to the reviewer", func(t *testing.T) {
		f := createPushRestartFixture(t, ctx, pool)
		svc := pushRestartServices(pool)
		validator := f.addTask(t, ctx, pool, "review-validator-agent", "running")
		if _, err := pool.Exec(ctx, `UPDATE issue SET assignee_id = $2 WHERE id = $1`, f.issueID, f.agents["review-validator-agent"]); err != nil {
			t.Fatal(err)
		}
		issue, err := svc.Queries.GetIssue(ctx, f.issueID)
		if err != nil {
			t.Fatal(err)
		}
		ap := db.Autopilot{AssigneeType: "agent", AssigneeID: f.agents["review-agent"]}

		got, err := svc.restartChainOnPush(ctx, ap, run, issue)
		if err != nil {
			t.Fatalf("restartChainOnPush: %v", err)
		}
		if s := taskStatus(t, ctx, pool, validator); s != "cancelled" {
			t.Fatalf("validator task status = %q, want cancelled", s)
		}
		if got.AssigneeID != f.agents["review-agent"] || got.AssigneeType.String != "agent" {
			t.Fatalf("returned issue assignee = %v/%q, want the reviewer", got.AssigneeID, got.AssigneeType.String)
		}
		status, persisted := issueState(t, ctx, pool, f.issueID)
		if persisted != f.agents["review-agent"] || status != StatusInProgress {
			t.Fatalf("persisted = %s/%v, want in_progress/reviewer", status, persisted)
		}
		if cs := issueComments(t, ctx, pool, f.issueID); len(cs) != 1 {
			t.Fatalf("comments = %d, want 1", len(cs))
		}
	})

	t.Run("fixer running is exempt; a queued reviewer beside it is cancelled", func(t *testing.T) {
		f := createPushRestartFixture(t, ctx, pool)
		svc := pushRestartServices(pool)
		fixer := f.addTask(t, ctx, pool, "fixer", "running")
		queued := f.addTask(t, ctx, pool, "review-agent", "queued")
		issue, err := svc.Queries.GetIssue(ctx, f.issueID)
		if err != nil {
			t.Fatal(err)
		}
		ap := db.Autopilot{AssigneeType: "agent", AssigneeID: f.agents["review-agent"]}

		if _, err := svc.restartChainOnPush(ctx, ap, run, issue); err != nil {
			t.Fatalf("restartChainOnPush: %v", err)
		}
		if s := taskStatus(t, ctx, pool, fixer); s != "running" {
			t.Fatalf("fixer task status = %q, want running (exempt)", s)
		}
		if s := taskStatus(t, ctx, pool, queued); s != "cancelled" {
			t.Fatalf("queued reviewer status = %q, want cancelled", s)
		}
		cs := issueComments(t, ctx, pool, f.issueID)
		if len(cs) != 1 || !strings.Contains(cs[0].content, "review-agent (queued)") || strings.Contains(cs[0].content, "fixer") {
			t.Fatalf("comment = %+v", cs)
		}
	})

	// ---- the rows the push-to-parked-card design adds (§3.1) -----------
	//
	// The fork's /park writes in_review + both assignee fields NULL through
	// UpdateIssueStatusAndUnassign — byte-identical to what park() seeds —
	// and the restart reads nothing but the card row and its tasks (P4), so
	// a human's /park and the pipeline's park are one case here.

	t.Run("parked at in_review with a stale reviewer: cancelled, card taken back, park named in the comment", func(t *testing.T) {
		f := createPushRestartFixture(t, ctx, pool)
		svc := pushRestartServices(pool)
		reviewer := f.addTask(t, ctx, pool, "review-agent", "running")
		f.park(t, ctx, pool, "in_review")
		issue, err := svc.Queries.GetIssue(ctx, f.issueID)
		if err != nil {
			t.Fatal(err)
		}
		ap := db.Autopilot{AssigneeType: "agent", AssigneeID: f.agents["review-agent"]}

		got, err := svc.restartChainOnPush(ctx, ap, run, issue)
		if err != nil {
			t.Fatalf("restartChainOnPush: %v", err)
		}
		if s := taskStatus(t, ctx, pool, reviewer); s != "cancelled" {
			t.Fatalf("stale reviewer status = %q, want cancelled", s)
		}
		if got.Status != StatusInProgress || got.AssigneeType.String != "agent" || got.AssigneeID != f.agents["review-agent"] {
			t.Fatalf("returned card = %s/%q/%v, want in_progress/agent/reviewer", got.Status, got.AssigneeType.String, got.AssigneeID)
		}
		status, assignee := issueState(t, ctx, pool, f.issueID)
		if status != StatusInProgress || assignee != f.agents["review-agent"] {
			t.Fatalf("persisted card = %s/%v, want in_progress/reviewer", status, assignee)
		}
		cs := issueComments(t, ctx, pool, f.issueID)
		if len(cs) != 1 {
			t.Fatalf("comments = %d, want 1", len(cs))
		}
		if !strings.Contains(cs[0].content, "sat at `in_review` with no assignee") || !strings.Contains(cs[0].content, "Park lifted") || !strings.Contains(cs[0].content, "review-agent (running)") {
			t.Fatalf("comment = %q", cs[0].content)
		}
		if cs[0].author != f.agents["review-agent"] {
			t.Fatalf("comment author = %v, want the entry agent", cs[0].author)
		}
	})

	t.Run("blocked, nothing active: card taken back, comment names blocked", func(t *testing.T) {
		f := createPushRestartFixture(t, ctx, pool)
		svc := pushRestartServices(pool)
		f.addTask(t, ctx, pool, "review-agent", "failed")
		f.park(t, ctx, pool, "blocked")
		issue, err := svc.Queries.GetIssue(ctx, f.issueID)
		if err != nil {
			t.Fatal(err)
		}
		ap := db.Autopilot{AssigneeType: "agent", AssigneeID: f.agents["review-agent"]}

		if _, err := svc.restartChainOnPush(ctx, ap, run, issue); err != nil {
			t.Fatalf("restartChainOnPush: %v", err)
		}
		status, assignee := issueState(t, ctx, pool, f.issueID)
		if status != StatusInProgress || assignee != f.agents["review-agent"] {
			t.Fatalf("persisted card = %s/%v, want in_progress/reviewer", status, assignee)
		}
		cs := issueComments(t, ctx, pool, f.issueID)
		if len(cs) != 1 || !strings.Contains(cs[0].content, "sat at `blocked` with no assignee") || strings.Contains(cs[0].content, "Cancelled") {
			t.Fatalf("comment = %+v", cs)
		}
	})

	t.Run("card 221: parked at in_review while the fixer runs through the park — fixer kept, card taken back", func(t *testing.T) {
		f := createPushRestartFixture(t, ctx, pool)
		svc := pushRestartServices(pool)
		fixer := f.addTask(t, ctx, pool, "fixer", "running")
		f.park(t, ctx, pool, "in_review")
		issue, err := svc.Queries.GetIssue(ctx, f.issueID)
		if err != nil {
			t.Fatal(err)
		}
		ap := db.Autopilot{AssigneeType: "agent", AssigneeID: f.agents["review-agent"]}

		if _, err := svc.restartChainOnPush(ctx, ap, run, issue); err != nil {
			t.Fatalf("restartChainOnPush: %v", err)
		}
		if s := taskStatus(t, ctx, pool, fixer); s != "running" {
			t.Fatalf("fixer status = %q, want running (exempt)", s)
		}
		status, assignee := issueState(t, ctx, pool, f.issueID)
		if status != StatusInProgress || assignee != f.agents["review-agent"] {
			t.Fatalf("persisted card = %s/%v, want in_progress/reviewer", status, assignee)
		}
		cs := issueComments(t, ctx, pool, f.issueID)
		if len(cs) != 1 || strings.Contains(cs[0].content, "fixer") || !strings.Contains(cs[0].content, "no assignee") {
			t.Fatalf("comment = %+v", cs)
		}
	})

	t.Run("P1: the status-and-assignee write lands before the comment, in one statement", func(t *testing.T) {
		f := createPushRestartFixture(t, ctx, pool)
		svc := pushRestartServices(pool)
		f.park(t, ctx, pool, "in_review")
		issue, err := svc.Queries.GetIssue(ctx, f.issueID)
		if err != nil {
			t.Fatal(err)
		}
		// A BEFORE INSERT trigger on comment, scoped to this issue, refuses
		// the trace unless the card is already in_progress WITH an assignee.
		// If the write were two statements, or came after the comment, the
		// trigger raises, createAgentComment logs the refusal, and the
		// comment count below is 0.
		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		fn, trg := "prt_restart_before_comment_"+suffix, "prt_trg_restart_before_comment_"+suffix
		drop := func() {
			_, _ = pool.Exec(context.Background(), "DROP TRIGGER IF EXISTS "+trg+" ON comment")
			_, _ = pool.Exec(context.Background(), "DROP FUNCTION IF EXISTS "+fn+"()")
		}
		t.Cleanup(drop)
		if _, err := pool.Exec(ctx, fmt.Sprintf(`
			CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
			DECLARE st text; asg uuid;
			BEGIN
				SELECT status, assignee_id INTO st, asg FROM issue WHERE id = NEW.issue_id;
				IF st <> 'in_progress' OR asg IS NULL THEN
					RAISE EXCEPTION 'restart trace inserted before the card was in_progress with an assignee (status=%%, assignee=%%)', st, asg;
				END IF;
				RETURN NEW;
			END; $$;`, fn)); err != nil {
			t.Fatalf("create trigger function: %v", err)
		}
		if _, err := pool.Exec(ctx, fmt.Sprintf(`
			CREATE TRIGGER %s BEFORE INSERT ON comment FOR EACH ROW
			WHEN (NEW.issue_id = '%s'::uuid) EXECUTE FUNCTION %s();`,
			trg, uuidString(t, f.issueID), fn)); err != nil {
			t.Fatalf("create trigger: %v", err)
		}
		ap := db.Autopilot{AssigneeType: "agent", AssigneeID: f.agents["review-agent"]}

		if _, err := svc.restartChainOnPush(ctx, ap, run, issue); err != nil {
			t.Fatalf("restartChainOnPush: %v", err)
		}
		if cs := issueComments(t, ctx, pool, f.issueID); len(cs) != 1 {
			t.Fatalf("comments = %d, want 1 — the trigger refused the trace, so the write did not precede it", len(cs))
		}
	})

	t.Run("status write refused: error returned, card untouched, ⚠ comment posted by the caller's helper", func(t *testing.T) {
		f := createPushRestartFixture(t, ctx, pool)
		svc := pushRestartServices(pool)
		f.park(t, ctx, pool, "in_review")
		issue, err := svc.Queries.GetIssue(ctx, f.issueID)
		if err != nil {
			t.Fatal(err)
		}
		// The write is tenant-guarded on workspace_id; a card presented under
		// the wrong workspace matches no row, which is the cheapest way to
		// make the one-statement write fail for real.
		wrong := issue
		wrong.WorkspaceID = mustUUID(t, "00000000-0000-0000-0000-000000000001")
		ap := db.Autopilot{AssigneeType: "agent", AssigneeID: f.agents["review-agent"]}

		_, err = svc.restartChainOnPush(ctx, ap, run, wrong)
		if err == nil {
			t.Fatal("expected the refused write to surface as an error")
		}
		svc.explainFailedRestart(ctx, ap, run, wrong, err)
		status, assignee := issueState(t, ctx, pool, f.issueID)
		if status != "in_review" || assignee.Valid {
			t.Fatalf("card moved despite the refused write: %s/%v", status, assignee)
		}
		cs := issueComments(t, ctx, pool, f.issueID)
		if len(cs) != 1 || !strings.HasPrefix(cs[0].content, "⚠ Head moved to `d5a9163`") || !strings.Contains(cs[0].content, "could not restart the review") {
			t.Fatalf("comment = %+v", cs)
		}
		if strings.Contains(cs[0].content, "mention://") {
			t.Fatalf("P2 violated: %q", cs[0].content)
		}
	})
}
