package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestAutopilotErrorType(t *testing.T) {
	cases := map[string]string{
		"unknown execution_mode: nope": "configuration",
		"issue blocked":                "issue_terminal",
		"issue cancelled":              "issue_terminal",
		"enqueue task: no runtime":     "dispatch_error",
		"task failed":                  "task_error",
		"unexpected":                   "autopilot_error",
	}

	for reason, want := range cases {
		if got := autopilotErrorType(reason); got != want {
			t.Fatalf("autopilotErrorType(%q) = %q, want %q", reason, got, want)
		}
	}
}

func TestTaskFailureReasonForAutopilotRun(t *testing.T) {
	cases := []struct {
		name string
		task db.AgentTaskQueue
		want string
	}{
		{
			name: "prefers raw error text",
			task: db.AgentTaskQueue{
				Error:         pgtype.Text{String: "tests failed", Valid: true},
				FailureReason: pgtype.Text{String: "agent_error", Valid: true},
			},
			want: "tests failed",
		},
		{
			name: "falls back to classified reason when error is blank",
			task: db.AgentTaskQueue{
				Error:         pgtype.Text{String: "   ", Valid: true},
				FailureReason: pgtype.Text{String: "codex_semantic_inactivity", Valid: true},
			},
			want: "codex_semantic_inactivity",
		},
		{
			name: "generic default when nothing is set",
			task: db.AgentTaskQueue{},
			want: "task failed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := taskFailureReasonForAutopilotRun(tc.task); got != tc.want {
				t.Fatalf("taskFailureReasonForAutopilotRun() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildIssueDescription_NoTriggerPayload(t *testing.T) {
	s := &AutopilotService{}
	ap := db.Autopilot{Description: pgtype.Text{String: "do the thing", Valid: true}}
	run := db.AutopilotRun{Source: "schedule", TriggeredAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}}

	got := s.buildIssueDescription(ap, run, "UTC")
	if !strings.HasPrefix(got.String, "do the thing") {
		t.Fatalf("description should preserve user description: %q", got.String)
	}
	if !strings.Contains(got.String, "Autopilot run triggered at") {
		t.Fatalf("description should include schedule note: %q", got.String)
	}
	if strings.Contains(got.String, "Webhook event") {
		t.Fatalf("description must not mention webhook for non-webhook source: %q", got.String)
	}
}

func TestBuildIssueDescription_UsesTriggerTimezone(t *testing.T) {
	s := &AutopilotService{}
	ap := db.Autopilot{Description: pgtype.Text{String: "daily sync", Valid: true}}
	run := db.AutopilotRun{
		Source:      "schedule",
		TriggeredAt: pgtype.Timestamptz{Time: time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC), Valid: true},
	}

	got := s.buildIssueDescription(ap, run, "Asia/Tokyo")
	if !strings.Contains(got.String, "Autopilot run triggered at 2026-05-26 09:00 Asia/Tokyo") {
		t.Fatalf("description should use trigger timezone: %q", got.String)
	}
	if strings.Contains(got.String, "2026-05-26 00:00 UTC") {
		t.Fatalf("description must not render the trigger time in UTC when trigger timezone is known: %q", got.String)
	}
}

// An invalid IANA timezone string must fall back to UTC instead of leaving the
// timestamp half-formatted in the issue body.
func TestBuildIssueDescription_InvalidTriggerTimezoneFallsBackToUTC(t *testing.T) {
	s := &AutopilotService{}
	ap := db.Autopilot{Description: pgtype.Text{String: "do the thing", Valid: true}}
	run := db.AutopilotRun{
		Source:      "schedule",
		TriggeredAt: pgtype.Timestamptz{Time: time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC), Valid: true},
	}

	got := s.buildIssueDescription(ap, run, "Foo/Bar")
	if !strings.Contains(got.String, "Autopilot run triggered at 2026-05-26 00:00 UTC") {
		t.Fatalf("invalid trigger timezone should fall back to UTC: %q", got.String)
	}
}

