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
		name       string
		tasks      []pushRestartTask
		exempt     map[string]bool
		wantCancel []string
	}{
		{"reviewer running",
			[]pushRestartTask{prTask("review-agent", "running")}, fixerOnly,
			[]string{"review-agent:running"}},
		{"validator running",
			[]pushRestartTask{prTask("review-agent", "completed"), prTask("review-validator-agent", "running")}, fixerOnly,
			[]string{"review-validator-agent:running"}},
		{"fixer running (its own push)",
			[]pushRestartTask{prTask("fixer", "running")}, fixerOnly,
			[]string{}},
		{"fixer running + queued reviewer",
			[]pushRestartTask{prTask("fixer", "running"), prTask("review-agent", "queued")}, fixerOnly,
			[]string{"review-agent:queued"}},
		{"fixer queued is not exempt",
			[]pushRestartTask{prTask("fixer", "queued")}, fixerOnly,
			[]string{"fixer:queued"}},
		{"fixer dispatched is exempt",
			[]pushRestartTask{prTask("fixer", "dispatched")}, fixerOnly,
			[]string{}},
		{"judge running",
			[]pushRestartTask{prTask("readiness-agent", "running")}, fixerOnly,
			[]string{"readiness-agent:running"}},
		{"nothing active",
			[]pushRestartTask{prTask("review-agent", "completed"), prTask("fixer", "failed"), prTask("fixer", "cancelled")}, fixerOnly,
			[]string{}},
		// A parked or blocked card is not a special case any more: the plan
		// does not know or care whether the card has an assignee
		// (push-to-parked-card design §3.1). The same tasks get the same
		// answer; the executor writes the assignee back.
		{"parked card with a stale reviewer: cancelled like any other",
			[]pushRestartTask{prTask("review-agent", "running")}, fixerOnly,
			[]string{"review-agent:running"}},
		{"parked card with the fixer running through the park (card 221): fixer kept",
			[]pushRestartTask{prTask("fixer", "running")}, fixerOnly,
			[]string{}},
		{"empty exempt set cancels a running fixer",
			[]pushRestartTask{prTask("fixer", "running")}, map[string]bool{},
			[]string{"fixer:running"}},
		{"waiting_local_directory counts as active",
			[]pushRestartTask{prTask("review-agent", "waiting_local_directory")}, fixerOnly,
			[]string{"review-agent:waiting_local_directory"}},
		{"a deferred fixer retry is for the old head: cancelled",
			[]pushRestartTask{prTask("fixer", "deferred")}, fixerOnly,
			[]string{"fixer:deferred"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cancel := planPushRestart(tc.tasks, tc.exempt)
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

	got := pushRestartComment(sha, "sterevdimitar", two, "in_progress", false)
	want := "↻ Head moved to `d5a9163` (pushed by sterevdimitar). Cancelled: review-agent (running), fixer (queued). Review restarted from the top."
	if got != want {
		t.Fatalf("assigned, two cancelled:\n got %q\nwant %q", got, want)
	}

	got = pushRestartComment(sha, "sterevdimitar", nil, "in_progress", false)
	want = "↻ Head moved to `d5a9163` (pushed by sterevdimitar). Review restarted from the top."
	if got != want {
		t.Fatalf("assigned, none cancelled:\n got %q\nwant %q", got, want)
	}

	// The card had no assignee: the comment says which state it sat in and
	// that the push lifted it (push-to-parked-card design §3.3).
	got = pushRestartComment(sha, "sterevdimitar", nil, "in_review", true)
	want = "↻ Head moved to `d5a9163` (pushed by sterevdimitar) while this card sat at `in_review` with no assignee. Park lifted. Review restarted from the top."
	if got != want {
		t.Fatalf("parked, none cancelled:\n got %q\nwant %q", got, want)
	}

	got = pushRestartComment(sha, "sterevdimitar", []pushRestartTask{prTask("review-agent", "queued")}, "blocked", true)
	want = "↻ Head moved to `d5a9163` (pushed by sterevdimitar) while this card sat at `blocked` with no assignee. Park lifted. Cancelled: review-agent (queued). Review restarted from the top."
	if got != want {
		t.Fatalf("blocked, one cancelled:\n got %q\nwant %q", got, want)
	}

	// An assigned card never gets the clause, whatever its status.
	if got := pushRestartComment(sha, "sterevdimitar", nil, "in_review", false); strings.Contains(got, "no assignee") {
		t.Fatalf("assigned card must not claim it was parked: %q", got)
	}

	got = pushRestartComment("", "", two, "in_progress", false)
	if !strings.Contains(got, "`unknown`") || !strings.Contains(got, "an unknown sender") {
		t.Fatalf("empty sha/sender: got %q", got)
	}

	for _, out := range []string{
		pushRestartComment(sha, "sterevdimitar", two, "in_progress", false),
		pushRestartComment(sha, "sterevdimitar", nil, "in_review", true),
		pushRestartComment("", "", nil, "", true),
		pushRestartComment(sha, "mention://agent/x", two, "mention://agent/x", true),
	} {
		if strings.Contains(out, "mention://") {
			t.Fatalf("P2: a restart comment must never carry a mention: %q", out)
		}
		if strings.HasPrefix(out, "/") {
			t.Fatalf("a pipeline comment must never begin with a slash: %q", out)
		}
	}
}
