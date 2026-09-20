package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Runtime availability (dev-command-center design
// 2026-09-20-runtime-placement §2): two pure predicates over an
// agent_runtime row, and the writers of down_until.
//
// down_until is the ONLY availability state a webhook runtime has (P3), and
// it has exactly three writers, each naming a reason:
//
//   - the availability probe (ProbeRuntimeAvailability, runtime_probe.go),
//     every other sweeper tick, writing a two-minute window;
//   - a refused dispatch — the receiver's 409 runtime_unavailable
//     (webhook_task_dispatch.go), writing a five-minute cool-down;
//   - a run that never started — a webhook task failed with
//     dispatch_timeout (markRuntimeDownForTask), five minutes.
//
// Nothing else may mark a runtime down, and in particular nothing marks one
// down on a transport error: the probe fails OPEN, because a translator that
// cannot answer must not take every runtime out.

const (
	// runtimeProbeDownWindow is twice the probe interval: a runtime stays
	// down continuously while the probe keeps saying so, and comes back
	// within a minute of a runner appearing — or of the probe stopping.
	runtimeProbeDownWindow = 2 * time.Minute
	// runtimeCoolDown is the owner's number (2026-09-20) for the two
	// dispatch-side writers: a refused dispatch and a never-started run.
	runtimeCoolDown = 5 * time.Minute
	// dispatchTimeoutDownReason is the third writer's reason text.
	dispatchTimeoutDownReason = "a run never started (dispatch_timeout)"
)

// runtimeAvailable is available(r): a webhook runtime that is in the
// rotation (cap not 0) and not in a cool-down. Pure.
func runtimeAvailable(r db.AgentRuntime, now time.Time) bool {
	if r.RuntimeMode != "webhook" {
		return false
	}
	if r.MaxConcurrentTasks.Valid && r.MaxConcurrentTasks.Int32 == 0 {
		return false
	}
	if r.DownUntil.Valid && r.DownUntil.Time.After(now) {
		return false
	}
	return true
}

// runtimeFree is free(r): no cap, or fewer runs on it than the cap. The
// count is the caller's (CountRunningTasksOnRuntime). Pure.
func runtimeFree(r db.AgentRuntime, running int64) bool {
	if !r.MaxConcurrentTasks.Valid {
		return true
	}
	return running < int64(r.MaxConcurrentTasks.Int32)
}

// markRuntimeDown writes down_until and its reason. Best-effort: a failed
// write is logged, and the next writer (or the next probe) tries again.
func (s *TaskService) markRuntimeDown(ctx context.Context, runtimeID pgtype.UUID, until time.Time, reason string) {
	if err := s.Queries.MarkRuntimeDown(ctx, db.MarkRuntimeDownParams{
		ID:         runtimeID,
		DownUntil:  pgtype.Timestamptz{Time: until, Valid: true},
		DownReason: pgtype.Text{String: reason, Valid: true},
	}); err != nil {
		slog.Warn("runtime placement: mark runtime down", "err", err, "runtime_id", util.UUIDToString(runtimeID))
		return
	}
	slog.Info("runtime placement: runtime marked down", "runtime_id", util.UUIDToString(runtimeID), "until", until, "reason", reason)
}

// markRuntimeUp clears the cool-down and stamps the check time.
func (s *TaskService) markRuntimeUp(ctx context.Context, runtimeID pgtype.UUID) {
	if err := s.Queries.MarkRuntimeUp(ctx, runtimeID); err != nil {
		slog.Warn("runtime placement: mark runtime up", "err", err, "runtime_id", util.UUIDToString(runtimeID))
	}
}

// markRuntimeDownForTask is the third writer: a webhook task that failed
// with dispatch_timeout — GitHub never started its run — marks the runtime
// it was placed on down for the cool-down. Called from the sweeper's
// HandleFailedWebhookTasks and from FailTaskWithOutput (which is how the
// translator's setup watch reports a run it cancelled). No-op for any other
// reason, or for a task with no runtime.
func (s *TaskService) markRuntimeDownForTask(ctx context.Context, t db.AgentTaskQueue) {
	if !t.FailureReason.Valid || t.FailureReason.String != "dispatch_timeout" || !t.RuntimeID.Valid {
		return
	}
	s.markRuntimeDown(ctx, t.RuntimeID, time.Now().Add(runtimeCoolDown), dispatchTimeoutDownReason)
}
