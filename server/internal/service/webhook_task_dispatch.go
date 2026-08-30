package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// webhookCallbackTokenTTL is the lifetime of the per-task JWT the receiver
// uses on /messages, /usage, /complete, /fail. 60 minutes covers the
// GH Actions 30-min job ceiling with comfortable margin while still rotating
// often enough that a leaked token has bounded value.
const webhookCallbackTokenTTL = 60 * time.Minute

// webhookDispatchTimeout caps each individual POST attempt. DispatchWithRetry
// makes 3 attempts so the total worst-case wait is ~3 × (5s timeout) + backoffs
// ≈ 20s, kept off the request-creating goroutine.
const webhookDispatchTimeout = 5 * time.Second

// webhookCallbackBaseURL is read from env so the URL the receiver calls back
// to can differ from whatever URL Multica was reached on (e.g. external
// hostname when Multica is behind a tunnel). Falls back to MULTICA_PUBLIC_URL
// then a sensible default that's only useful for unit tests.
func webhookCallbackBaseURL() string {
	if v := os.Getenv("MULTICA_WEBHOOK_CALLBACK_URL"); v != "" {
		return v
	}
	if v := os.Getenv("MULTICA_PUBLIC_URL"); v != "" {
		return v + "/api/daemon"
	}
	return "http://localhost:8080/api/daemon"
}

// webhookAgentData mirrors handler.TaskAgentData for the dispatch payload.
// Defined in the service package to avoid a circular import from handler.
// Field names and JSON tags match the daemon claim response exactly so
// the receiver's jq / Go parsing works identically for both paths.
type webhookAgentData struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Instructions string            `json:"instructions"`
	Skills       []AgentSkillData  `json:"skills,omitempty"`
	CustomEnv    map[string]string `json:"custom_env,omitempty"`
	CustomArgs   []string          `json:"custom_args,omitempty"`
	McpConfig    json.RawMessage   `json:"mcp_config,omitempty"`
	Model        string            `json:"model,omitempty"`
}

// buildWebhookAgentData loads the agent's configuration fields and skills.
// Mirrors daemon.go:1209-1236 but lives in the service package.
func (s *TaskService) buildWebhookAgentData(ctx context.Context, agent db.Agent) webhookAgentData {
	data := webhookAgentData{
		ID:           util.UUIDToString(agent.ID),
		Name:         agent.Name,
		Instructions: agent.Instructions,
		Model:        agent.Model.String,
	}

	if agent.CustomEnv != nil {
		if err := json.Unmarshal(agent.CustomEnv, &data.CustomEnv); err != nil {
			slog.Warn("webhook: unmarshal custom_env", "agent_id", data.ID, "err", err)
		}
	}
	if agent.CustomArgs != nil {
		if err := json.Unmarshal(agent.CustomArgs, &data.CustomArgs); err != nil {
			slog.Warn("webhook: unmarshal custom_args", "agent_id", data.ID, "err", err)
		}
	}
	if agent.McpConfig != nil {
		data.McpConfig = json.RawMessage(agent.McpConfig)
	}

	data.Skills = s.LoadAgentSkills(ctx, agent.ID)
	return data
}

// webhookIssueData carries the issue context the receiver needs to build a
// useful prompt. The daemon path relies on `multica issue get <id>` (CLI) to
// fetch this; webhook runtimes don't have the CLI, so we inline the data.
type webhookIssueData struct {
	ID          string               `json:"id"`
	Title       string               `json:"title"`
	Description string               `json:"description,omitempty"`
	Status      string               `json:"status"`
	Priority    string               `json:"priority"`
	Comments    []webhookCommentData `json:"comments,omitempty"`
}

type webhookCommentData struct {
	AuthorType string `json:"author_type"`
	Content    string `json:"content"`
	CreatedAt  string `json:"created_at"`
}

// webhookMaxIssueComments caps the number of comments included in the
// dispatch payload to keep the payload size bounded. 20 most recent
// covers the active conversation without blowing up on long-running issues.
const webhookMaxIssueComments = 20

