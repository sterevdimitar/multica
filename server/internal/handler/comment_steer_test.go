package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
)

// createSteerVerbIssue creates an issue in in_progress assigned to agentID
// (agent assignment lets a plain member comment route via the assignee
// fallback, and lets /park's assignee-clear be observable). Cleanup mirrors
// createCommentTriggerPreviewIssue.
func createSteerVerbIssue(t *testing.T, title, agentID string) string {
	t.Helper()
	ctx := context.Background()

	var number int
	if err := testPool.QueryRow(ctx, `
		UPDATE workspace
		SET issue_counter = GREATEST(issue_counter, (SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)) + 1
		WHERE id = $1 RETURNING issue_counter
	`, testWorkspaceID).Scan(&number); err != nil {
		t.Fatalf("next issue number: %v", err)
	}

	var issueID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, creator_type, creator_id, title, status, priority, assignee_type, assignee_id, number)
		VALUES ($1, 'member', $2, $3, 'in_progress', 'none', 'agent', $4, $5)
		RETURNING id
	`, testWorkspaceID, testUserID, title, agentID, number).Scan(&issueID); err != nil {
		t.Fatalf("create issue: %v", err)
	}

	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
		testPool.Exec(context.Background(), `DELETE FROM comment WHERE issue_id = $1`, issueID)
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
	})

	return issueID
}

// postSteerComment posts content as the default test member (X-User-ID:
// testUserID, the only author_type ParseVerb reads verbs from) and returns
// the decoded response. Fails the test on a non-201.
func postSteerComment(t *testing.T, issueID, content string) CommentResponse {
	t.Helper()
	w := httptest.NewRecorder()
	r := withURLParam(newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": content}), "id", issueID)
	testHandler.CreateComment(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateComment(%q): expected 201, got %d: %s", content, w.Code, w.Body.String())
	}
	var resp CommentResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode comment: %v", err)
	}
	return resp
}

// countAllTasksForIssue counts every agent_task_queue row for issueID,
// regardless of status - the strictest form of "zero rows added" the plan's
// contract table asks for.
func countAllTasksForIssue(t *testing.T, issueID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID).Scan(&n); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	return n
}

// loadIssueAssigneeAndStatus reads back the fields /park's contract governs.
func loadIssueAssigneeAndStatus(t *testing.T, issueID string) (status string, assigneeType *string, assigneeID *string) {
	t.Helper()
	var st string
	var at, aid *string
	if err := testPool.QueryRow(context.Background(),
		`SELECT status, assignee_type, assignee_id FROM issue WHERE id = $1`, issueID).Scan(&st, &at, &aid); err != nil {
		t.Fatalf("load issue: %v", err)
	}
	return st, at, aid
}

// installParkCommentBeforeUnassignTrigger makes an INSERT into comment fail
// for issueID if, at the moment of insert, the issue still has a non-NULL
// assignee_id - but ONLY for a row whose content does not start with "/".
// That scoping matters: the human's own "/park ..." comment is (correctly)
// stored by CreateComment BEFORE parkIssueForComment runs at all, while the
// issue is still assigned, so a trigger that fired on every insert would
// trip on that legitimate write and never isolate the ack. parkAckBody never
// begins with "/" (see its doc), so "does not start with /" is exactly "is
// the ack, not the human's control-verb comment" for this test's purposes.
// It exists to prove the ack lands strictly after the park write's
// assignee-clear, not merely that both are true by the time the test looks
// at end state - the same technique installCommentBeforeAssigneeClearTrigger
// uses in internal/service/issue_lifecycle_db_test.go, reimplemented here
// because that one is unexported in a different package. Fixed
// trigger/function name plus DROP ... IF EXISTS makes reruns self-healing;
// do not run this test concurrently with another copy of itself.
func installParkCommentBeforeUnassignTrigger(t *testing.T, issueID string) {
	t.Helper()
	ctx := context.Background()
	const triggerName = "steer_park_comment_assignee_ordering"
	const functionName = triggerName + "_fn"

	drop := func() {
		testPool.Exec(context.Background(), "DROP TRIGGER IF EXISTS "+triggerName+" ON comment")
		testPool.Exec(context.Background(), "DROP FUNCTION IF EXISTS "+functionName+"()")
	}
	drop()
	t.Cleanup(drop)

	if _, err := testPool.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION %s()
		RETURNS trigger
		LANGUAGE plpgsql
		AS $$
		DECLARE
			still_assigned uuid;
		BEGIN
			SELECT assignee_id INTO still_assigned FROM issue WHERE id = NEW.issue_id;
			IF still_assigned IS NOT NULL THEN
				RAISE EXCEPTION 'steer_park_comment_assignee: comment inserted before assignee was cleared';
			END IF;
			RETURN NEW;
		END;
		$$;
	`, functionName)); err != nil {
		t.Fatalf("create park-comment-before-unassign trigger function: %v", err)
	}
	if _, err := testPool.Exec(ctx, fmt.Sprintf(`
		CREATE TRIGGER %s
		BEFORE INSERT ON comment
		FOR EACH ROW
		WHEN (NEW.issue_id = %s::uuid AND NEW.content NOT LIKE '/%%')
		EXECUTE FUNCTION %s();
	`, triggerName, quoteLiteralForTest(issueID), functionName)); err != nil {
		t.Fatalf("create park-comment-before-unassign trigger: %v", err)
	}
}

