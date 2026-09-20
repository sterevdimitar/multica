package service

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func uuidN(n byte) pgtype.UUID {
	var u pgtype.UUID
	u.Bytes[15] = n
	u.Valid = true
	return u
}

func rt(n byte, name string) db.AgentRuntime {
	return db.AgentRuntime{
		ID:          uuidN(n),
		Name:        name,
		RuntimeMode: "webhook",
		WebhookUrl:  pgtype.Text{String: "http://t/" + name, Valid: true},
	}
}

// choosePlacement is design §3 as a table: the home wins when it is up and
// free; full is a wait, never a move (P2); down goes to the first runtime in
// order that is in the fallback set, available and free (P1).
func TestChoosePlacement(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	down := func(r db.AgentRuntime) db.AgentRuntime {
		r.DownUntil = at(now.Add(time.Minute))
		r.DownReason = pgtype.Text{String: "no online runner", Valid: true}
		return r
	}
	capped := func(r db.AgentRuntime, n int32) db.AgentRuntime { r.MaxConcurrentTasks = capOf(n); return r }
	home := rt(1, "Local PC")
	ci := rt(2, "CircleCI")
	laptop := rt(3, "dpm-laptop")
	gha := rt(4, "GitHub Actions")
	all := []pgtype.UUID{home.ID, ci.ID, laptop.ID, gha.ID}

	cases := map[string]struct {
		home     db.AgentRuntime
		ordered  []db.AgentRuntime
		fallback []pgtype.UUID
		running  map[byte]int64
		want     string // "" = wait
		offHome  bool
	}{
		"home up and free":           {home, []db.AgentRuntime{ci, home}, all, nil, "Local PC", false},
		"home up, no cap, many runs": {home, []db.AgentRuntime{ci, home}, all, map[byte]int64{1: 99}, "Local PC", false},
		"home full is a wait (P2)":   {capped(home, 2), []db.AgentRuntime{ci, home}, all, map[byte]int64{1: 2}, "", false},
		"home down, first in order":  {down(home), []db.AgentRuntime{ci, laptop, home}, all, nil, "CircleCI", true},
		"home down, first full":      {down(home), []db.AgentRuntime{capped(ci, 1), laptop}, all, map[byte]int64{2: 1}, "dpm-laptop", true},
		"home down, order respected": {down(home), []db.AgentRuntime{laptop, ci}, all, nil, "dpm-laptop", true},
		"home down, empty set waits": {down(home), []db.AgentRuntime{ci, laptop}, nil, nil, "", false},
		"home down, not in set":      {down(home), []db.AgentRuntime{ci, laptop}, []pgtype.UUID{laptop.ID}, nil, "dpm-laptop", true},
		"home cap 0 is down":         {capped(home, 0), []db.AgentRuntime{home, ci}, all, nil, "CircleCI", true},
		"fallback cap 0 is skipped":  {down(home), []db.AgentRuntime{capped(ci, 0), laptop}, all, nil, "dpm-laptop", true},
		"fallback in cool-down":      {down(home), []db.AgentRuntime{down(ci), laptop}, all, nil, "dpm-laptop", true},
		"every fallback down":        {down(home), []db.AgentRuntime{down(ci), down(laptop)}, all, nil, "", false},
		"every fallback full":        {down(home), []db.AgentRuntime{capped(ci, 1), capped(laptop, 1)}, all, map[byte]int64{2: 1, 3: 1}, "", false},
		"fallback without a URL":     {down(home), []db.AgentRuntime{{ID: uuidN(2), Name: "x", RuntimeMode: "webhook"}, laptop}, all, nil, "dpm-laptop", true},
		"home in order but down":     {down(home), []db.AgentRuntime{home, ci}, all, nil, "CircleCI", true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			running := func(id pgtype.UUID) int64 { return tc.running[id.Bytes[15]] }
			got, offHome, ok := choosePlacement(tc.home, tc.ordered, tc.fallback, running, now)
			if tc.want == "" {
				if ok {
					t.Fatalf("expected a wait, got %q", got.Name)
				}
				return
			}
			if !ok {
				t.Fatalf("expected %q, got a wait", tc.want)
			}
			if got.Name != tc.want {
				t.Errorf("placed on %q, want %q", got.Name, tc.want)
			}
			if offHome != tc.offHome {
				t.Errorf("offHome = %v, want %v", offHome, tc.offHome)
			}
		})
	}
}

func TestFailoverNote(t *testing.T) {
	home := rt(1, "Local PC")
	home.DownReason = pgtype.Text{String: `no online runner carries "local-pc"`, Valid: true}
	ci := rt(2, "CircleCI")
	ci.CustomName = pgtype.Text{String: "Circle", Valid: true}
	if got, want := failoverNote(home, ci), `↪ Local PC is down (no online runner carries "local-pc") — running on Circle`; got != want {
		t.Errorf("note = %q\nwant %q", got, want)
	}
	home.MaxConcurrentTasks = capOf(0)
	if got := failoverNote(home, ci); got != "↪ Local PC is down (out of the rotation) — running on Circle" {
		t.Errorf("note = %q", got)
	}
}

func TestSerialisationKey(t *testing.T) {
	agent := uuidN(9)
	issue := db.AgentTaskQueue{AgentID: agent, IssueID: uuidN(1)}
	chat := db.AgentTaskQueue{AgentID: agent, ChatSessionID: uuidN(2)}
	bare := db.AgentTaskQueue{AgentID: agent}
	if serialisationKey(issue) == serialisationKey(chat) || serialisationKey(issue) == serialisationKey(bare) {
		t.Error("keys must distinguish issue, chat and bare tasks")
	}
	if serialisationKey(issue) != serialisationKey(db.AgentTaskQueue{AgentID: agent, IssueID: uuidN(1)}) {
		t.Error("two tasks on one (agent, issue) must share a key")
	}
	if util.UUIDToString(agent) == "" {
		t.Fatal("fixture uuid")
	}
}
