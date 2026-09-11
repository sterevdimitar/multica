package handler

import (
	"encoding/json"
	"strings"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The SQL returns -1 for "no priced usage" because sqlc infers every cast
// as NOT NULL and a real NULL would fail to scan. The wire must never show
// that sentinel, and must never show 0 for it either: null is the only
// honest value for "we do not know", and the popover renders it as a dash.
func TestProgressTaskFromRow_Cost(t *testing.T) {
	cases := []struct {
		name string
		sum  float64
		want string // the JSON fragment for cost_usd
	}{
		{"priced step", 0.157, `"cost_usd":0.157`},
		{"no priced usage (sentinel)", -1, `"cost_usd":null`},
		{"genuinely free is still a number", 0, `"cost_usd":0`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := progressTaskFromRow(db.ListTaskProgressByIssueRow{
				AgentName:    "conflict-agent",
				Status:       "completed",
				TotalCostUsd: tc.sum,
			})
			raw, err := json.Marshal(task)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), tc.want) {
				t.Errorf("json = %s, want it to contain %s", raw, tc.want)
			}
		})
	}
}

// The dashboard aggregates use the same sentinel and the same mapping.
func TestCostPtr(t *testing.T) {
	if got := costPtr(-1); got != nil {
		t.Errorf("costPtr(-1) = %v, want nil", *got)
	}
	if got := costPtr(0.157); got == nil || *got != 0.157 {
		t.Errorf("costPtr(0.157) = %v, want 0.157", got)
	}
	if got := costPtr(0); got == nil || *got != 0 {
		t.Errorf("costPtr(0) = %v, want 0 (a real zero is a number, not unknown)", got)
	}
}
