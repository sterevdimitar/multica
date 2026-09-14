package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A pull-request push restarts the review chain.
//
// When a `synchronize` attaches to a card that already exists (one card per
// pull request), every agent run on that card began on a head that no longer
// exists. Before this file, the attach path simply queued one more reviewer
// task beside them, and a card could carry two whole chains — two reviews,
// two validators, two fixers — for one pull request (dev-command-center card
// 221, 2026-09-13). Now the push cancels what is in flight, hands the card
// back to the chain's entry agent, and lets the attach path enqueue exactly
// the one fresh review it always did.
//
// The one exemption is the run that made the push. The fork cannot tell
// pushes apart by author — the payload's `sender` is the PAT owner for a
// human push and the pipeline's alike, and no credential here can look a
// commit up — so the exemption is by agent NAME and task state: a task that
// is running or dispatched and belongs to an agent named in
// MULTICA_PUSH_EXEMPT_AGENTS is never cancelled by a push. The default names
// the pipeline's fixer, the only agent that writes to the branch.
//
// Design: dev-command-center
// docs/superpowers/specs/2026-09-13-outside-push-restarts-the-chain-design.md.

// pushRestartTask is the slice of a task row the planner needs.
type pushRestartTask struct {
	ID        pgtype.UUID
	AgentID   pgtype.UUID
	AgentName string
	Status    string
}

// pushActiveStatuses are the task states a push invalidates: not yet run, or
// running on the head that just went away.
var pushActiveStatuses = map[string]bool{
	"queued":                  true,
	"dispatched":              true,
	"running":                 true,
	"waiting_local_directory": true,
	// A retry the fork is holding (dispatch_timeout / timeout /
	// push_rejected, 5 then 10 minutes) — a run that is still coming, for
	// the old head. Every enumeration of live statuses includes it.
	"deferred": true,
}

// pushExemptAgents reads MULTICA_PUSH_EXEMPT_AGENTS: a comma-separated list
// of agent names whose running or dispatched task a push never cancels.
// Unset means the pipeline's fixer; an explicitly empty value exempts
// nothing. The name is load-bearing the way `readiness-agent` is on the
// runner: renaming the fixer in pipeline/agents.yaml without updating this
// setting makes its own push cancel it mid-run.
func pushExemptAgents() map[string]bool {
	raw, set := os.LookupEnv("MULTICA_PUSH_EXEMPT_AGENTS")
	if !set {
		return map[string]bool{"fixer": true}
	}
	out := map[string]bool{}
	for _, name := range strings.Split(raw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out[name] = true
		}
	}
	return out
}

// planPushRestart is the whole decision, pure so its table is its test.
//
// Every active task is cancelled except one that is running or dispatched
// under an exempt name — the run that made the push. The restart itself is
// unconditional: a card with no assignee (parked at in_review, blocked after
// a failed run) is not an exception, because a push to a parked pull request
// is a fix attempt and the pipeline's job is to look at it (dev-command-center
// docs/superpowers/specs/2026-09-13-push-to-parked-card-design.md §2). Whether
// the card had an assignee is the executor's business — it writes one back.
func planPushRestart(tasks []pushRestartTask, exempt map[string]bool) (cancel []pushRestartTask) {
	for _, t := range tasks {
		if !pushActiveStatuses[t.Status] {
			continue
		}
		if (t.Status == "running" || t.Status == "dispatched") && exempt[t.AgentName] {
			continue
		}
		cancel = append(cancel, t)
	}
	return cancel
}

