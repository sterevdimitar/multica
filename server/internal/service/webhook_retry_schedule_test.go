package service

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestGitHubUnreachableRetrySchedule locks in the one schedule shared by the
// three "GitHub was unreachable" failures (dev-command-center design
// 2026-09-13-github-unreachable-retry, D4): dispatch_timeout, push_rejected,
// and timeout on a webhook task — three attempts, the retries deferred 5 and
// then 10 minutes. The daemon's timeout keeps upstream behaviour: a webhook
// runtime's "run never finished" is an orphaned GitHub Actions job, the
// daemon's is a dead process the daemon itself puts back, and the two do not
// share a cause.
func TestGitHubUnreachableRetrySchedule(t *testing.T) {
	ceilingCases := []struct {
		reason  string
		max     int32
		webhook bool
		want    int32
	}{
		{"dispatch_timeout", 2, true, githubUnreachableMaxAttempts},
		{"dispatch_timeout", 2, false, githubUnreachableMaxAttempts}, // webhook-only by construction; the flag is irrelevant
		{"push_rejected", 2, true, githubUnreachableMaxAttempts},
		{"timeout", 2, true, githubUnreachableMaxAttempts},
		{"timeout", 2, false, 2},                                              // the daemon's timeout is untouched
		{"dispatch_timeout", 1, true, 1},                                      // retry disabled stays disabled
		{"dispatch_timeout", 5, true, 5},                                      // widen-only: a larger budget is kept
		{"agent_error.provider_network", 2, true, providerNetworkMaxAttempts}, // unrelated schedule untouched
	}
	for _, tc := range ceilingCases {
		if got := retryAttemptCeiling(tc.reason, tc.max, tc.webhook); got != tc.want {
			t.Errorf("ceiling(%q, %d, webhook=%v) = %d, want %d", tc.reason, tc.max, tc.webhook, got, tc.want)
		}
	}

	delayCases := []struct {
		reason        string
		failedAttempt int32
		webhook       bool
		want          time.Duration
	}{
		{"dispatch_timeout", 1, true, 5 * time.Minute},
		{"dispatch_timeout", 2, true, 10 * time.Minute},
		{"dispatch_timeout", 3, true, 0}, // no fourth attempt; nothing to defer
		{"push_rejected", 1, true, 5 * time.Minute},
		{"push_rejected", 2, true, 10 * time.Minute},
		{"timeout", 1, true, 5 * time.Minute},
		{"timeout", 2, true, 10 * time.Minute},
		{"timeout", 1, false, 0}, // daemon timeout: immediate, as before
		{"timeout", 2, false, 0},
		{"agent_error.provider_network", 2, false, providerNetworkFinalRetryWait}, // unchanged
		{"runtime_offline", 1, true, 0},                                           // unchanged
	}
	for _, tc := range delayCases {
		if got := retryDelayForAttempt(tc.reason, tc.failedAttempt, tc.webhook); got != tc.want {
			t.Errorf("retryDelayForAttempt(%q, %d, webhook=%v) = %s, want %s", tc.reason, tc.failedAttempt, tc.webhook, got, tc.want)
		}
	}

	mkTask := func(attempt, max int32) db.AgentTaskQueue {
		return db.AgentTaskQueue{
			Attempt:     attempt,
			MaxAttempts: max,
			IssueID:     pgtype.UUID{Bytes: [16]byte{1}, Valid: true},
		}
	}
	eligCases := []struct {
		name    string
		reason  string
		attempt int32
		max     int32
		webhook bool
		want    bool
	}{
		{"dispatch_timeout first failure retries", "dispatch_timeout", 1, 2, true, true},
		{"dispatch_timeout second failure retries (10-minute tier)", "dispatch_timeout", 2, 2, true, true},
		{"dispatch_timeout third failure is final", "dispatch_timeout", 3, 2, true, false},
		{"push_rejected is retryable at all", "push_rejected", 1, 2, true, true},
		{"push_rejected third failure is final", "push_rejected", 3, 2, true, false},
		{"webhook timeout second failure retries", "timeout", 2, 2, true, true},
		{"daemon timeout exhausts at attempt 2, as before", "timeout", 2, 2, false, false},
		{"retry disabled never revived", "push_rejected", 1, 1, true, false},
	}
	for _, tc := range eligCases {
		if got := retryEligible(tc.reason, mkTask(tc.attempt, tc.max), tc.webhook); got != tc.want {
			t.Errorf("%s: retryEligible(%q, attempt=%d/max=%d, webhook=%v) = %v, want %v",
				tc.name, tc.reason, tc.attempt, tc.max, tc.webhook, got, tc.want)
		}
	}
}

// push_rejected must resume: the whole value of the retry is that the fixer
// rebuilds its fix from its own session record instead of from scratch.
func TestPushRejectedResumesTheSession(t *testing.T) {
	if resumeUnsafeFailureReason("push_rejected") {
		t.Fatal("push_rejected is resume-unsafe; the retry would start blind")
	}
	if !retryableReasons["push_rejected"] {
		t.Fatal("push_rejected is not retryable")
	}
}
