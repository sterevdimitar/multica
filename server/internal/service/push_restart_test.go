package service

import (
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// The planner is the whole decision of the push-restart design
// (dev-command-center docs/superpowers/specs/2026-09-13-outside-push-restarts-the-chain-design.md
// §2.1); its table here IS the spec's table, one row per line. The executor
// (push_restart_db_test.go) only checks that the plan is carried out.

func TestPushExemptAgents(t *testing.T) {
	t.Run("unset defaults to the pipeline's fixer", func(t *testing.T) {
		t.Setenv("MULTICA_PUSH_EXEMPT_AGENTS", "placeholder")
		os.Unsetenv("MULTICA_PUSH_EXEMPT_AGENTS")
		if got := pushExemptAgents(); !reflect.DeepEqual(got, map[string]bool{"fixer": true}) {
			t.Fatalf("unset: got %v, want {fixer}", got)
		}
	})
	t.Run("empty string exempts nothing", func(t *testing.T) {
		t.Setenv("MULTICA_PUSH_EXEMPT_AGENTS", "")
		if got := pushExemptAgents(); len(got) != 0 {
			t.Fatalf("empty: got %v, want {}", got)
		}
	})
	t.Run("comma list is trimmed and empties dropped", func(t *testing.T) {
		t.Setenv("MULTICA_PUSH_EXEMPT_AGENTS", "fixer, conflict-agent ,")
		want := map[string]bool{"fixer": true, "conflict-agent": true}
		if got := pushExemptAgents(); !reflect.DeepEqual(got, want) {
			t.Fatalf("list: got %v, want %v", got, want)
		}
	})
}

func prTask(name, status string) pushRestartTask {
	var id pgtype.UUID
	copy(id.Bytes[:], []byte(name + "/" + status + "................")[:16])
	id.Valid = true
	return pushRestartTask{ID: id, AgentName: name, Status: status}
}

func names(ts []pushRestartTask) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.AgentName+":"+t.Status)
	}
	sort.Strings(out)
	return out
}

func TestPlanPushRestart(t *testing.T) {
	fixerOnly := map[string]bool{"fixer": true}
	cases := []struct {
		name        string
		assigned    bool
		tasks       []pushRestartTask
		exempt      map[string]bool
		wantCancel  []string
		wantRestart bool
	}{
		{"reviewer running", true,
			[]pushRestartTask{prTask("review-agent", "running")}, fixerOnly,
			[]string{"review-agent:running"}, true},
		{"validator running", true,
			[]pushRestartTask{prTask("review-agent", "completed"), prTask("review-validator-agent", "running")}, fixerOnly,
			[]string{"review-validator-agent:running"}, true},
		{"fixer running (its own push)", true,
			[]pushRestartTask{prTask("fixer", "running")}, fixerOnly,
			[]string{}, true},
		{"fixer running + queued reviewer", true,
			[]pushRestartTask{prTask("fixer", "running"), prTask("review-agent", "queued")}, fixerOnly,
			[]string{"review-agent:queued"}, true},
		{"fixer queued is not exempt", true,
			[]pushRestartTask{prTask("fixer", "queued")}, fixerOnly,
			[]string{"fixer:queued"}, true},
		{"fixer dispatched is exempt", true,
			[]pushRestartTask{prTask("fixer", "dispatched")}, fixerOnly,
			[]string{}, true},
		{"judge running", true,
			[]pushRestartTask{prTask("readiness-agent", "running")}, fixerOnly,
			[]string{"readiness-agent:running"}, true},
		{"nothing active", true,
			[]pushRestartTask{prTask("review-agent", "completed"), prTask("fixer", "failed"), prTask("fixer", "cancelled")}, fixerOnly,
			[]string{}, true},
		{"no assignee: untouched", false,
			[]pushRestartTask{prTask("review-agent", "running")}, fixerOnly,
			[]string{}, false},
		{"empty exempt set cancels a running fixer", true,
			[]pushRestartTask{prTask("fixer", "running")}, map[string]bool{},
			[]string{"fixer:running"}, true},
		{"waiting_local_directory counts as active", true,
			[]pushRestartTask{prTask("review-agent", "waiting_local_directory")}, fixerOnly,
			[]string{"review-agent:waiting_local_directory"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cancel, restart := planPushRestart(tc.assigned, tc.tasks, tc.exempt)
			if restart != tc.wantRestart {
				t.Fatalf("restart = %v, want %v", restart, tc.wantRestart)
			}
			if got := names(cancel); !reflect.DeepEqual(got, tc.wantCancel) {
				t.Fatalf("cancel = %v, want %v", got, tc.wantCancel)
			}
		})
	}
}

