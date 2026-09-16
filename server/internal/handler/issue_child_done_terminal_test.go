package handler

import "testing"

// isTerminalChildStatus decides which child transitions count as "this stage
// is finished" for the parent-wake barrier. archived joins done/cancelled:
// a card put away is finished work, and a parent must not wait on it.
func TestIsTerminalChildStatus(t *testing.T) {
	terminal := []string{"done", "cancelled", "archived"}
	for _, s := range terminal {
		if !isTerminalChildStatus(s) {
			t.Errorf("isTerminalChildStatus(%q) = false, want true", s)
		}
	}
	open := []string{"backlog", "todo", "in_progress", "in_review", "blocked", "", "nonsense"}
	for _, s := range open {
		if isTerminalChildStatus(s) {
			t.Errorf("isTerminalChildStatus(%q) = true, want false", s)
		}
	}
}
