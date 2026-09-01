package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Issue status constants, mirroring the CHECK constraint on issue.status
// (server/migrations/001_init.up.sql:58):
//
//	CHECK (status IN ('backlog', 'todo', 'in_progress', 'in_review', 'done',
//	                   'blocked', 'cancelled'))
const (
	StatusBacklog    = "backlog"
	StatusTodo       = "todo"
	StatusInProgress = "in_progress"
	StatusInReview   = "in_review"
	StatusDone       = "done"
	StatusBlocked    = "blocked"
	StatusCancelled  = "cancelled"
)

// runStartStatusByAgent maps an agent name to the issue status a dispatched
// task should write for it. It is empty today because every agent maps to
// StatusInProgress (see RunStartStatus); it is the extension point for
// per-phase statuses (reviewing / fixing / judging), which additionally
// require a migration to the status CHECK constraint before they can be
// used here.
var runStartStatusByAgent = map[string]string{}

// RunStartStatus returns the issue status a dispatched task writes for the
// given agent. Every agent maps to StatusInProgress today; see
// runStartStatusByAgent above for the extension point and why it needs a
// migration first.
func RunStartStatus(agentName string) string {
	if status, ok := runStartStatusByAgent[agentName]; ok {
		return status
	}
	return StatusInProgress
}

// promotableStatuses is the allow-list of statuses a starting run may write
// StatusInProgress over. Terminal statuses (done, cancelled) and the
// human-decision status (in_review) are deliberately excluded: a dispatch
// arriving while one of those holds must never overwrite it. Any status not in
// this list - including one added by a future migration - is refused by
// default.
//
// Both writers that consult MayPromoteToRunning - MarkIssueRunning (starting a
// run) and MarkIssueBlocked (parking a failed one) - share this exact list and
// refuse the same three statuses (in_review, done, cancelled) for the same
// reasons: a pending human decision, a merge trigger, and a terminal state
// must not be touched by either writer, even though their motivations for
// checking are opposite (promoting vs. parking).
//
// StatusInProgress is in the list on purpose, and must stay. A card can be
// dispatched to while already running - a second run against the same card, or
// a retry - and that has to be a permitted write rather than a refusal (it is
// a real write: it bumps updated_at and broadcasts issue:updated with
// status_changed: false, not a no-op). Removing this entry would not break any
// caller loudly; it would just make the promotion path return early for every
// card after the first dispatch. The same applies to MarkIssueBlocked
// re-blocking an already-blocked card.
var promotableStatuses = map[string]bool{
	StatusBacklog:    true,
	StatusTodo:       true,
	StatusBlocked:    true,
	StatusInProgress: true,
}

// MayPromoteToRunning reports whether a card at the given status may be
// touched by either lifecycle writer - a starting run promoting it to running,
// or a failed run parking it as blocked. See promotableStatuses for the list
// and why it is shared.
func MayPromoteToRunning(currentStatus string) bool {
	return promotableStatuses[currentStatus]
}

// MarkIssueRunning writes the starting status for a dispatched task's issue.
// Best-effort: every write failure is logged and swallowed, and a refusal by
// the guard below is logged too (at Info, since it is an expected outcome,
// not a failure). Never returns an error, because no caller may change
// dispatch behaviour on its result.
//
// The passed issue may be stale (fetched earlier in the caller's request), so
// this re-reads the issue's current status before consulting
// MayPromoteToRunning - see promotableStatuses for why the guard exists and
// why it is shared with MarkIssueBlocked. Writes via UpdateIssueStatus (not
// the unassign variant): a running card keeps its assignee, since the chain
// wakes the next agent through it.
func (s *TaskService) MarkIssueRunning(ctx context.Context, issue db.Issue, agentName string) {
	current, err := s.Queries.GetIssue(ctx, issue.ID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("lifecycle: reload issue for run-start status", "err", err, "issue_id", util.UUIDToString(issue.ID))
		}
		return
	}
	if !MayPromoteToRunning(current.Status) {
		slog.Info("lifecycle: run-start status refused", "issue_id", util.UUIDToString(current.ID), "current_status", current.Status)
		return
	}
	prevStatus := current.Status
	newStatus := RunStartStatus(agentName)
	updated, err := s.Queries.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{
		ID:          current.ID,
		Status:      newStatus,
		WorkspaceID: current.WorkspaceID,
	})
	if err != nil {
		slog.Error("lifecycle: write run-start status", "err", err,
			"issue_id", util.UUIDToString(current.ID),
			"workspace_id", util.UUIDToString(current.WorkspaceID),
			"current_status", prevStatus,
			"new_status", newStatus,
			"agent_name", agentName)
		return
	}
	slog.Info("lifecycle: run-start status written",
		"issue_id", util.UUIDToString(current.ID),
		"prev_status", prevStatus,
		"new_status", newStatus,
		"agent_name", agentName)
	s.broadcastIssueUpdated(updated, prevStatus)
}

