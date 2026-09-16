package handler

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// job_started_at is the left end of the popover's wall-time span: the
// instant the runner's job began executing. It must reach the wire as a
// timestamp when set and as null when not — never the zero instant, which
// would make a task that never reached a runner look like it ran since 0001.
func TestProgressTaskFromRow_JobStartedAt(t *testing.T) {
	at := time.Date(2026, 9, 16, 10, 23, 48, 0, time.UTC)
	cases := []struct {
		name string
		ts   pgtype.Timestamptz
		want string
	}{
		{"reached a runner", pgtype.Timestamptz{Time: at, Valid: true}, `"job_started_at":"2026-09-16T10:23:48Z"`},
		{"never reached a runner", pgtype.Timestamptz{}, `"job_started_at":null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := progressTaskFromRow(db.ListTaskProgressByIssueRow{
				AgentName:    "review-agent",
				Status:       "completed",
				JobStartedAt: tc.ts,
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

// The wire value is a duration the runner measured on its own clock. Only a
// non-negative one is a measurement: a negative startup would derive a
// job_started_at AFTER started_at, the inversion the design rules out, so
// it is dropped rather than clamped to 0 (0 would claim "no startup at all").
func TestStartupMsFromRequest(t *testing.T) {
	ms := func(n int64) *int64 { return &n }
	cases := []struct {
		name string
		in   *int64
		want *int64
	}{
		{"absent", nil, nil},
		{"measured", ms(16_000), ms(16_000)},
		{"zero is a measurement", ms(0), ms(0)},
		{"negative is not", ms(-1), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := startupMsFromRequest(TaskStartRequest{StartupMs: tc.in})
			switch {
			case got == nil && tc.want == nil:
			case got == nil || tc.want == nil || *got != *tc.want:
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
