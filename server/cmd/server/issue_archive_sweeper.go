package main

import (
	"context"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
)

// issueArchiveSweepInterval is how often the archive sweeper runs. The
// threshold it applies (ISSUE_ARCHIVE_AFTER, 72h by default) is days, so a
// quarter-hour cadence is plenty; the constant is not configurable on
// purpose — the threshold is the knob.
const issueArchiveSweepInterval = 15 * time.Minute

// runIssueArchiveSweeper moves cards that have sat in done/cancelled for
// longer than olderThan to archived (service.ArchiveStaleTerminalIssues).
// The first tick runs immediately so a restart never delays an overdue
// archive; then one tick per interval until ctx is done. Started from
// main.go beside runRuntimeSweeper, gated by ISSUE_ARCHIVE_SWEEP_ENABLED.
func runIssueArchiveSweeper(ctx context.Context, taskSvc *service.TaskService, olderThan time.Duration) {
	taskSvc.ArchiveStaleTerminalIssues(ctx, olderThan)

	ticker := time.NewTicker(issueArchiveSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			taskSvc.ArchiveStaleTerminalIssues(ctx, olderThan)
		}
	}
}
