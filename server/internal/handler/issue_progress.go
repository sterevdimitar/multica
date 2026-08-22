package handler

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ----------------------------------------------------------------------
// GET /api/issues/{id}/progress
//
// A purpose-built, READ-ONLY projection behind the ticket-progress popover.
// It must not widen into a general task API: there is deliberately no
// GET /api/tasks/{id}, and failure_reason stays write-only over the public
// API. Nothing in this file writes — two SELECT paths and a clock.
//
// Kept in its own file so the fork patch survives upstream rebases cleanly.
// ----------------------------------------------------------------------

// expectedStepsHopCap bounds the NEXT_AGENT walk. A misconfigured chain
// should degrade to "no expected steps", never to an unbounded query loop.
const expectedStepsHopCap = 10

// readinessAgentName is dispatched by fix-pr-watch rather than by a
// NEXT_AGENT edge, so it never appears in the walk and is appended last.
const readinessAgentName = "readiness-agent"

// IssueProgressTokens is the display vocabulary: CacheCreation is the DB's
// cache_write_tokens. Cache READS are reported but excluded from the
// headline number client-side.
type IssueProgressTokens struct {
	Input         int64 `json:"input"`
	Output        int64 `json:"output"`
	CacheCreation int64 `json:"cache_creation"`
	CacheRead     int64 `json:"cache_read"`
}

type IssueProgressTask struct {
	TaskID      string              `json:"task_id"`
	AgentName   string              `json:"agent_name"`
	Status      string              `json:"status"`
	QueuedAt    time.Time           `json:"queued_at"`
	StartedAt   *time.Time          `json:"started_at"`
	CompletedAt *time.Time          `json:"completed_at"`
	Tokens      IssueProgressTokens `json:"tokens"`
	Turns       int64               `json:"turns"`
	IsLive      bool                `json:"is_live"`
}

type IssueProgressResponse struct {
	Tasks []IssueProgressTask `json:"tasks"` // [] never null
	// ExpectedSteps is nil (JSON null) whenever the chain could not be
	// derived — the UI then omits "step N of M" and the pending rows rather
	// than showing a guess.
	ExpectedSteps []string  `json:"expected_steps"`
	ServerNow     time.Time `json:"server_now"`
}

// liveTaskStatuses are the statuses that mean "this step has not finished".
var liveTaskStatuses = map[string]bool{
	"queued":     true,
	"dispatched": true,
	"running":    true,
}

// GetIssueProgress serves the per-step progress projection for one issue.
// Auth and validation are identical to GetIssueUsage — same middleware
// chain, no new auth surface.
func (h *Handler) GetIssueProgress(w http.ResponseWriter, r *http.Request) {
	issueID := chi.URLParam(r, "id")
	issue, ok := h.loadIssueForUser(w, r, issueID)
	if !ok {
		return
	}

	rows, err := h.Queries.ListTaskProgressByIssue(r.Context(), issue.ID)
	if err != nil {
		slog.Warn("list task progress failed", "issue_id", issueID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to get issue progress")
		return
	}

	tasks := make([]IssueProgressTask, 0, len(rows))
	for _, row := range rows {
		tasks = append(tasks, IssueProgressTask{
			TaskID:      uuidToString(row.TaskID),
			AgentName:   row.AgentName,
			Status:      row.Status,
			QueuedAt:    row.CreatedAt.Time,
			StartedAt:   timestampPtr(row.StartedAt),
			CompletedAt: timestampPtr(row.CompletedAt),
			Tokens: IssueProgressTokens{
				Input:         row.InputTokens,
				Output:        row.OutputTokens,
				CacheCreation: row.CacheWriteTokens,
				CacheRead:     row.CacheReadTokens,
			},
			Turns:  row.NumTurns,
			IsLive: liveTaskStatuses[row.Status],
		})
	}

	writeJSON(w, http.StatusOK, IssueProgressResponse{
		Tasks:         tasks,
		ExpectedSteps: h.deriveExpectedSteps(r.Context(), uuidToString(issue.WorkspaceID), issue.ID),
		ServerNow:     time.Now().UTC(),
	})
}

// timestampPtr turns a nullable timestamptz into a *time.Time so an unstarted
// or unfinished step serializes as JSON null rather than the zero instant.
func timestampPtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time.UTC()
	return &t
}

// deriveExpectedSteps walks the nominal agent chain, best effort.
//
//	autopilot assignee (must be an agent) → repeat: append agent.name,
//	next := custom_env["NEXT_AGENT"]; "" terminates.
//
// Guards: a seen-set (cycle → nil), a hop cap (→ nil), and an unresolvable
// name (→ nil). Any failure returns nil, and the UI degrades to "no pending
// rows, no step N of M" rather than showing a wrong chain.
//
// It reads custom_env server-side but EXPOSES RESOLVED NAMES ONLY — never an
// env key or value. custom_env can hold secrets; the fork masks it behind
// owner/admin-gated endpoints for exactly that reason.
func (h *Handler) deriveExpectedSteps(ctx context.Context, workspaceID string, issueID pgtype.UUID) []string {
	wsUUID := parseUUID(workspaceID)
	if !wsUUID.Valid {
		return nil
	}

	assignee, err := h.Queries.GetAutopilotAssigneeForIssue(ctx, issueID)
	if err != nil {
		return nil // no autopilot run on this issue — nothing nominal to show
	}
	if assignee.AssigneeType != "agent" || !assignee.AssigneeID.Valid {
		return nil // squad or member assignee: no single chain entry point
	}

	agent, err := h.Queries.GetAgent(ctx, assignee.AssigneeID)
	if err != nil {
		return nil
	}

	steps := make([]string, 0, expectedStepsHopCap)
	seen := map[string]bool{}
	for hop := 0; ; hop++ {
		if hop >= expectedStepsHopCap {
			return nil
		}
		if seen[agent.Name] {
			return nil // cycle
		}
		seen[agent.Name] = true
		steps = append(steps, agent.Name)

		next := unmarshalCustomEnv(agent)["NEXT_AGENT"]
		if next == "" {
			break
		}
		agent, err = h.Queries.GetAgentByNameInWorkspace(ctx, db.GetAgentByNameInWorkspaceParams{
			WorkspaceID: wsUUID,
			Name:        next,
		})
		if err != nil {
			return nil // NEXT_AGENT names an agent that does not exist here
		}
	}

	// The readiness judge is dispatched by fix-pr-watch, not by a chain
	// edge, so it is appended when it exists and has not already been walked.
	if !seen[readinessAgentName] {
		if _, err := h.Queries.GetAgentByNameInWorkspace(ctx, db.GetAgentByNameInWorkspaceParams{
			WorkspaceID: wsUUID,
			Name:        readinessAgentName,
		}); err == nil {
			steps = append(steps, readinessAgentName)
		}
	}

	return steps
}