const testPushPayload = `{"action":"synchronize","number":108,
  "pull_request":{"html_url":"https://github.com/sterevdimitar/dev-command-center/pull/108",
    "title":"release: the tag exists before the release PR does",
    "user":{"login":"sterevdimitar"},
    "head":{"ref":"claude/release-tag-before-pr","sha":"d5a9163c77f31dc7cc601b1553e19f4cb57c89c5"},
    "base":{"ref":"main"}},
  "sender":{"login":"sterevdimitar"},
  "repository":{"full_name":"sterevdimitar/dev-command-center"}}`

func TestPullRequestPush(t *testing.T) {
	sha, sender := pullRequestPush(envelopeRun(t, "github.pull_request", testPushPayload))
	if sha != "d5a9163c77f31dc7cc601b1553e19f4cb57c89c5" || sender != "sterevdimitar" {
		t.Fatalf("got (%q, %q)", sha, sender)
	}
	sha, sender = pullRequestPush(envelopeRun(t, "github.pull_request", testPRPayload))
	if sha != "" || sender != "" {
		t.Fatalf("payload without sha/sender: got (%q, %q), want empty", sha, sender)
	}
	sha, sender = pullRequestPush(envelopeRun(t, "github.pull_request", `{"pull_request":`))
	if sha != "" || sender != "" {
		t.Fatalf("malformed: got (%q, %q), want empty", sha, sender)
	}
	sha, sender = pullRequestPush(envelopeRun(t, "github.push", `{"after":"abc"}`))
	if sha != "" || sender != "" {
		t.Fatalf("non-PR event: got (%q, %q), want empty", sha, sender)
	}
}

func TestPushRestartComment(t *testing.T) {
	sha := "d5a9163c77f31dc7cc601b1553e19f4cb57c89c5"
	two := []pushRestartTask{prTask("review-agent", "running"), prTask("fixer", "queued")}

	got := pushRestartComment(sha, "sterevdimitar", two)
	want := "↻ Head moved to `d5a9163` (pushed by sterevdimitar). Cancelled: review-agent (running), fixer (queued). Review restarted from the top."
	if got != want {
		t.Fatalf("two cancelled:\n got %q\nwant %q", got, want)
	}

	got = pushRestartComment(sha, "sterevdimitar", nil)
	want = "↻ Head moved to `d5a9163` (pushed by sterevdimitar). Review restarted from the top."
	if got != want {
		t.Fatalf("none cancelled:\n got %q\nwant %q", got, want)
	}

	got = pushRestartComment("", "", two)
	if !strings.Contains(got, "`unknown`") || !strings.Contains(got, "an unknown sender") {
		t.Fatalf("empty sha/sender: got %q", got)
	}

	for _, out := range []string{
		pushRestartComment(sha, "sterevdimitar", two),
		pushRestartComment(sha, "sterevdimitar", nil),
		pushRestartComment("", "", nil),
		pushRestartComment(sha, "mention://agent/x", two),
	} {
		if strings.Contains(out, "mention://") {
			t.Fatalf("I2: a restart comment must never carry a mention: %q", out)
		}
	}
}