func TestInterpolateTemplate_InvalidTriggerTimezoneFallsBackToUTC(t *testing.T) {
	s := &AutopilotService{}
	ap := db.Autopilot{
		Title:              "fallback",
		IssueTitleTemplate: pgtype.Text{String: "report {{date}}", Valid: true},
	}
	run := db.AutopilotRun{
		TriggeredAt: pgtype.Timestamptz{Time: time.Date(2026, 5, 26, 23, 30, 0, 0, time.UTC), Valid: true},
	}

	got := s.interpolateTemplate(ap, run, "Foo/Bar")
	if want := "report 2026-05-26"; got != want {
		t.Fatalf("interpolateTemplate = %q, want %q", got, want)
	}
}

func TestBuildIssueDescription_WithWebhookPayload(t *testing.T) {
	s := &AutopilotService{}
	ap := db.Autopilot{Description: pgtype.Text{String: "watch PRs", Valid: true}}
	payload := []byte(`{"event":"github.pull_request.opened","eventPayload":{"number":7},"request":{"receivedAt":"2026-05-09T00:00:00Z","contentType":"application/json"}}`)
	run := db.AutopilotRun{Source: "webhook", TriggerPayload: payload, TriggeredAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}}

	got := s.buildIssueDescription(ap, run, "UTC")
	if !strings.HasPrefix(got.String, "watch PRs") {
		t.Fatalf("user description not preserved: %q", got.String)
	}
	if !strings.Contains(got.String, "Webhook event: github.pull_request.opened") {
		t.Fatalf("description should include webhook event line: %q", got.String)
	}
	if !strings.Contains(got.String, "\"number\": 7") && !strings.Contains(got.String, "\"number\":7") {
		t.Fatalf("description should include payload json: %q", got.String)
	}
	// Italic schedule line must come before the webhook block.
	idxItalic := strings.Index(got.String, "*Autopilot run triggered")
	idxWebhook := strings.Index(got.String, "Webhook event")
	if idxItalic < 0 || idxWebhook < 0 || idxItalic > idxWebhook {
		t.Fatalf("italic line should appear before webhook block: %q", got.String)
	}
}

func TestBuildIssueDescription_WebhookSourceMissingEnvelope(t *testing.T) {
	// Defensive: if a future caller stuffs a non-envelope JSON object into
	// trigger_payload, we should still emit a webhook block with sensible
	// defaults rather than skipping the section entirely.
	s := &AutopilotService{}
	ap := db.Autopilot{Description: pgtype.Text{String: "thing", Valid: true}}
	payload := []byte(`{"raw":"missing envelope"}`)
	run := db.AutopilotRun{Source: "webhook", TriggerPayload: payload, TriggeredAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}}

	got := s.buildIssueDescription(ap, run, "UTC")
	if !strings.Contains(got.String, "Webhook event:") {
		t.Fatalf("should still emit webhook block: %q", got.String)
	}
}

func TestBuildIssueDescription_NonWebhookSourceWithPayloadIgnored(t *testing.T) {
	// Manual / schedule with a payload should not get a webhook block.
	s := &AutopilotService{}
	ap := db.Autopilot{Description: pgtype.Text{String: "thing", Valid: true}}
	run := db.AutopilotRun{Source: "manual", TriggerPayload: []byte(`{"event":"x.y"}`), TriggeredAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}}

	got := s.buildIssueDescription(ap, run, "UTC")
	if strings.Contains(got.String, "Webhook event") {
		t.Fatalf("non-webhook source should not include webhook block: %q", got.String)
	}
}

// ── GitHub pull-request rendering ───────────────────────────────────────
//
// A GitHub PR payload is ~30 KB of JSON, and dumping it verbatim made the
// issue body unreadable for the humans watching the board while burying
// the one thing the agent actually needs — the PR URL — inside a wall of
// API links. For pull_request events we render a short header plus the
// PR's own description instead. Every other event keeps the raw dump,
// because it is the agent's only channel for event context and we have no
// per-provider knowledge to summarise it with.