// quoteLiteralForTest quote-escapes a UUID string for interpolation into a
// WHEN clause. UUIDs never contain a single quote, but this avoids trusting
// that invariant silently the way plain fmt.Sprintf("%s") would.
func quoteLiteralForTest(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func TestParkCommentEnqueuesNoTask(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "Park Agent 1", nil)
	issueID := createSteerVerbIssue(t, "park enqueues no task", agentID)

	postSteerComment(t, issueID, "/park taking a look myself")

	if got := countAllTasksForIssue(t, issueID); got != 0 {
		t.Fatalf("task rows after /park = %d, want 0", got)
	}
}

func TestParkCommentParksTheCard(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "Park Agent 2", nil)
	issueID := createSteerVerbIssue(t, "park parks the card", agentID)

	postSteerComment(t, issueID, "/park needs a human decision")

	status, assigneeType, assigneeID := loadIssueAssigneeAndStatus(t, issueID)
	if status != service.StatusInReview {
		t.Errorf("status = %q, want %q", status, service.StatusInReview)
	}
	if assigneeType != nil {
		t.Errorf("assignee_type = %v, want NULL", *assigneeType)
	}
	if assigneeID != nil {
		t.Errorf("assignee_id = %v, want NULL", *assigneeID)
	}
}

func TestParkCommentPostsTheAckAfterUnassigning(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "Park Agent 3", nil)
	issueID := createSteerVerbIssue(t, "park ack ordering", agentID)

	installParkCommentBeforeUnassignTrigger(t, issueID)

	postSteerComment(t, issueID, "/park ordering check")

	// If the ack insert had raced ahead of the assignee-clear, the trigger
	// above would have raised inside parkIssueForComment's own
	// postControlVerbAck call, which swallows the error and posts nothing.
	// Two rows here (the /park comment itself, stored before the trigger
	// existed to fire on it, plus the ack) prove the ack was inserted only
	// once assignee_id was already NULL.
	var count int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM comment WHERE issue_id = $1`, issueID).Scan(&count); err != nil {
		t.Fatalf("count comments: %v", err)
	}
	if count != 2 {
		t.Fatalf("comment count = %d, want 2 (the /park comment plus its ack, proving the ack landed after the assignee was cleared)", count)
	}
}

func TestResumeEnqueuesNoTask(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "Resume Agent", nil)
	issueID := createSteerVerbIssue(t, "resume enqueues no task", agentID)

	statusBefore, assigneeTypeBefore, assigneeIDBefore := loadIssueAssigneeAndStatus(t, issueID)

	postSteerComment(t, issueID, "/resume")

	if got := countAllTasksForIssue(t, issueID); got != 0 {
		t.Fatalf("task rows after /resume = %d, want 0", got)
	}
	statusAfter, assigneeTypeAfter, assigneeIDAfter := loadIssueAssigneeAndStatus(t, issueID)
	if statusAfter != statusBefore {
		t.Errorf("status changed: %q -> %q, want unchanged", statusBefore, statusAfter)
	}
	if (assigneeTypeBefore == nil) != (assigneeTypeAfter == nil) || (assigneeTypeBefore != nil && *assigneeTypeBefore != *assigneeTypeAfter) {
		t.Errorf("assignee_type changed: %v -> %v, want unchanged", assigneeTypeBefore, assigneeTypeAfter)
	}
	if (assigneeIDBefore == nil) != (assigneeIDAfter == nil) || (assigneeIDBefore != nil && *assigneeIDBefore != *assigneeIDAfter) {
		t.Errorf("assignee_id changed: %v -> %v, want unchanged", assigneeIDBefore, assigneeIDAfter)
	}
}

func TestUnknownVerbRepliesAndEnqueuesNoTask(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "Unknown Verb Agent", nil)
	issueID := createSteerVerbIssue(t, "unknown verb replies", agentID)

	postSteerComment(t, issueID, "/frobnicate the thing")

	if got := countAllTasksForIssue(t, issueID); got != 0 {
		t.Fatalf("task rows after unknown verb = %d, want 0", got)
	}

	var replyContent string
	if err := testPool.QueryRow(context.Background(),
		`SELECT content FROM comment WHERE issue_id = $1 ORDER BY created_at DESC LIMIT 1`, issueID).Scan(&replyContent); err != nil {
		t.Fatalf("load reply comment: %v", err)
	}
	if strings.HasPrefix(replyContent, "/") {
		t.Fatalf("ack begins with %q, must never begin with /", replyContent[:1])
	}
	for _, verb := range []string{"/park", "/resume", "/note"} {
		if !strings.Contains(replyContent, verb) {
			t.Errorf("ack %q does not name %s", replyContent, verb)
		}
	}
}

func TestOrdinaryCommentStillEnqueues(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "Ordinary Comment Agent", nil)
	issueID := createSteerVerbIssue(t, "ordinary comment still enqueues", agentID)

	postSteerComment(t, issueID, "please take a look at this")

	if got := countAllTasksForIssue(t, issueID); got != 1 {
		t.Fatalf("task rows after ordinary comment = %d, want 1", got)
	}
}

// TestNoteCommentStillSuppressesTriggeringAsToday pins upstream's /note
// behavior (MUL-3115) as unchanged by this task: /note is NOT a control verb
// for handleControlVerb's purposes (ParseVerb's VerbNote falls through to
// the default branch in CreateComment), and isNoteComment - untouched here -
// keeps suppressing its trigger exactly as before.
func TestNoteCommentStillSuppressesTriggeringAsToday(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "Note Comment Agent", nil)
	issueID := createSteerVerbIssue(t, "note still suppresses triggering", agentID)

	resp := postSteerComment(t, issueID, "/note just leaving context, no run please")

	if resp.ID == "" {
		t.Fatal("note comment was not stored")
	}
	if got := countAllTasksForIssue(t, issueID); got != 0 {
		t.Fatalf("task rows after /note = %d, want 0", got)
	}
	status, assigneeType, _ := loadIssueAssigneeAndStatus(t, issueID)
	if status != "in_progress" {
		t.Errorf("status = %q, want unchanged in_progress (/note must not park)", status)
	}
	if assigneeType == nil || *assigneeType != "agent" {
		t.Errorf("assignee_type = %v, want still 'agent' (/note must not unassign)", assigneeType)
	}
}

// TestConsumedVerbIsNotReplayedOnCompletion is the runaway regression test:
// a consumed /park comment must not be replayed by
// reconcileCommentsOnCompletion when a later task on the same issue
// completes. Before this task's fix, a /park comment created no task and so
// left no delivered_comment_ids receipt; reconcileCommentsOnCompletion
// treated it as an undelivered comment and replayed it through the normal
// trigger path on the next completion, dispatching a fresh agent run onto a
// card a human had just asked to be left alone - the 2026-07-23 runaway's
// shape, reached via a different route.
func TestConsumedVerbIsNotReplayedOnCompletion(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	var agentID, runtimeID string
	if err := testPool.QueryRow(ctx,
		`SELECT id, runtime_id FROM agent WHERE workspace_id = $1 AND runtime_id IS NOT NULL LIMIT 1`,
		testWorkspaceID).Scan(&agentID, &runtimeID); err != nil {
		t.Fatalf("setup: get agent: %v", err)
	}

	issueID := createSteerVerbIssue(t, "consumed verb not replayed", agentID)

	// Trigger comment created before the run starts, exactly like the
	// existing MUL-4195 reconcile fixture.
	var triggerCommentID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type, created_at)
		VALUES ($1, $2, 'member', $3, 'initial request', 'comment', now() - interval '10 minutes')
		RETURNING id
	`, issueID, testWorkspaceID, testUserID).Scan(&triggerCommentID); err != nil {
		t.Fatalf("setup: trigger comment: %v", err)
	}

	// A running task whose started_at is in the past, and whose delivered
	// set contains only the trigger comment - so anything else on the issue
	// counts as "undelivered" to reconcileCommentsOnCompletion.
	var taskID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, trigger_comment_id, delivered_comment_ids, status, priority, created_at, started_at)
		VALUES ($1, $2, $3, $4, ARRAY[$4::uuid], 'running', 0, now() - interval '10 minutes', now() - interval '5 minutes')
		RETURNING id
	`, agentID, runtimeID, issueID, triggerCommentID).Scan(&taskID); err != nil {
		t.Fatalf("setup: running task: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	})

	// A /park comment posted while the run is busy - via the real handler,
	// so it goes through the exact same interception as production traffic.
	// It parks the card (clearing the assignee) but must create no task, so
	// it is left "undelivered" from task1's point of view.
	//
	// The reason deliberately @-mentions the SAME agent whose task is about
	// to complete. This is not incidental: if reconcileCommentsOnCompletion
	// ever replayed this comment, an assignee-based route would find nothing
	// (park just cleared the assignee), so a test without this mention would
	// pass "by accident" - closed off by the wrong mechanism - even with the
	// guard this test exists to pin removed entirely. The explicit mention
	// gives the replay path a live, assignee-independent route to the same
	// agent, so the only thing standing between this comment and a fresh
	// task is the isConsumedControlVerbComment guard in daemon.go.
	parkContent := fmt.Sprintf("/park stop, [@Agent](mention://agent/%s) look at this", agentID)
	postSteerComment(t, issueID, parkContent)

	if w := completeTaskViaHandler(t, taskID, "done"); w.Code != http.StatusOK {
		t.Fatalf("CompleteTask: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if n := pendingTaskCountForAgentIssue(t, issueID, agentID); n != 0 {
		t.Fatalf("follow-up tasks scheduled by reconcile after /park = %d, want 0 (the /park comment must not be replayed)", n)
	}
}

// TestNoteCommentIsNotReplayedOnCompletion is the pre-existing sibling
// guarantee this task must not disturb: a /note comment (upstream's own
// mechanism, untouched here) is likewise never replayed on completion. Same
// shape as TestConsumedVerbIsNotReplayedOnCompletion, proving daemon.go's
// isNoteComment guard still does its job unmodified.
func TestNoteCommentIsNotReplayedOnCompletion(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	var agentID, runtimeID string
	if err := testPool.QueryRow(ctx,
		`SELECT id, runtime_id FROM agent WHERE workspace_id = $1 AND runtime_id IS NOT NULL LIMIT 1`,
		testWorkspaceID).Scan(&agentID, &runtimeID); err != nil {
		t.Fatalf("setup: get agent: %v", err)
	}

	issueID := createSteerVerbIssue(t, "note not replayed", agentID)

	var triggerCommentID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type, created_at)
		VALUES ($1, $2, 'member', $3, 'initial request', 'comment', now() - interval '10 minutes')
		RETURNING id
	`, issueID, testWorkspaceID, testUserID).Scan(&triggerCommentID); err != nil {
		t.Fatalf("setup: trigger comment: %v", err)
	}

	var taskID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, trigger_comment_id, delivered_comment_ids, status, priority, created_at, started_at)
		VALUES ($1, $2, $3, $4, ARRAY[$4::uuid], 'running', 0, now() - interval '10 minutes', now() - interval '5 minutes')
		RETURNING id
	`, agentID, runtimeID, issueID, triggerCommentID).Scan(&taskID); err != nil {
		t.Fatalf("setup: running task: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	})

	postSteerComment(t, issueID, "/note just context, nothing to run")

	if w := completeTaskViaHandler(t, taskID, "done"); w.Code != http.StatusOK {
		t.Fatalf("CompleteTask: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if n := pendingTaskCountForAgentIssue(t, issueID, agentID); n != 0 {
		t.Fatalf("follow-up tasks scheduled by reconcile after /note = %d, want 0", n)
	}
}
