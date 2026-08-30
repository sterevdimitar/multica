package service

import (
	"context"
	"log/slog"

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
// StatusInProgress is in the list on purpose, and must stay. A card can be
// dispatched to while already running - a second run against the same card, or
// a retry - and that has to be a no-op rather than a refusal. Removing this
// entry would not break any caller loudly; it would just make the promotion
// path return early for every card after the first dispatch.
var promotableStatuses = map[string]bool{
	StatusBacklog:    true,
	StatusTodo:       true,
	StatusBlocked:    true,
	StatusInProgress: true,
}

// MayPromoteToRunning reports whether a card at the given status may be moved
// by a starting run. True for backlog, todo, blocked, in_progress; false for
// in_review, done, cancelled and any unrecognised value.
func MayPromoteToRunning(currentStatus string) bool {
	return promotableStatuses[currentStatus]
}

// MarkIssueRunning writes the starting status for a dispatched task's issue.
// Best-effort: every failure is logged and swallowed. Never returns an error,
// because no caller may change dispatch behaviour on its result.
//
// The passed issue may be stale (fetched earlier in the caller's request), so
// this re-reads the issue's current status before consulting
// MayPromoteToRunning — the guard protects a merge trigger (done) and a
// pending human decision (in_review), and trusting a stale struct could let a
// dispatch clobber either. Writes via UpdateIssueStatus (not the unassign
// variant): a running card keeps its assignee, since the chain wakes the next
// agent through it.
func (s *TaskService) MarkIssueRunning(ctx context.Context, issue db.Issue, agentName string) {
	current, err := s.Queries.GetIssue(ctx, issue.ID)
	if err != nil {
		slog.Error("lifecycle: reload issue for run-start status", "err", err, "issue_id", util.UUIDToString(issue.ID))
		return
	}
	if !MayPromoteToRunning(current.Status) {
		return
	}
	prevStatus := current.Status
	updated, err := s.Queries.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{
		ID:          current.ID,
		Status:      RunStartStatus(agentName),
		WorkspaceID: current.WorkspaceID,
	})
	if err != nil {
		slog.Error("lifecycle: write run-start status", "err", err, "issue_id", util.UUIDToString(issue.ID))
		return
	}
	s.broadcastIssueUpdated(updated, prevStatus)
}