// prPayload builds a webhook envelope shaped like a real GitHub
// pull_request event. Field names mirror the payload captured from
// production run 5c0b3850.
func prPayload(t *testing.T, body string) []byte {
	t.Helper()
	return prPayloadWith(t, map[string]any{
		"html_url": "https://github.com/acme/widgets/pull/6",
		"title":    "fix: the thing",
		"body":     body,
		"user":     map[string]any{"login": "octocat"},
		"head":     map[string]any{"ref": "feature/x"},
		"base":     map[string]any{"ref": "main"},
	})
}

func prPayloadWith(t *testing.T, pr map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"event": "github.pull_request.opened",
		"eventPayload": map[string]any{
			"action":       "opened",
			"number":       6,
			"pull_request": pr,
			"repository":   map[string]any{"full_name": "acme/widgets"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func webhookRun(payload []byte) db.AutopilotRun {
	return db.AutopilotRun{
		Source:         "webhook",
		TriggerPayload: payload,
		TriggeredAt:    pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
	}
}

func TestBuildIssueDescription_GitHubPullRequestIsHumanReadable(t *testing.T) {
	s := &AutopilotService{}
	ap := db.Autopilot{Description: pgtype.Text{String: "watch PRs", Valid: true}}
	got := s.buildIssueDescription(ap, webhookRun(prPayload(t, "Fixes the widget.\n\n## Details\nlots of prose")), "UTC").String

	// One Card Per Pull Request (2026-08-28) moved the autopilot's own
	// description and the rename instruction BELOW the pull-request block for
	// PR-triggered runs — they're identical on every card and don't belong at
	// the top (design §3.3). The identity line leads instead; the boilerplate
	// is still preserved verbatim, just relocated.
	if !strings.HasPrefix(got, "acme/widgets #6") {
		t.Errorf("PR-triggered description must lead with the identity line:\n%s", got)
	}
	if !strings.Contains(got, "watch PRs") {
		t.Errorf("user description not preserved:\n%s", got)
	}
	if !strings.Contains(got, "*Autopilot run triggered at") {
		t.Errorf("rename instruction missing:\n%s", got)
	}

	for _, want := range []string{
		"https://github.com/acme/widgets/pull/6", // the one link the agent needs
		"acme/widgets#6",
		"fix: the thing",
		"octocat",
		"feature/x",
		"main",
		"Fixes the widget.", // the PR's own description, verbatim
		"## Details",
		"lots of prose",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("body missing %q:\n%s", want, got)
		}
	}

	// The whole point: no raw payload dump.
	if strings.Contains(got, "Webhook payload:") || strings.Contains(got, "```json") {
		t.Errorf("PR events must not dump the raw payload:\n%s", got)
	}
	// None of the API-link noise from the payload may leak through.
	if strings.Contains(got, "api.github.com") {
		t.Errorf("API links leaked into the body:\n%s", got)
	}
}

// The event name alone is NOT the discriminator. An envelope that says
// pull_request but carries no usable pull_request object must fall back to
// the raw dump rather than render a header full of blanks.
func TestBuildIssueDescription_PullRequestEventWithoutPRObjectFallsBack(t *testing.T) {
	s := &AutopilotService{}
	ap := db.Autopilot{Description: pgtype.Text{String: "watch PRs", Valid: true}}
	payload := []byte(`{"event":"github.pull_request.opened","eventPayload":{"number":7}}`)

	got := s.buildIssueDescription(ap, webhookRun(payload), "UTC").String
	if !strings.Contains(got, "Webhook payload:") {
		t.Errorf("should fall back to the raw dump:\n%s", got)
	}
	if strings.Contains(got, "**Pull request:**") {
		t.Errorf("must not render a PR header with no PR object:\n%s", got)
	}
}

// A pull_request object with no html_url is equally unusable.
func TestBuildIssueDescription_PullRequestWithoutURLFallsBack(t *testing.T) {
	s := &AutopilotService{}
	ap := db.Autopilot{Description: pgtype.Text{String: "watch PRs", Valid: true}}
	payload := prPayloadWith(t, map[string]any{"title": "no url here"})

	got := s.buildIssueDescription(ap, webhookRun(payload), "UTC").String
	if !strings.Contains(got, "Webhook payload:") {
		t.Errorf("should fall back to the raw dump:\n%s", got)
	}
}

// Non-GitHub and non-PR webhooks are untouched — we have no provider
// knowledge to summarise them with, and the raw payload is the agent's
// only source of event context.
func TestBuildIssueDescription_OtherWebhookEventsStillDumpPayload(t *testing.T) {
	s := &AutopilotService{}
	ap := db.Autopilot{Description: pgtype.Text{String: "watch things", Valid: true}}
	for _, event := range []string{
		"github.workflow_run.completed",
		"github.issue_comment.created",
		"gitlab.Merge Request Hook",
		"webhook.received",
	} {
		t.Run(event, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{
				"event":        event,
				"eventPayload": map[string]any{"pull_request": map[string]any{"html_url": "https://example.test/pull/1"}},
			})
			got := s.buildIssueDescription(ap, webhookRun(raw), "UTC").String
			if !strings.Contains(got, "Webhook payload:") {
				t.Errorf("%s should still dump the raw payload:\n%s", event, got)
			}
		})
	}
}