// MarkIssueBlocked parks a card whose task failed, so a human can investigate.
// Clears the assignee in the same statement as the status write. Best-effort,
// same rationale as MarkIssueRunning: every write failure is logged and
// swallowed, a guard refusal is logged too (at Info - for this writer it is
// often the routine, designed-for outcome, e.g. a task failing after its PR
// already merged (done) or after a human moved the card to in_review), and
// this never returns an error because no caller may change fail-task
// behaviour on its result. failureReason is also written into the posted
// comment - see blockedReasonCommentBody below.
//
// The passed issue may be stale, so this re-reads its current status before
// consulting MayPromoteToRunning - see promotableStatuses for why the guard
// exists and why it is shared with MarkIssueRunning (that shared guard is
// what stops a task failing after its PR already merged from dragging a done
// card back to blocked, which is exactly the scenario the runner used to
// handle with a PRAlreadyMerged file overlay). Writes via
// UpdateIssueStatusAndUnassign, not UpdateIssueStatus: a blocked card needs a
// human to pick it back up, so its stale assignee is cleared rather than left
// pointing at an agent that is done trying.
//
// Once the status write succeeds, this also posts one comment naming
// failureReason and how to resume the card. agentID authors that comment as
// the agent whose task failed (comment.author_id is NOT NULL, so a synthetic
// "system" author isn't an option here). The comment MUST be posted after
// the status write's assignee-clear has taken effect, never before:
// createAgentComment's every caller posts a comment that may mention nobody,
// and shouldEnqueueAgentTask/shouldEnqueueAssigneeFallback wake the card's
// assignee on exactly that shape - a still-assigned card getting a comment
// here would risk the runaway-dispatch bug fixed by clearing the assignee
// first (see UpdateIssueStatusAndUnassign above). Status and assignee move
// together in that one statement, so "after the write succeeds" already
// satisfies the ordering; the DB test for this asserts the ordering directly
// rather than trusting it.
//
// This comment is itself a createAgentComment call, so it also cancels any
// deferred escalation-fallback armed for (issueID, agentID) - see
// CancelDeferredEscalationsForIssueAgent, logged there with reason
// "agent_comment_acknowledged". On the common path (FailTask's errMsg != "")
// FailTask has already posted its own comment and cancelled the same set, so
// this is a no-op repeat. But when errMsg == "" that earlier comment is
// skipped, so this comment is the one that cancels the escalation - and it
// does so under a log line that says "acknowledged" for a task that actually
// died. That mislabelling is a pre-existing, undocumented-until-now side
// effect; it is not changed here.
func (s *TaskService) MarkIssueBlocked(ctx context.Context, issue db.Issue, agentID pgtype.UUID, failureReason string) {
	current, err := s.Queries.GetIssue(ctx, issue.ID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("lifecycle: reload issue for blocked status", "err", err, "issue_id", util.UUIDToString(issue.ID), "failure_reason", failureReason)
		}
		return
	}
	if !MayPromoteToRunning(current.Status) {
		slog.Info("lifecycle: blocked status refused", "issue_id", util.UUIDToString(current.ID), "current_status", current.Status, "failure_reason", failureReason)
		return
	}
	prevStatus := current.Status
	updated, err := s.Queries.UpdateIssueStatusAndUnassign(ctx, db.UpdateIssueStatusAndUnassignParams{
		ID:          current.ID,
		Status:      StatusBlocked,
		WorkspaceID: current.WorkspaceID,
	})
	if err != nil {
		slog.Error("lifecycle: write blocked status", "err", err,
			"issue_id", util.UUIDToString(current.ID),
			"workspace_id", util.UUIDToString(current.WorkspaceID),
			"current_status", prevStatus,
			"new_status", StatusBlocked,
			"failure_reason", failureReason)
		return
	}
	slog.Info("lifecycle: blocked status written",
		"issue_id", util.UUIDToString(current.ID),
		"prev_status", prevStatus,
		"new_status", StatusBlocked)
	s.broadcastIssueUpdated(updated, prevStatus)
	s.createAgentComment(ctx, current.ID, agentID, blockedReasonCommentBody(failureReason), "system", pgtype.UUID{}, pgtype.UUID{})
}

