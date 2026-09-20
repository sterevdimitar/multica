package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Runtime placement (dev-command-center design
// 2026-09-20-runtime-placement §3): where a webhook task runs.
//
// The agent's runtime is a PREFERENCE, not an override: when it is up the
// agent runs only there — a full runtime is a wait, never a move (P2). Only
// a runtime that is DOWN (runtimeAvailable false: out of the rotation, or
// in a cool-down) moves a run, and then to the first runtime in the
// workspace's fallback order that is in the agent's fallback set, available
// and free (P1). When nothing fits, the run waits — left `queued`, ⏸ in the
// popover — and is re-placed by every drain: each completion, each sweeper
// tick, each cool-down expiry.
//
// Invariants that do not surface by running the code:
//
//   - P5: every placement goes through PlaceAgentTask, whose status='queued'
//     predicate lets exactly one of two racing drains win. A drain that
//     loses reads pgx.ErrNoRows and moves on.
//   - P12: the HOME is the agent's runtime NOW, never task.runtime_id. A
//     retry child inherits its parent's runtime_id, which may be the
//     fallback the parent ran on.
//   - P13: the failover note is posted through createAgentComment, which
//     writes the row and publishes the event and dispatches nothing — the
//     enqueue-on-comment predicates live in the HTTP comment handler only.
//   - The cap check is read-then-act, so a race can exceed a cap by one; the
//     per-agent gate has the same property and it is accepted for the same
//     reason (a slot's worth, once, under contention).

// choosePlacement is the pure core of the placement rule: given the agent's
// home, the workspace's webhook runtimes in fallback order, the agent's
// fallback set and a running-count lookup, it returns the runtime to
// dispatch on, whether that is off the home, and ok=false for "wait".
func choosePlacement(
	home db.AgentRuntime,
	ordered []db.AgentRuntime,
	fallback []pgtype.UUID,
	running func(runtimeID pgtype.UUID) int64,
	now time.Time,
) (target db.AgentRuntime, offHome bool, ok bool) {
	if runtimeAvailable(home, now) {
		if runtimeFree(home, running(home.ID)) {
			return home, false, true
		}
		return db.AgentRuntime{}, false, false // full is not down (P2)
	}
	homeID := util.UUIDToString(home.ID)
	for _, r := range ordered {
		// The home was judged above from the fresher read; its copy in the
		// ordered list is never a fallback for itself.
		if util.UUIDToString(r.ID) == homeID {
			continue
		}
		if !inFallbackSet(r.ID, fallback) || !runtimeAvailable(r, now) {
			continue
		}
		if !r.WebhookUrl.Valid || r.WebhookUrl.String == "" {
			continue
		}
		if runtimeFree(r, running(r.ID)) {
			return r, true, true
		}
	}
	return db.AgentRuntime{}, false, false
}

func inFallbackSet(id pgtype.UUID, set []pgtype.UUID) bool {
	want := util.UUIDToString(id)
	for _, s := range set {
		if util.UUIDToString(s) == want {
			return true
		}
	}
	return false
}

// runtimeDisplayName is what Multica's clients render: custom_name ?? name.
func runtimeDisplayName(r db.AgentRuntime) string {
	if r.CustomName.Valid && r.CustomName.String != "" {
		return r.CustomName.String
	}
	return r.Name
}

// failoverNote is the one comment a failed-over run leaves on the card:
// `↪ Local PC is down (no online runner carries "local-pc") — running on CircleCI`.
func failoverNote(home, target db.AgentRuntime) string {
	reason := "unavailable"
	if home.MaxConcurrentTasks.Valid && home.MaxConcurrentTasks.Int32 == 0 {
		reason = "out of the rotation"
	} else if home.DownReason.Valid && home.DownReason.String != "" {
		reason = home.DownReason.String
	}
	return fmt.Sprintf("↪ %s is down (%s) — running on %s", runtimeDisplayName(home), reason, runtimeDisplayName(target))
}

// placeWebhookTask is the body of MaybeDispatchToWebhook: the only path
// that moves a webhook task from queued to dispatched. Returns false only
// when the agent's CURRENT runtime is not a webhook runtime (or cannot be
// loaded) — the daemon claim path, untouched. Every other outcome returns
// true: dispatched on the home, dispatched on a fallback (with the note),
// or left queued to wait.
func (s *TaskService) placeWebhookTask(ctx context.Context, task db.AgentTaskQueue) bool {
	taskID := util.UUIDToString(task.ID)
	agent, err := s.Queries.GetAgent(ctx, task.AgentID)
	if err != nil {
		slog.Error("webhook: load agent for placement", "err", err, "task_id", taskID)
		return false
	}
	if !agent.RuntimeID.Valid {
		return false
	}
	home, err := s.Queries.GetAgentRuntime(ctx, agent.RuntimeID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("webhook: load runtime for placement", "err", err, "task_id", taskID)
		}
		return false
	}
	if home.RuntimeMode != "webhook" {
		return false
	}
	if !home.WebhookUrl.Valid || home.WebhookUrl.String == "" {
		slog.Error("webhook: runtime in mode=webhook but no webhook_url", "runtime_id", util.UUIDToString(home.ID))
		return false
	}

	running, err := s.Queries.CountRunningTasks(ctx, task.AgentID)
	if err != nil {
		slog.Error("webhook: count running tasks", "err", err, "task_id", taskID)
		return false
	}
	if running >= int64(agent.MaxConcurrentTasks) {
		slog.Info("webhook: no agent capacity, task stays queued",
			"task_id", taskID, "agent_id", util.UUIDToString(task.AgentID),
			"running", running, "max", agent.MaxConcurrentTasks)
		return true
	}

	ordered, err := s.Queries.ListWebhookRuntimesByOrder(ctx, agent.WorkspaceID)
	if err != nil {
		slog.Error("webhook: list runtimes for placement", "err", err, "task_id", taskID)
		return true
	}
	runningOn := func(id pgtype.UUID) int64 {
		n, err := s.Queries.CountRunningTasksOnRuntime(ctx, id)
		if err != nil {
			slog.Warn("webhook: count running tasks on runtime", "err", err, "runtime_id", util.UUIDToString(id))
			return 0
		}
		return n
	}
	target, offHome, ok := choosePlacement(home, ordered, agent.FallbackRuntimeIds, runningOn, time.Now())
	if !ok {
		slog.Info("webhook: no runtime for the task yet, it waits",
			"task_id", taskID, "home", runtimeDisplayName(home),
			"home_available", runtimeAvailable(home, time.Now()),
			"fallbacks", len(agent.FallbackRuntimeIds))
		return true
	}
	var from *db.AgentRuntime
	if offHome {
		from = &home
	}
	s.dispatchWebhookTask(ctx, task, target, agent, from)
	return true
}