// A PR opened with no description must not leave a dangling empty section.
func TestBuildIssueDescription_EmptyPullRequestBody(t *testing.T) {
	s := &AutopilotService{}
	ap := db.Autopilot{Description: pgtype.Text{String: "watch PRs", Valid: true}}
	got := s.buildIssueDescription(ap, webhookRun(prPayload(t, "   ")), "UTC").String

	if !strings.Contains(got, "https://github.com/acme/widgets/pull/6") {
		t.Errorf("link must still be present:\n%s", got)
	}
	if !strings.Contains(got, "no description provided") {
		t.Errorf("empty body should say so explicitly:\n%s", got)
	}
}

// PR bodies are unbounded. Left uncapped, one runaway description would
// bloat the issue row for everyone looking at the board.
func TestBuildIssueDescription_LongPullRequestBodyIsTruncated(t *testing.T) {
	s := &AutopilotService{}
	ap := db.Autopilot{Description: pgtype.Text{String: "watch PRs", Valid: true}}
	long := strings.Repeat("x", maxIssuePRBodyBytes+5000)
	got := s.buildIssueDescription(ap, webhookRun(prPayload(t, long)), "UTC").String

	if len(got) > maxIssuePRBodyBytes+4096 {
		t.Errorf("body is %d bytes, want it capped near %d", len(got), maxIssuePRBodyBytes)
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("a truncated body must say so:\n%s", got[len(got)-300:])
	}
	if !strings.Contains(got, "https://github.com/acme/widgets/pull/6") {
		t.Errorf("link must survive truncation:\n%s", got[:400])
	}
}

// TestInterpolateTemplate covers the three behaviours that real autopilot
// runs depend on: {{date}} substitution, falling back to Title when the
// template is unset/empty, and leaving any non-{{date}} text alone (the
// handler is the layer that prevents unknown tokens from being stored in
// the first place — service-layer interpolation stays substitute-or-leave).
func TestInterpolateTemplate(t *testing.T) {
	s := &AutopilotService{}
	run := db.AutopilotRun{TriggeredAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}}
	today := run.TriggeredAt.Time.UTC().Format("2006-01-02")

	cases := []struct {
		name   string
		ap     db.Autopilot
		expect string
	}{
		{
			name:   "date placeholder substituted",
			ap:     db.Autopilot{Title: "fallback", IssueTitleTemplate: pgtype.Text{String: "probe — {{date}}", Valid: true}},
			expect: "probe — " + today,
		},
		{
			name:   "date placeholder with whitespace substituted",
			ap:     db.Autopilot{Title: "fallback", IssueTitleTemplate: pgtype.Text{String: "probe — {{ date }}", Valid: true}},
			expect: "probe — " + today,
		},
		{
			name:   "empty template falls back to autopilot title",
			ap:     db.Autopilot{Title: "fallback title", IssueTitleTemplate: pgtype.Text{Valid: false}},
			expect: "fallback title",
		},
		{
			name:   "template without placeholder is returned verbatim",
			ap:     db.Autopilot{Title: "fallback", IssueTitleTemplate: pgtype.Text{String: "static title", Valid: true}},
			expect: "static title",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.interpolateTemplate(tc.ap, run, "UTC"); got != tc.expect {
				t.Fatalf("interpolateTemplate = %q, want %q", got, tc.expect)
			}
		})
	}
}

