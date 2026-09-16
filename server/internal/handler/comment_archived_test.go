package handler

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
)

// An archived card is inert for comments: an @mention on it yields no
// trigger on the submit path and none on the preview path, and a member
// comment that mentions nobody does not fall back to the assignee either.
func TestMentionOnArchivedCardEnqueuesNoTask(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "Archived Mention Agent", nil)
	issueID := createSteerVerbIssueWithStatus(t, "mention on archived card", agentID, service.StatusArchived)

	content := fmt.Sprintf("[@Agent](mention://agent/%s) please look", agentID)
	preview := previewCommentTriggersForTest(t, issueID, CommentTriggerPreviewRequest{Content: content})
	if len(preview.Agents) != 0 {
		t.Fatalf("preview agents on archived card = %+v, want none", preview.Agents)
	}

	postSteerComment(t, issueID, content)
	postSteerComment(t, issueID, "a plain member comment that would wake the assignee elsewhere")

	if got := countAllTasksForIssue(t, issueID); got != 0 {
		t.Fatalf("task rows after comments on an archived card = %d, want 0", got)
	}
	status, assigneeType, _ := loadIssueAssigneeAndStatus(t, issueID)
	if status != service.StatusArchived || assigneeType == nil {
		t.Fatalf("card after comments = %q / assignee %v; want archived with its assignee kept", status, assigneeType)
	}
}

// /park is a status write (in_review + unassign) and would silently
// un-archive a card. On an archived card it is refused with an ack that
// says so; the card and its assignee are untouched and nothing runs.
func TestParkOnArchivedCardIsRefused(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "Archived Park Agent", nil)
	issueID := createSteerVerbIssueWithStatus(t, "park on archived card", agentID, service.StatusArchived)

	postSteerComment(t, issueID, "/park pulling this back")

	status, assigneeType, assigneeID := loadIssueAssigneeAndStatus(t, issueID)
	if status != service.StatusArchived {
		t.Errorf("status = %q, want archived (park must be refused)", status)
	}
	if assigneeType == nil || assigneeID == nil {
		t.Errorf("assignee cleared by a refused /park; want it kept")
	}
	if got := countAllTasksForIssue(t, issueID); got != 0 {
		t.Errorf("task rows after /park on archived card = %d, want 0", got)
	}
	var latest string
	if err := testPool.QueryRow(context.Background(),
		`SELECT content FROM comment WHERE issue_id = $1 ORDER BY created_at DESC LIMIT 1`, issueID).Scan(&latest); err != nil {
		t.Fatalf("load latest comment: %v", err)
	}
	if !strings.Contains(latest, "Archived") {
		t.Errorf("latest comment = %q, want the archived-park ack", latest)
	}
}

// /resume on an archived card is what it is on every card: the comment is
// stored and nothing else happens — no status write, no task.
func TestResumeOnArchivedCardIsStoredOnly(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "Archived Resume Agent", nil)
	issueID := createSteerVerbIssueWithStatus(t, "resume on archived card", agentID, service.StatusArchived)

	postSteerComment(t, issueID, "/resume")

	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM comment WHERE issue_id = $1`, issueID).Scan(&n); err != nil {
		t.Fatalf("count comments: %v", err)
	}
	if n != 1 {
		t.Errorf("comments after /resume = %d, want 1 (the verb itself, no ack)", n)
	}
	if status, _, _ := loadIssueAssigneeAndStatus(t, issueID); status != service.StatusArchived {
		t.Errorf("status = %q, want archived", status)
	}
	if got := countAllTasksForIssue(t, issueID); got != 0 {
		t.Errorf("task rows after /resume on archived card = %d, want 0", got)
	}
}

// Same invariants as TestParkAckBodyInvariants: the ack can never itself be
// parsed as a control verb or dispatch an agent.
func TestArchivedParkAckBodyInvariants(t *testing.T) {
	body := archivedParkAckBody()
	if strings.HasPrefix(body, "/") {
		t.Errorf("archivedParkAckBody() = %q, begins with /", body)
	}
	if strings.Contains(body, "mention://") {
		t.Errorf("archivedParkAckBody() = %q, contains mention://", body)
	}
	if !strings.Contains(body, "Archived") {
		t.Errorf("archivedParkAckBody() = %q, does not name the column", body)
	}
}
