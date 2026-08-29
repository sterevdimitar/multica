package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const testPRPayload = `{"action":"opened","number":26,
  "pull_request":{"html_url":"https://github.com/sterevdimitar/dev-command-center/pull/26",
    "title":"fix: don't write issue status when the PR is already merged",
    "user":{"login":"u"},"head":{"ref":"h"},"base":{"ref":"main"}},
  "repository":{"full_name":"sterevdimitar/dev-command-center"}}`

// envelopeRun builds a webhook-sourced run carrying the {event, eventPayload}
// envelope. Check db.AutopilotRun's field names in
// server/pkg/db/generated/models.go before adjusting this.
//
// Named envelopeRun, not webhookRun (the plan's original name): autopilot_test.go
// already declares a package-level webhookRun(payload []byte) db.AutopilotRun helper
// with a different signature. Renamed here to avoid a redeclaration; every later task
// in this plan that needs this helper uses envelopeRun too.
//
// Deviation from the plan's literal helper body: json.Marshal(map[string]any{...,
// "eventPayload": json.RawMessage(eventPayload)}) compacts/validates the RawMessage
// during marshal, so it cannot represent a deliberately MALFORMED inner payload (the
// "malformed payload" test case below needs exactly that) — Marshal fails before
// pullRequestSlug is ever reached. Built by string concatenation instead, so
// eventPayload's bytes reach TriggerPayload completely unvalidated, malformed or not.
func envelopeRun(t *testing.T, event, eventPayload string) db.AutopilotRun {
	t.Helper()
	eventJSON, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	raw := fmt.Sprintf(`{"event":%s}`, eventJSON)
	if eventPayload != "" {
		raw = fmt.Sprintf(`{"event":%s,"eventPayload":%s}`, eventJSON, eventPayload)
	}
	return db.AutopilotRun{Source: "webhook", TriggerPayload: []byte(raw)}
}

func TestPullRequestSlug(t *testing.T) {
	slug, ok := pullRequestSlug(envelopeRun(t, "github.pull_request", testPRPayload))
	if !ok {
		t.Fatal("a pull_request payload must yield a slug")
	}
	if slug != "sterevdimitar/dev-command-center#26" {
		t.Fatalf("slug = %q, want owner/repo#number with no space", slug)
	}
}

// I3: every one of these must report absent, so no metadata key is written.
func TestPullRequestSlugAbsentCases(t *testing.T) {
	cases := []struct{ name, event, payload string }{
		{"non-PR event", "github.push", `{"ref":"refs/heads/main"}`},
		{"PR event, no pull_request object", "github.pull_request", `{"action":"opened"}`},
		{"malformed payload", "github.pull_request", `{`},
		{"empty payload", "github.pull_request", ``},
		{"not a webhook run", "", ``},
	}
	for _, c := range cases {
		if slug, ok := pullRequestSlug(envelopeRun(t, c.event, c.payload)); ok {
			t.Errorf("%s: expected absent, got %q", c.name, slug)
		}
	}
}

func TestPullRequestSlugNeverEmptyWhenPresent(t *testing.T) {
	if slug, ok := pullRequestSlug(db.AutopilotRun{}); ok && slug == "" {
		t.Fatal("ok with an empty slug would collapse every PR-less card into one")
	}
}

func TestInterpolateTemplateRendersPRTitle(t *testing.T) {
	s := &AutopilotService{}
	ap := db.Autopilot{Title: "PR Review on MR"}
	ap.IssueTitleTemplate = pgtype.Text{String: "{{pr_title}}", Valid: true}

	got := s.interpolateTemplate(ap, envelopeRun(t, "github.pull_request", testPRPayload), "UTC")
	want := "fix: don't write issue status when the PR is already merged"
	if got != want {
		t.Fatalf("title = %q, want the PR's own title %q", got, want)
	}
}

func TestInterpolateTemplatePRTitleFallsBackToAutopilotTitle(t *testing.T) {
	s := &AutopilotService{}
	ap := db.Autopilot{Title: "PR Review on MR"}
	ap.IssueTitleTemplate = pgtype.Text{String: "{{pr_title}}", Valid: true}

	if got := s.interpolateTemplate(ap, db.AutopilotRun{}, "UTC"); got != "PR Review on MR" {
		t.Fatalf("PR-less title = %q, want the autopilot's own title", got)
	}
}

func TestValidateIssueTitleTemplateAcceptsNewTokens(t *testing.T) {
	for _, tmpl := range []string{"{{pr_title}}", "{{pr}}", "{{date}} {{pr}}"} {
		if err := ValidateIssueTitleTemplate(tmpl); err != nil {
			t.Errorf("%s must validate: %v", tmpl, err)
		}
	}
	if err := ValidateIssueTitleTemplate("{{nope}}"); err == nil {
		t.Fatal("an unknown token must still be rejected")
	}
}

func TestBuildIssueDescriptionLeadsWithPRIdentity(t *testing.T) {
	s := &AutopilotService{}
	ap := db.Autopilot{Description: pgtype.Text{String: "Fires the review chain…", Valid: true}}

	got := s.buildIssueDescription(ap, envelopeRun(t, "github.pull_request", testPRPayload), "UTC")
	first := strings.SplitN(strings.TrimSpace(got.String), "\n", 2)[0]
	if first != "sterevdimitar/dev-command-center #26" {
		t.Fatalf("first line = %q, want the identity line with a space before #", first)
	}
}

// I7: a non-PR run's description must be unchanged.
func TestBuildIssueDescriptionUnchangedWithoutPR(t *testing.T) {
	s := &AutopilotService{}
	ap := db.Autopilot{Description: pgtype.Text{String: "Fires the review chain…", Valid: true}}

	got := s.buildIssueDescription(ap, db.AutopilotRun{}, "UTC")
	if !strings.HasPrefix(got.String, "Fires the review chain…") {
		t.Fatalf("non-PR description must still open with the autopilot description, got %q",
			got.String[:min(60, len(got.String))])
	}
}