func TestInterpolateTemplate_UsesTriggerTimezoneForDate(t *testing.T) {
	s := &AutopilotService{}
	ap := db.Autopilot{
		Title:              "fallback",
		IssueTitleTemplate: pgtype.Text{String: "Tokyo report {{date}}", Valid: true},
	}
	run := db.AutopilotRun{
		TriggeredAt: pgtype.Timestamptz{Time: time.Date(2026, 5, 26, 23, 30, 0, 0, time.UTC), Valid: true},
	}

	got := s.interpolateTemplate(ap, run, "Asia/Tokyo")
	if want := "Tokyo report 2026-05-27"; got != want {
		t.Fatalf("interpolateTemplate = %q, want %q", got, want)
	}
}

// TestValidateIssueTitleTemplate locks down what create/update accept.
// Reject path: anything inside {{...}} that is not in the supported set.
// Accept path: empty, plain text, and the canonical {{date}} placeholder
// in both compact and whitespace-padded forms.
func TestValidateIssueTitleTemplate(t *testing.T) {
	t.Run("accepts empty template", func(t *testing.T) {
		if err := ValidateIssueTitleTemplate(""); err != nil {
			t.Fatalf("empty template must be valid: %v", err)
		}
	})
	t.Run("accepts plain text", func(t *testing.T) {
		if err := ValidateIssueTitleTemplate("daily report"); err != nil {
			t.Fatalf("plain text must be valid: %v", err)
		}
	})
	t.Run("accepts {{date}}", func(t *testing.T) {
		if err := ValidateIssueTitleTemplate("probe — {{date}}"); err != nil {
			t.Fatalf("{{date}} must be valid: %v", err)
		}
	})
	t.Run("accepts {{ date }} with whitespace", func(t *testing.T) {
		if err := ValidateIssueTitleTemplate("probe — {{ date }}"); err != nil {
			t.Fatalf("{{ date }} must be valid: %v", err)
		}
	})

	rejections := []struct {
		name string
		tmpl string
		// nameInError is the offending variable name that must appear in the
		// returned error so CLI users see which token was rejected.
		nameInError string
	}{
		{"go template style", "probe — {{.TriggeredAt}}", ".TriggeredAt"},
		{"mustache style unknown variable", "probe — {{trigger_id}}", "trigger_id"},
		{"datetime not yet supported", "probe — {{datetime}}", "datetime"},
		{"empty placeholder", "probe — {{}}", ""},
		{"mixed valid + invalid still fails", "probe — {{date}} {{trigger_source}}", "trigger_source"},
	}
	for _, tc := range rejections {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateIssueTitleTemplate(tc.tmpl)
			if err == nil {
				t.Fatalf("expected rejection for %q", tc.tmpl)
			}
			if !strings.Contains(err.Error(), "unknown template variable") {
				t.Fatalf("error should mention unknown template variable: %v", err)
			}
			if tc.nameInError != "" && !strings.Contains(err.Error(), tc.nameInError) {
				t.Fatalf("error should name the offending token %q: %v", tc.nameInError, err)
			}
		})
	}
}