// DrainWebhookQueue places every queued webhook task it can — the
// workspace-wide replacement for the per-agent drain (P11). Called where a
// slot may have freed (CompleteTask, FailTaskWithOutput,
// CancelTaskWithResult, a refused dispatch) and from every sweeper tick,
// which is what re-places a waiting run when a cool-down expires, a probe
// flips a runtime up, or a cap is raised. Returns how many were dispatched.
func (s *TaskService) DrainWebhookQueue(ctx context.Context) int {
	if os.Getenv("MULTICA_WEBHOOK_RUNTIME") != "1" {
		return 0
	}
	tasks, err := s.Queries.FindQueuedWebhookTasks(ctx)
	if err != nil {
		slog.Warn("webhook: list queued tasks for the drain", "err", err)
		return 0
	}
	// The query's per-(issue, agent) serialisation holds at query time; an
	// earlier iteration of THIS drain may have dispatched a sibling, so the
	// same key is kept here and a second task on it waits for the next drain.
	placed := 0
	taken := map[string]bool{}
	for _, task := range tasks {
		key := serialisationKey(task)
		if taken[key] {
			continue
		}
		if !s.placeWebhookTask(ctx, task) {
			continue
		}
		if after, err := s.Queries.GetAgentTask(ctx, task.ID); err == nil && after.Status == "dispatched" {
			placed++
			taken[key] = true
		}
	}
	return placed
}

// serialisationKey mirrors FindQueuedWebhookTasks' NOT EXISTS: one active
// task per (agent, issue), per (agent, chat session), or per agent for the
// bare quick-create case.
func serialisationKey(t db.AgentTaskQueue) string {
	agent := util.UUIDToString(t.AgentID)
	switch {
	case t.IssueID.Valid:
		return agent + "|issue|" + util.UUIDToString(t.IssueID)
	case t.ChatSessionID.Valid:
		return agent + "|chat|" + util.UUIDToString(t.ChatSessionID)
	case t.AutopilotRunID.Valid:
		return agent + "|autopilot|" + util.UUIDToString(t.ID)
	default:
		return agent + "|bare"
	}
}

// alternativeRuntimeAvailable reports whether some runtime in the agent's
// fallback set, other than failedRuntimeID, is available now — the retry
// schedule's question for a dispatch_timeout (design §3, Retries). The
// failed runtime is already in its cool-down when this is asked.
func (s *TaskService) alternativeRuntimeAvailable(ctx context.Context, agent db.Agent, failedRuntimeID pgtype.UUID) bool {
	if len(agent.FallbackRuntimeIds) == 0 {
		return false
	}
	ordered, err := s.Queries.ListWebhookRuntimesByOrder(ctx, agent.WorkspaceID)
	if err != nil {
		return false
	}
	now := time.Now()
	failed := util.UUIDToString(failedRuntimeID)
	for _, r := range ordered {
		if util.UUIDToString(r.ID) == failed || !inFallbackSet(r.ID, agent.FallbackRuntimeIds) {
			continue
		}
		if runtimeAvailable(r, now) {
			return true
		}
	}
	return false
}
