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
		if got.AssigneeID != f.agents["review-agent"] {
			t.Fatalf("assignee moved: %v", got.AssigneeID)
		}
		cs := issueComments(t, ctx, pool, f.issueID)
		if len(cs) != 1 {
			t.Fatalf("comments = %d, want 1", len(cs))
		}
		if cs[0].author != f.agents["review-agent"] {
			t.Fatalf("comment author = %v, want the entry agent", cs[0].author)
		}
		if !strings.Contains(cs[0].content, "`d5a9163`") || !strings.Contains(cs[0].content, "review-agent (running)") {
			t.Fatalf("comment = %q", cs[0].content)
		}
		if strings.Contains(cs[0].content, "mention://") {
			t.Fatalf("I2 violated: %q", cs[0].content)
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
		var persisted pgtype.UUID
		if err := pool.QueryRow(ctx, `SELECT assignee_id FROM issue WHERE id = $1`, f.issueID).Scan(&persisted); err != nil {
			t.Fatal(err)
		}
		if persisted != f.agents["review-agent"] {
			t.Fatalf("persisted assignee = %v, want the reviewer", persisted)
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

	t.Run("no assignee: nothing cancelled, no comment, issue returned unchanged", func(t *testing.T) {
		f := createPushRestartFixture(t, ctx, pool)
		svc := pushRestartServices(pool)
		reviewer := f.addTask(t, ctx, pool, "review-agent", "running")
		if _, err := pool.Exec(ctx, `UPDATE issue SET assignee_type = NULL, assignee_id = NULL, status = 'in_review' WHERE id = $1`, f.issueID); err != nil {
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
		if s := taskStatus(t, ctx, pool, reviewer); s != "running" {
			t.Fatalf("parked card: reviewer status = %q, want running (untouched)", s)
		}
		if got.AssigneeID.Valid {
			t.Fatalf("parked card must not be re-assigned: %v", got.AssigneeID)
		}
		if cs := issueComments(t, ctx, pool, f.issueID); len(cs) != 0 {
			t.Fatalf("parked card must get no comment: %+v", cs)
		}
	})
}
