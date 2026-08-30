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
// given agent. Every agent maps to StatusInProgress today; the table is the
// extension point for per-phase statuses (reviewing / fixing / judging),
// which additionally require a migration to the status CHECK constraint.
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
// or a failed run parking it as blocked. True for backlog, todo, blocked,
// in_progress; false for in_review, done, cancelled and any unrecognised
// value. See promotableStatuses for why the list is shared.
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
	s.broadcastIssueUpdated(updated, prevStatus)
}

// MarkIssueBlocked parks a card whose task failed, so a human can investigate.
// Clears the assignee in the same statement as the status write. Best-effort,
// same rationale as MarkIssueRunning: every write failure is logged and
// swallowed, a guard refusal is logged too (at Info - for this writer it is
// often the routine, designed-for outcome, e.g. a task failing after its PR
// already merged (done) or after a human moved the card to in_review), and
// this never returns an error because no caller may change fail-task
// behaviour on its result. failureReason is log context only; it is never
// written to the card.
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
// and shouldEnqueueOnComment/isAgentAssigneeReady wakes the card's assignee
// on exactly that shape - a still-assigned card getting a comment here would
// risk the runaway-dispatch bug fixed by clearing the assignee first (see
// UpdateIssueStatusAndUnassign above). Status and assignee move together in
// that one statement, so "after the write succeeds" already satisfies the
// ordering; the DB test for this asserts the ordering directly rather than
// trusting it.
func (s *TaskService) MarkIssueBlocked(ctx context.Context, issue db.Issue, agentID pgtype.UUID, failureReason string) {
	current, err := s.Queries.GetIssue(ctx, issue.ID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("lifecycle: reload issue for blocked status", "err", err, "issue_id", util.UUIDToString(issue.ID), "failure_reason", failureReason)
		}
		return
	}
	if !MayPromoteToRunning(current.Status) {
		slog.Info("lifecycle: blocked status refused", "issue_id", util.UUIDToString(current.ID), "current_status", current.Status)
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
	s.broadcastIssueUpdated(updated, prevStatus)
	s.createAgentComment(ctx, current.ID, agentID, blockedReasonCommentBody(failureReason), "system", pgtype.UUID{}, pgtype.UUID{})
}

// blockedReasonCommentBody renders the comment MarkIssueBlocked posts after
// parking a card. It always starts with "Classified as" - never with
// failureReason itself - so an attacker-controlled or future reason string
// that happened to start with "/" can never be parsed as a pipeline control
// verb (pipeline comments are member-authored; a leading "/" is a command).
// It never embeds a mention:// link, describing the @-mention mechanism in
// prose instead, so this comment can never itself dispatch an agent onto the
// card it just blocked - and failureReason is defused before interpolation
// so a reason string that itself contains the literal substring "mention://"
// (classifier bug, future free-text reason, etc.) can't smuggle one in
// either. It deliberately omits the raw error text: FailTask already posts
// that in a separate system comment on the same condition, and this one is
// meant to carry only what that one doesn't - the classified reason and how
// to resume.
func blockedReasonCommentBody(failureReason string) string {
	safeReason := strings.ReplaceAll(failureReason, "mention://", "mention-blocked://")
	return fmt.Sprintf(
		"Classified as `%s`.\n\n@-mention an agent on this card to resume - the run reassigns and the card returns to in-progress.",
		safeReason,
	)
}