// pullRequestPush reads the pushed head and the pushing account from the
// run's {event, eventPayload} envelope — the same envelope pullRequestFor
// decodes. Absent fields are empty strings; nothing here can fail.
func pullRequestPush(run db.AutopilotRun) (headSHA, sender string) {
	if run.Source != "webhook" || len(run.TriggerPayload) == 0 {
		return "", ""
	}
	var env struct {
		Event        string          `json:"event"`
		EventPayload json.RawMessage `json:"eventPayload"`
	}
	if err := json.Unmarshal(run.TriggerPayload, &env); err != nil {
		return "", ""
	}
	if !strings.HasPrefix(env.Event, "github.pull_request") || len(env.EventPayload) == 0 {
		return "", ""
	}
	var ev struct {
		PullRequest *struct {
			Head struct {
				SHA string `json:"sha"`
			} `json:"head"`
		} `json:"pull_request"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
	}
	if err := json.Unmarshal(env.EventPayload, &ev); err != nil {
		return "", ""
	}
	if ev.PullRequest != nil {
		headSHA = strings.TrimSpace(ev.PullRequest.Head.SHA)
	}
	return headSHA, strings.TrimSpace(ev.Sender.Login)
}

// pushRestartComment renders the trace left on the card. It is built from
// agent names (ours), a SHA, an issue status (ours), and the sender —
// untrusted payload text — reduced to the characters a GitHub login can
// contain, so the body can never carry a mention: a mention here would be a
// second dispatch.
//
// When the card had no assignee at the push — parked at in_review, blocked
// after a failed run — the comment says which state it sat in and that the
// push lifted it, so a human reading the card knows the park did not
// silently evaporate. An assigned card never gets that clause.
func pushRestartComment(headSHA, sender string, cancelled []pushRestartTask, priorStatus string, wasUnassigned bool) string {
	short := "unknown"
	if headSHA != "" {
		short = headSHA
		if len(short) > 7 {
			short = short[:7]
		}
	}
	by := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			return r
		}
		return -1
	}, sender)
	if by == "" {
		by = "an unknown sender"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "↻ Head moved to `%s` (pushed by %s)", short, by)
	if wasUnassigned {
		status := strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r == '_' {
				return r
			}
			return -1
		}, priorStatus)
		if status == "" {
			status = "unknown"
		}
		fmt.Fprintf(&b, " while this card sat at `%s` with no assignee. Park lifted.", status)
	} else {
		b.WriteString(".")
	}
	if len(cancelled) > 0 {
		parts := make([]string, 0, len(cancelled))
		for _, t := range cancelled {
			parts = append(parts, fmt.Sprintf("%s (%s)", t.AgentName, t.Status))
		}
		fmt.Fprintf(&b, " Cancelled: %s.", strings.Join(parts, ", "))
	}
	b.WriteString(" Review restarted from the top.")
	return b.String()
}

// restartChainOnPush executes planPushRestart for a synchronize that
// attached to issue, and returns the issue the attach path must enqueue for.
//
// Per-task cancellation goes through TaskService.CancelTask, the Stop
// button's path, so a cancelled run's GitHub job is cancelled too and its
// step-finalizer never runs: a cancelled run writes nothing. A cancel that
// fails is logged and skipped — the restart still happens; a failed
// re-assign is returned, because enqueueing for the wrong agent is worse
// than not enqueueing. The card is handed back to the autopilot's assignee
// only for agent-assigned autopilots; a card with no assignee is left
// exactly as it was.
func (s *AutopilotService) restartChainOnPush(ctx context.Context, ap db.Autopilot, run db.AutopilotRun, issue db.Issue) (db.Issue, error) {
	if ap.AssigneeType != "agent" || !ap.AssigneeID.Valid || !issue.AssigneeID.Valid {
		return issue, nil
	}
	rows, err := s.Queries.ListTasksByIssue(ctx, issue.ID)
	if err != nil {
		return issue, fmt.Errorf("list tasks on issue: %w", err)
	}
	names := map[pgtype.UUID]string{}
	tasks := make([]pushRestartTask, 0, len(rows))
	for _, r := range rows {
		if !pushActiveStatuses[r.Status] {
			continue
		}
		name, seen := names[r.AgentID]
		if !seen {
			if agent, err := s.Queries.GetAgent(ctx, r.AgentID); err == nil {
				name = agent.Name
			}
			names[r.AgentID] = name
		}
		tasks = append(tasks, pushRestartTask{ID: r.ID, AgentID: r.AgentID, AgentName: name, Status: r.Status})
	}
	cancel := planPushRestart(tasks, pushExemptAgents())
	headSHA, sender := pullRequestPush(run)
	cancelled := make([]pushRestartTask, 0, len(cancel))
	for _, t := range cancel {
		if _, err := s.TaskSvc.CancelTask(ctx, t.ID); err != nil {
			slog.Warn("push restart: cancel task failed",
				"issue_id", util.UUIDToString(issue.ID), "task_id", util.UUIDToString(t.ID),
				"agent", t.AgentName, "error", err)
			continue
		}
		cancelled = append(cancelled, t)
	}
	if issue.AssigneeType.String != "agent" || issue.AssigneeID != ap.AssigneeID {
		updated, err := s.Queries.UpdateIssueAssignee(ctx, db.UpdateIssueAssigneeParams{
			ID:           issue.ID,
			AssigneeType: pgtype.Text{String: "agent", Valid: true},
			AssigneeID:   ap.AssigneeID,
		})
		if err != nil {
			return issue, fmt.Errorf("hand the card back to the entry agent: %w", err)
		}
		slog.Info("push restart: card handed back to the entry agent",
			"issue_id", util.UUIDToString(issue.ID),
			"from", util.UUIDToString(issue.AssigneeID), "to", util.UUIDToString(ap.AssigneeID))
		issue = updated
	}
	slog.Info("push restart",
		"issue_id", util.UUIDToString(issue.ID), "head", headSHA, "sender", sender,
		"cancelled", len(cancelled), "planned", len(cancel))
	s.TaskSvc.createAgentComment(ctx, issue.ID, ap.AssigneeID,
		pushRestartComment(headSHA, sender, cancelled, issue.Status, false), "comment", pgtype.UUID{}, pgtype.UUID{})
	return issue, nil
}