// MarkIssueNotRunning returns a card to todo when the run that put it at
// in_progress was abandoned rather than finished - e.g. an operator cancelled
// every task for an agent, deleted/reassigned a runtime, or revoked a
// workspace member, none of which is a task failure and none of which the
// operator experiences as a surprise. cause is log context only (e.g.
// "agent_tasks_cancelled", "runtime_deleted", "workspace_revoked") - it is
// never written to the issue or posted as a comment: unlike MarkIssueBlocked,
// this writer does not post anything, because the operator who cancelled the
// tasks or deleted the runtime already knows they did it - the failure
// path's comment exists because a failure is a surprise, an abandonment
// isn't.
//
// Writes via UpdateIssueStatus (not the unassign variant): unlike a failed
// run, an abandoned one has not told us anything about the assignee being
// unable to do the work, so the assignee is left in place. An archived
// agent's lingering assignment is harmless - isAgentAssigneeReady returns
// false once agent.archived_at is set, so the card cannot wake on it.
//
// Best-effort, same rationale as MarkIssueRunning/MarkIssueBlocked: every
// write failure is logged and swallowed, a guard refusal is logged too (at
// Info), and this never returns an error because no caller may change
// cancel/delete/revoke behaviour on its result.
//
// The guard here is deliberately NARROWER than MayPromoteToRunning, and that
// is intentional, not an oversight: this writer only undoes a write this
// system itself made (RunStartStatus promoting a card to in_progress on
// dispatch), so it must fire only when the card is still sitting exactly at
// in_progress. Reusing promotableStatuses (backlog/todo/blocked/in_progress)
// would let a runtime delete or agent-cancel drag an already-blocked card
// back to todo, erasing a real "a task on this card failed" signal for a
// reason that has nothing to do with that failure. If a future reader is
// tempted to unify this with promotableStatuses, don't: the two guards
// answer different questions ("may a run start/park here?" vs. "is this
// exactly the write we're allowed to undo?").
func (s *TaskService) MarkIssueNotRunning(ctx context.Context, issue db.Issue, cause string) {
	current, err := s.Queries.GetIssue(ctx, issue.ID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("lifecycle: reload issue for not-running status", "err", err, "issue_id", util.UUIDToString(issue.ID), "cause", cause)
		}
		return
	}
	if current.Status != StatusInProgress {
		slog.Info("lifecycle: not-running status refused", "issue_id", util.UUIDToString(current.ID), "current_status", current.Status, "cause", cause)
		return
	}
	prevStatus := current.Status
	updated, err := s.Queries.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{
		ID:          current.ID,
		Status:      StatusTodo,
		WorkspaceID: current.WorkspaceID,
	})
	if err != nil {
		slog.Error("lifecycle: write not-running status", "err", err,
			"issue_id", util.UUIDToString(current.ID),
			"workspace_id", util.UUIDToString(current.WorkspaceID),
			"current_status", prevStatus,
			"new_status", StatusTodo,
			"cause", cause)
		return
	}
	slog.Info("lifecycle: not-running status written",
		"issue_id", util.UUIDToString(current.ID),
		"prev_status", prevStatus,
		"new_status", StatusTodo,
		"cause", cause)
	s.broadcastIssueUpdated(updated, prevStatus)
}

// blockedCommentReasonReplacer neutralises characters in failureReason that
// would otherwise escape the markdown code span it is interpolated into.
// failure_reason is not constrained to the classifier's taxonomy at the API
// boundary (TaskFailRequest.FailureReason passes it through unchecked, and
// the daemon already sends at least one off-taxonomy literal,
// local_directory_error), so a backtick or newline reaching here is expected,
// not exceptional. mention:// is scrubbed for the reason described below;
// backticks and newlines are scrubbed so the reason can never close the code
// span early or break it across lines. The impact of not scrubbing them
// would be cosmetic (malformed markdown, not a smuggled mention), but the
// fix is cheap enough that there is no reason to leave it open.
var blockedCommentReasonReplacer = strings.NewReplacer(
	"mention://", "mention-blocked://",
	"`", "'",
	"\n", " ",
	"\r", " ",
)

// blockedReasonCommentBody renders the comment MarkIssueBlocked posts after
// parking a card. It always starts with "Classified as" - never with
// failureReason itself - so an attacker-controlled or future reason string
// that happened to start with "/" can never be parsed as a pipeline control
// verb (pipeline comments are member-authored; a leading "/" is a command).
// It never embeds a mention:// link, describing the @-mention mechanism in
// prose instead, so this comment can never itself dispatch an agent onto the
// card it just blocked - and failureReason is defused before interpolation
// (see blockedCommentReasonReplacer) so a reason string that itself contains
// the literal substring "mention://", a backtick, or a newline can't smuggle
// one in or break the code span. It deliberately omits the raw error text:
// FailTask already posts that in a separate system comment on the same
// condition, and this one is meant to carry only what that one doesn't - the
// classified reason and how to resume. The resume line says only what
// actually happens: an @-mention starts a new run and the card returns to
// in_progress. It does not promise reassignment, because nothing in this
// backend reassigns the card - computeCommentAgentTriggers creates a task
// from the @-mention without touching issue.assignee_*, so a resumed card
// runs at in_progress with a NULL assignee.
func blockedReasonCommentBody(failureReason string) string {
	safeReason := blockedCommentReasonReplacer.Replace(failureReason)
	return fmt.Sprintf(
		"Classified as `%s`.\n\n@-mention an agent on this card to resume - it starts a new run and the card returns to in progress.",
		safeReason,
	)
}