// buildWebhookIssueData loads the issue and its recent comments.
func (s *TaskService) buildWebhookIssueData(ctx context.Context, task db.AgentTaskQueue) *webhookIssueData {
	if !task.IssueID.Valid {
		return nil
	}
	issue, err := s.Queries.GetIssue(ctx, task.IssueID)
	if err != nil {
		slog.Warn("webhook: load issue for context", "issue_id", util.UUIDToString(task.IssueID), "err", err)
		return nil
	}

	data := &webhookIssueData{
		ID:          util.UUIDToString(issue.ID),
		Title:       issue.Title,
		Description: issue.Description.String,
		Status:      issue.Status,
		Priority:    issue.Priority,
	}

	comments, err := s.Queries.ListCommentsForIssue(ctx, db.ListCommentsForIssueParams{
		IssueID:     issue.ID,
		WorkspaceID: issue.WorkspaceID,
		Limit:       webhookMaxIssueComments,
	})
	if err != nil {
		slog.Warn("webhook: load issue comments", "issue_id", data.ID, "err", err)
		return data
	}
	for _, c := range comments {
		data.Comments = append(data.Comments, webhookCommentData{
			AuthorType: c.AuthorType,
			Content:    c.Content,
			CreatedAt:  c.CreatedAt.Time.Format("2006-01-02T15:04:05Z"),
		})
	}
	return data
}

// webhookPullRequestData carries linked PR metadata so the receiver can
// tell the agent which branch/commits to review. Sourced from the
// github_pull_request table via ListPullRequestsByIssue.
type webhookPullRequestData struct {
	Number       int32  `json:"number"`
	Title        string `json:"title"`
	State        string `json:"state"`
	HTMLURL      string `json:"html_url"`
	Branch       string `json:"branch,omitempty"`
	HeadSHA      string `json:"head_sha,omitempty"`
	RepoOwner    string `json:"repo_owner"`
	RepoName     string `json:"repo_name"`
	Additions    int32  `json:"additions"`
	Deletions    int32  `json:"deletions"`
	ChangedFiles int32  `json:"changed_files"`
}

// buildWebhookPullRequests loads the linked PRs for an issue.
func (s *TaskService) buildWebhookPullRequests(ctx context.Context, issueID pgtype.UUID) []webhookPullRequestData {
	if !issueID.Valid {
		return nil
	}
	rows, err := s.Queries.ListPullRequestsByIssue(ctx, issueID)
	if err != nil {
		slog.Warn("webhook: load linked PRs", "issue_id", util.UUIDToString(issueID), "err", err)
		return nil
	}
	if len(rows) == 0 {
		return nil
	}
	prs := make([]webhookPullRequestData, 0, len(rows))
	for _, r := range rows {
		prs = append(prs, webhookPullRequestData{
			Number:       r.PrNumber,
			Title:        r.Title,
			State:        r.State,
			HTMLURL:      r.HtmlUrl,
			Branch:       r.Branch.String,
			HeadSHA:      r.HeadSha,
			RepoOwner:    r.RepoOwner,
			RepoName:     r.RepoName,
			Additions:    r.Additions,
			Deletions:    r.Deletions,
			ChangedFiles: r.ChangedFiles,
		})
	}
	return prs
}

// webhookHTTPClient is the http.Client used for outbound dispatches. Exposed
// as a package variable so tests can swap in an httptest.NewServer-aware
// transport, but immutable in production.
var webhookHTTPClient = &http.Client{Timeout: webhookDispatchTimeout}

