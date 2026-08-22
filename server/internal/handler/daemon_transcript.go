package handler

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The session-transcript archive: upload one run's raw claude session JSONL,
// fetch the newest one for the calling task's own (agent, issue) pair.
//
// Both endpoints sit behind the SAME middleware chain and the SAME
// requireDaemonTaskAccess gate as /complete and /messages, so they
// authenticate with the per-task callback JWT and nothing else.
// MULTICA_PAT gains no new holders.
//
// Invariants that do not surface by running the code:
//
//   - The (agent_id, issue_id) pair is resolved SERVER-SIDE from the task the
//     JWT names, never from the request. A task can therefore never fetch
//     another pair's conversation, no matter what it asks for.
//   - The archive is append-only: a second PUT for the same task inserts a
//     second row. Newest-wins on read makes that harmless and keeps the table
//     a faithful record of what each run produced.
//   - The body is stored VERBATIM as received (gzip). The server never
//     decompresses it: it is opaque bytes to Multica, and a decompression
//     step here would be a zip-bomb surface on a path a runner can call.

// maxTranscriptBytes caps the COMPRESSED upload at 32 MiB. A gzipped session
// JSONL for a 30-turn run is single-digit MiB; 32 MiB is generous headroom
// that still bounds one row's cost.
const maxTranscriptBytes = 32 << 20

// UploadTaskTranscript handles PUT /api/daemon/tasks/{taskId}/transcript.
//
// Body: gzip bytes, stored as-is. Header X-Session-Id is required — a
// transcript whose session it cannot name is unusable for resume, and
// storing it anyway would put a row in the table that the GET can never
// turn into a --resume argument.
func (h *Handler) UploadTaskTranscript(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	task, ok := h.requireDaemonTaskAccess(w, r, taskID)
	if !ok {
		return
	}

	sessionID := r.Header.Get("X-Session-Id")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "X-Session-Id header is required")
		return
	}

	// Read one byte past the cap so an oversize body is detected rather
	// than silently truncated into a corrupt archive row.
	content, err := io.ReadAll(io.LimitReader(r.Body, maxTranscriptBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if len(content) > maxTranscriptBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "transcript exceeds the 32 MiB compressed cap")
		return
	}
	if len(content) == 0 {
		writeError(w, http.StatusBadRequest, "empty transcript body")
		return
	}

	row, err := h.Queries.InsertSessionTranscript(r.Context(), db.InsertSessionTranscriptParams{
		TaskID:       task.ID,
		IssueID:      task.IssueID,
		AgentID:      task.AgentID,
		SessionID:    sessionID,
		Content:      content,
		ContentBytes: int32(len(content)),
	})
	if err != nil {
		slog.Error("transcript upload: insert", "err", err, "task_id", taskID)
		writeError(w, http.StatusInternalServerError, "failed to store transcript")
		return
	}

	slog.Info("transcript archived",
		"task_id", taskID,
		"issue_id", util.UUIDToString(task.IssueID),
		"agent_id", util.UUIDToString(task.AgentID),
		"session_id", sessionID,
		"bytes", row.ContentBytes)
	w.WriteHeader(http.StatusOK)
}

// GetTaskResumeTranscript handles GET
// /api/daemon/tasks/{taskId}/resume-transcript.
//
// Returns the newest archived transcript for the CALLING task's own
// (agent_id, issue_id) pair — resolved from the task row the JWT authorizes,
// never from a query parameter. 404 when the pair has no archive yet, which
// the runner treats as "start a fresh session", never as an error.
func (h *Handler) GetTaskResumeTranscript(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	task, ok := h.requireDaemonTaskAccess(w, r, taskID)
	if !ok {
		return
	}

	row, err := h.Queries.GetLatestSessionTranscriptForAgentIssue(r.Context(),
		db.GetLatestSessionTranscriptForAgentIssueParams{
			AgentID: task.AgentID,
			IssueID: task.IssueID,
		})
	if err != nil {
		if isNotFound(err) || errors.Is(err, io.EOF) {
			writeError(w, http.StatusNotFound, "no archived transcript for this agent and issue")
			return
		}
		slog.Error("resume transcript: query", "err", err, "task_id", taskID)
		writeError(w, http.StatusInternalServerError, "failed to load transcript")
		return
	}

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("X-Session-Id", row.SessionID)
	w.Header().Set("X-Archived-At", row.CreatedAt.Time.UTC().Format(time.RFC3339))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(row.Content); err != nil {
		slog.Warn("resume transcript: write body", "err", err, "task_id", taskID)
	}
}
