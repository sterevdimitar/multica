package service

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func webhookRuntime() db.AgentRuntime {
	return db.AgentRuntime{RuntimeMode: "webhook"}
}

func capOf(n int32) pgtype.Int4 { return pgtype.Int4{Int32: n, Valid: true} }

func at(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// available(r) from the design §2: webhook mode, cap not 0, not in cool-down.
func TestRuntimeAvailable(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	cases := map[string]struct {
		mutate func(*db.AgentRuntime)
		want   bool
	}{
		"webhook, no cap, never down": {func(*db.AgentRuntime) {}, true},
		"daemon mode":                 {func(r *db.AgentRuntime) { r.RuntimeMode = "local" }, false},
		"cap 3":                       {func(r *db.AgentRuntime) { r.MaxConcurrentTasks = capOf(3) }, true},
		"cap 0 is out of rotation":    {func(r *db.AgentRuntime) { r.MaxConcurrentTasks = capOf(0) }, false},
		"down_until in the future":    {func(r *db.AgentRuntime) { r.DownUntil = at(now.Add(time.Minute)) }, false},
		"down_until in the past":      {func(r *db.AgentRuntime) { r.DownUntil = at(now.Add(-time.Second)) }, true},
		"down_until exactly now":      {func(r *db.AgentRuntime) { r.DownUntil = at(now) }, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := webhookRuntime()
			tc.mutate(&r)
			if got := runtimeAvailable(r, now); got != tc.want {
				t.Fatalf("runtimeAvailable = %v, want %v", got, tc.want)
			}
		})
	}
}

// free(r): no cap, or running below it.
func TestRuntimeFree(t *testing.T) {
	cases := map[string]struct {
		cap     pgtype.Int4
		running int64
		want    bool
	}{
		"no cap, many running": {pgtype.Int4{}, 100, true},
		"cap 3, 2 running":     {capOf(3), 2, true},
		"cap 3, 3 running":     {capOf(3), 3, false},
		"cap 3, 4 running":     {capOf(3), 4, false},
		"cap 0, none running":  {capOf(0), 0, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := webhookRuntime()
			r.MaxConcurrentTasks = tc.cap
			if got := runtimeFree(r, tc.running); got != tc.want {
				t.Fatalf("runtimeFree = %v, want %v", got, tc.want)
			}
		})
	}
}
