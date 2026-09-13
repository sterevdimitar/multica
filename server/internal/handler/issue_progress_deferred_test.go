package handler

import (
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A deferred row is a retry that has not fired yet (the 5/10-minute
// GitHub-unreachable schedule). It is a run that is still coming, so the
// popover must draw it ⏸ like a queued step, and every consumer of "is
// anything still coming on this card" must count it (dev-command-center
// design 2026-09-13-github-unreachable-retry, invariant 3). The web maps
// is_live && status != running to the ⏸ glyph, so is_live is the whole
// contract here.
func TestProgressTaskFromRow_DeferredIsLive(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{"deferred", true},
		{"queued", true},
		{"dispatched", true},
		{"running", true},
		{"completed", false},
		{"failed", false},
		{"cancelled", false},
	}
	for _, tc := range cases {
		task := progressTaskFromRow(db.ListTaskProgressByIssueRow{AgentName: "fixer", Status: tc.status})
		if task.IsLive != tc.want {
			t.Errorf("status %q: is_live = %v, want %v", tc.status, task.IsLive, tc.want)
		}
	}
}
