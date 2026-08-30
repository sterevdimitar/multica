package service

import "testing"

func TestRunStartStatusDefaultsToInProgress(t *testing.T) {
	agents := []string{"review-agent", "fix-agent", "readiness-agent", ""}

	for _, agent := range agents {
		t.Run(agent, func(t *testing.T) {
			got := RunStartStatus(agent)
			if got != "in_progress" {
				t.Errorf("RunStartStatus(%q) = %q, want %q", agent, got, "in_progress")
			}
		})
	}
}

func TestRunStartStatusOnlyReturnsSchemaStatuses(t *testing.T) {
	// Agent names sourced from pipeline/agents.yaml in the dev-command-center
	// repo (not readable from this fork).
	agents := []string{
		"review-agent",
		"review-validator-agent",
		"fixer",
		"readiness-agent",
	}

	// The exact CHECK constraint set from server/migrations/001_init.up.sql:58.
	schemaStatuses := map[string]bool{
		"backlog":     true,
		"todo":        true,
		"in_progress": true,
		"in_review":   true,
		"done":        true,
		"blocked":     true,
		"cancelled":   true,
	}

	for _, agent := range agents {
		t.Run(agent, func(t *testing.T) {
			got := RunStartStatus(agent)
			if !schemaStatuses[got] {
				t.Errorf("RunStartStatus(%q) = %q, not a valid schema status", agent, got)
			}
		})
	}

	// The agent list above can only cover agents that exist today. The table
	// itself is the thing that drifts: adding a per-phase status like
	// "reviewing" to runStartStatusByAgent without first migrating the CHECK
	// constraint would make every dispatch for that agent fail its status
	// write. Iterating the table catches that whatever the agent is called.
	for agent, status := range runStartStatusByAgent {
		t.Run("table/"+agent, func(t *testing.T) {
			if !schemaStatuses[status] {
				t.Errorf("runStartStatusByAgent[%q] = %q, not a valid schema status; "+
					"a per-phase status needs a CHECK-constraint migration first", agent, status)
			}
		})
	}
}

func TestMayPromoteToRunning(t *testing.T) {
	allowed := []string{"backlog", "todo", "blocked", "in_progress"}
	for _, status := range allowed {
		t.Run(status, func(t *testing.T) {
			if !MayPromoteToRunning(status) {
				t.Errorf("MayPromoteToRunning(%q) = false, want true", status)
			}
		})
	}

	disallowed := []string{"in_review", "done", "cancelled", "", "nonsense"}
	for _, status := range disallowed {
		t.Run(status, func(t *testing.T) {
			if MayPromoteToRunning(status) {
				t.Errorf("MayPromoteToRunning(%q) = true, want false", status)
			}
		})
	}
}