// MaybeDispatchToWebhook checks whether the task's runtime is webhook-mode
// and — if so — fires a webhook dispatch in a background goroutine. Returns
// true when a dispatch was started (caller should skip the normal
// notifyTaskAvailable wakeup), false when the runtime is local and the
// existing daemon-claim path should be taken instead.
//
// The dispatch is fire-and-forget at the goroutine level: failures are
// logged but don't propagate back to the request that created the task.
// DispatchWithRetry's exponential backoff handles transient receiver
// outages; permanent failures land in the task's failure_reason via a
// downstream queue-failer (left for Task A10, currently the task just sits
// claimed-but-unstarted until the operator intervenes — same failure mode
// as a daemon that's claimed a task then crashed).
//
// Disabled by default; set MULTICA_WEBHOOK_RUNTIME=1 to enable. With the
// flag off, webhook runtimes can still be registered but dispatch is a
// no-op (the task stays queued forever) — useful for staging where you
// want the schema in place but don't yet want the side-effects.
func (s *TaskService) MaybeDispatchToWebhook(ctx context.Context, task db.AgentTaskQueue) bool {
	if os.Getenv("MULTICA_WEBHOOK_RUNTIME") != "1" {
		return false
	}

	runtime, err := s.Queries.GetAgentRuntime(ctx, task.RuntimeID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("webhook: load runtime", "err", err, "task_id", util.UUIDToString(task.ID))
		}
		return false
	}
	if runtime.RuntimeMode != "webhook" {
		return false
	}
	if !runtime.WebhookUrl.Valid || runtime.WebhookUrl.String == "" {
		slog.Error("webhook: runtime in mode=webhook but no webhook_url", "runtime_id", util.UUIDToString(runtime.ID))
		return false
	}

	agent, err := s.Queries.GetAgent(ctx, task.AgentID)
	if err != nil {
		slog.Error("webhook: load agent for capacity check", "err", err, "task_id", util.UUIDToString(task.ID))
		return false
	}
	running, err := s.Queries.CountRunningTasks(ctx, task.AgentID)
	if err != nil {
		slog.Error("webhook: count running tasks", "err", err, "task_id", util.UUIDToString(task.ID))
		return false
	}
	if running >= int64(agent.MaxConcurrentTasks) {
		slog.Info("webhook: no capacity, task stays queued",
			"task_id", util.UUIDToString(task.ID),
			"agent_id", util.UUIDToString(task.AgentID),
			"running", running, "max", agent.MaxConcurrentTasks)
		return true
	}

	s.dispatchWebhookTask(ctx, task, runtime, agent)
	return true
}

// dispatchWebhookTask transitions a queued task to dispatched and fires
// the webhook POST in a background goroutine. Extracted from
// MaybeDispatchToWebhook so MaybeDispatchNextQueuedWebhookTask can
// reuse the same dispatch logic.
func (s *TaskService) dispatchWebhookTask(ctx context.Context, task db.AgentTaskQueue, runtime db.AgentRuntime, agent db.Agent) {
	taskID := util.UUIDToString(task.ID)
	runtimeID := util.UUIDToString(runtime.ID)

	dispatched, err := s.Queries.DispatchAgentTask(ctx, task.ID)
	if err != nil {
		slog.Error("webhook: set task dispatched", "err", err, "task_id", taskID)
		return
	}

	// Move the card off its promotable statuses (see promotableStatuses) the
	// moment the run actually starts, so the board reflects work in progress
	// instead of waiting for completion. Best-effort: MarkIssueRunning never
	// errors, and dispatch has already happened above regardless of what it
	// does.
	if dispatched.IssueID.Valid {
		if issue, err := s.Queries.GetIssue(ctx, dispatched.IssueID); err != nil {
			slog.Error("webhook: load issue for run-start status", "err", err, "task_id", taskID, "issue_id", util.UUIDToString(dispatched.IssueID))
		} else {
			// MarkIssueRunning re-reads this same issue by ID before acting on
			// it. That is deliberate, not a redundant round-trip to delete: the
			// issue loaded here can be stale, and trusting a stale `done` would
			// let this dispatch silently clobber a merge nobody asked to cancel.
			s.MarkIssueRunning(ctx, issue, agent.Name)
		}
	}

	// Record the delivery receipt so completion reconciliation
	// (reconcileCommentsOnCompletion) skips the comments this dispatch conveyed —
	// most importantly the task's own trigger comment. The daemon claim path
	// records this via FinalizeTaskClaim; the webhook path must do the same, or
	// every completion replays the trigger comment as a fresh task, an unbounded
	// duplicate-task loop that burns quota (each new webhook task re-triggers on
	// its own completion). The set mirrors reconcile's planned set: the trigger
	// comment plus any coalesced comment ids, all of which the payload conveys
	// (trigger_comment content + the recent issue-comments context). The subset
	// guard in SetTaskDeliveredCommentIDs permits exactly these ids, and the CAS
	// matches because the task is dispatched with started_at still NULL — this
	// runs before the POST goroutine, so the receiver cannot have started yet.
	if dispatched.TriggerCommentID.Valid || len(dispatched.CoalescedCommentIds) > 0 {
		delivered := make([]pgtype.UUID, 0, len(dispatched.CoalescedCommentIds)+1)
		if dispatched.TriggerCommentID.Valid {
			delivered = append(delivered, dispatched.TriggerCommentID)
		}
		delivered = append(delivered, dispatched.CoalescedCommentIds...)
		if _, err := s.Queries.SetTaskDeliveredCommentIDs(ctx, db.SetTaskDeliveredCommentIDsParams{
			DeliveredCommentIds:      delivered,
			TaskID:                   dispatched.ID,
			RuntimeID:                dispatched.RuntimeID,
			DispatchedAt:             dispatched.DispatchedAt,
			ExpectedTriggerCommentID: dispatched.TriggerCommentID,
		}); err != nil {
			slog.Error("webhook: record delivery receipt", "err", err, "task_id", taskID)
		}
	}

	tok, err := auth.IssueCallbackToken(auth.JWTSecret(), taskID, runtimeID, webhookCallbackTokenTTL)
	if err != nil {
		slog.Error("webhook: issue callback token", "err", err, "task_id", taskID)
		return
	}

	// Build a map from the dispatched task so we can attach agent data
	// without changing the top-level wire shape. json.Marshal + Unmarshal
	// is a one-liner round-trip that keeps the field names in sync with
	// AgentTaskQueue's column tags automatically.
	taskBytes, err := json.Marshal(dispatched)
	if err != nil {
		slog.Error("webhook: marshal task", "err", err, "task_id", taskID)
		return
	}
	var taskMap map[string]any
	if err := json.Unmarshal(taskBytes, &taskMap); err != nil {
		slog.Error("webhook: unmarshal task to map", "err", err, "task_id", taskID)
		return
	}
	taskMap["agent"] = s.buildWebhookAgentData(ctx, agent)

	if issueData := s.buildWebhookIssueData(ctx, dispatched); issueData != nil {
		taskMap["issue"] = issueData
	}
	if prs := s.buildWebhookPullRequests(ctx, dispatched.IssueID); len(prs) > 0 {
		taskMap["pull_requests"] = prs
	}

	// Load trigger comment content if present (matches daemon.go:1349-1365).
	if dispatched.TriggerCommentID.Valid {
		if comment, err := s.Queries.GetComment(ctx, dispatched.TriggerCommentID); err == nil {
			taskMap["trigger_comment"] = map[string]string{
				"content":     comment.Content,
				"author_type": comment.AuthorType,
			}
		}
	}

	payload := map[string]any{
		"task":     taskMap,
		"callback": map[string]any{"url": webhookCallbackBaseURL(), "token": tok},
	}
	target := DispatchTarget{
		URL:       runtime.WebhookUrl.String,
		Secret:    runtime.WebhookSecret.String,
		RuntimeID: runtimeID,
		EventType: runtime.WebhookEventType.String,
	}

	go func() {
		dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		err := DispatchWithRetry(dctx, target, payload, webhookHTTPClient, time.Now,
			RetryPolicy{MaxAttempts: 3, InitialBackoff: 1 * time.Second})
		if err != nil {
			slog.Error("webhook: dispatch failed after retries",
				"err", err, "task_id", taskID, "runtime_id", runtimeID, "url", target.URL)
			return
		}
		slog.Info("webhook: dispatched", "task_id", taskID, "runtime_id", runtimeID)
	}()
}

