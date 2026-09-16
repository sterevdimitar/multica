package handler

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// dispatched_at is the left end of the popover's wall-time span: the instant
// the fork fired the webhook. It must reach the wire as a timestamp when set
// and as null when not — never the zero instant, which would make a
// never-dispatched task look like it waited since 0001.
func TestProgressTaskFromRow_DispatchedAt(t *testing.T) {
	at := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		ts   pgtype.Timestamptz
		want string
	}{
		{"dispatched", pgtype.Timestamptz{Time: at, Valid: true}, `"dispatched_at":"2026-09-16T10:00:00Z"`},
		{"never dispatched", pgtype.Timestamptz{}, `"dispatched_at":null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := progressTaskFromRow(db.ListTaskProgressByIssueRow{
				AgentName:    "review-agent",
				Status:       "completed",
				DispatchedAt: tc.ts,
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
