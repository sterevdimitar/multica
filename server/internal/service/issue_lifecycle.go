package service

// Issue status constants, mirroring the CHECK constraint on issues.status
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

// mayPromoteToRunning is the allow-list of statuses a starting run may move a
// card out of. Terminal statuses (done, cancelled) and the human-decision
// status (in_review) are deliberately excluded: a dispatch arriving while one
// of those holds must never overwrite it. Any status not in this list -
// including one added by a future migration - is refused by default.
var mayPromoteToRunning = map[string]bool{
	StatusBacklog:    true,
	StatusTodo:       true,
	StatusBlocked:    true,
	StatusInProgress: true,
}

// MayPromoteToRunning reports whether a card at the given status may be moved
// by a starting run. True for backlog, todo, blocked, in_progress; false for
// in_review, done, cancelled and any unrecognised value.
func MayPromoteToRunning(currentStatus string) bool {
	return mayPromoteToRunning[currentStatus]
}