// MaybeDispatchNextQueuedWebhookTask checks whether the agent has
// capacity for another webhook task after one just completed or failed.
// If a queued task exists and the agent's max_concurrent_tasks allows
// it, the task is dispatched immediately. Called from CompleteTask and
// FailTask so the queue drains without waiting for the next enqueue.
func (s *TaskService) MaybeDispatchNextQueuedWebhookTask(ctx context.Context, agentID pgtype.UUID) {
	if os.Getenv("MULTICA_WEBHOOK_RUNTIME") != "1" {
		return
	}

	agent, err := s.Queries.GetAgent(ctx, agentID)
	if err != nil {
		return
	}
	if !agent.RuntimeID.Valid {
		return
	}

	runtime, err := s.Queries.GetAgentRuntime(ctx, agent.RuntimeID)
	if err != nil || runtime.RuntimeMode != "webhook" {
		return
	}

	running, err := s.Queries.CountRunningTasks(ctx, agentID)
	if err != nil || running >= int64(agent.MaxConcurrentTasks) {
		return
	}

	next, err := s.Queries.FindOldestQueuedTaskForAgent(ctx, agentID)
	if err != nil {
		return
	}

	slog.Info("webhook: draining queue after completion",
		"task_id", util.UUIDToString(next.ID),
		"agent_id", util.UUIDToString(agentID))
	s.dispatchWebhookTask(ctx, next, runtime, agent)
}
