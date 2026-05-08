# Webhook Runtime — Design Note

Background and decision record for the `feature/webhook-runtime` patch. Written before code; updated as choices solidify. The implementation plan that drives this work lives outside this fork at `dev-command-center/docs/superpowers/plans/2026-05-08-multica-webhook-runtime.md`.

## 1. Goal

Add a generic `RuntimeMode='webhook'` to `agent_runtime`, with `webhook_url`, `webhook_secret`, `webhook_event_type` columns. On task assignment to such a runtime, the server POSTs the same payload `DaemonClaim` would have returned (with HMAC signature) to the configured URL. The receiver streams progress and final results back through the unchanged `/api/daemon/tasks/{taskId}/{start|messages|usage|complete|fail}` endpoints, authenticated with a per-task callback token (JWT, 60 min TTL) issued at dispatch time.

The point of the patch is: **anywhere that can accept an HTTP POST and call back into Multica's existing daemon endpoints can host an agent task** — GitHub Actions, Cloud Run jobs, AWS Batch, GitLab CI. Multica stays generic; the URL handler is the substrate-specific glue.

## 2. Findings from the source

### 2.1 `AgentRuntime` model

`server/pkg/db/generated/models.go:46`:

```go
type AgentRuntime struct {
    ID             pgtype.UUID
    WorkspaceID    pgtype.UUID
    DaemonID       pgtype.Text
    Name           string
    RuntimeMode    string
    Provider       string
    Status         string
    DeviceInfo     string
    Metadata       []byte
    LastSeenAt     pgtype.Timestamptz
    CreatedAt      pgtype.Timestamptz
    UpdatedAt      pgtype.Timestamptz
    OwnerID        pgtype.UUID
    LegacyDaemonID pgtype.Text
}
```

### 2.2 Existing schema constraint

Migration `004_agent_runtime_loop.up.sql` created:

```sql
runtime_mode TEXT NOT NULL CHECK (runtime_mode IN ('local', 'cloud')),
```

Note: the table is **`agent_runtime` (singular)**, not `agent_runtimes`. The auto-generated CHECK constraint name follows Postgres convention `agent_runtime_runtime_mode_check`, but our migration drops it dynamically via a DO block to be robust against historical renames.

### 2.3 `DaemonRegisterRequest`

`server/internal/handler/daemon.go:126`:

```go
type DaemonRegisterRequest struct {
    WorkspaceID     string   `json:"workspace_id"`
    DaemonID        string   `json:"daemon_id"`
    LegacyDaemonIDs []string `json:"legacy_daemon_ids"`
    DeviceName      string   `json:"device_name"`
    CLIVersion      string   `json:"cli_version"`
    LaunchedBy      string   `json:"launched_by"`
    Runtimes        []struct {
        Name    string `json:"name"`
        Type    string `json:"type"`
        Version string `json:"version"`
        Status  string `json:"status"`
    } `json:"runtimes"`
}
```

### 2.4 Server hardcodes RuntimeMode

`server/internal/handler/daemon.go:293` inside `DaemonRegister`:

```go
row, err := h.Queries.UpsertAgentRuntime(r.Context(), db.UpsertAgentRuntimeParams{
    WorkspaceID: wsUUID,
    DaemonID:    strToText(req.DaemonID),
    Name:        name,
    RuntimeMode: "local",  // <— hardcoded; the request's runtime_mode is ignored
    Provider:    provider,
    ...
})
```

The patch lifts this so a webhook caller can register itself with `runtime_mode: "webhook"` and the dispatch fields.

### 2.5 Hook point — `EnqueueTaskFor*`

`server/internal/service/task.go`:

- `EnqueueTaskForIssue` (line 104) → `enqueueIssueTask` (line 117) — primary creation path.
- `EnqueueTaskForMention` (line 171) — comment-mention path.
- `EnqueueQuickCreateTask` (line 226) — natural-language quick-create.

All three converge on:

```go
task, err := s.Queries.CreateAgentTask(...)
...
s.broadcastTaskEvent(ctx, protocol.EventTaskQueued, task)
s.notifyTaskAvailable(task)   // ← in-process channel that triggers daemon claim
return task, nil
```

`notifyTaskAvailable` is the wakeup that local daemons subscribe to; webhook runtimes don't subscribe to it. **The dispatch hook lives between `broadcastTaskEvent` and `notifyTaskAvailable`**: if the task's runtime is webhook-mode, fire the dispatcher (in a goroutine, off the request path) and skip `notifyTaskAvailable` for that runtime.

The check is cheap because `agent.RuntimeID` is already loaded by the existing flow — we extend the load to also fetch `RuntimeMode` (or load the runtime row separately at this point, depending on what's cleaner against the existing query layer).

### 2.6 Claim payload — `AgentTaskResponse`

`server/internal/handler/agent.go:136` — 30+ field struct that `DaemonClaim` returns. Key fields the webhook receiver will need: `ID`, `AgentID`, `RuntimeID`, `Agent.{Instructions, McpConfig, CustomEnv, CustomArgs, Model}`, `WorkDir`, `PriorSessionID`, `TriggerCommentContent`, `AutopilotDescription`, `ChatMessage`, `Repos`, `ProjectResources`.

The builder is `taskToResponse` at `agent.go:192`, in the `handler` package. **Refactor required (small):** move `taskToResponse` (or extract its logic) into a package the `service` layer can call — likely `server/internal/service/taskpayload.go` or `server/pkg/protocol/`. Without this refactor the dispatcher would have an awkward circular dependency with `handler`. The plan handles this in Task A9 Step 4.

## 3. Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Storage of webhook config | Three new columns on `agent_runtime` (not a sidecar table) | Single-row upsert during register matches existing pattern; webhook fields are NULL for non-webhook rows. |
| Existing `runtime_mode` constraint | Drop, recreate with `IN ('local', 'cloud', 'webhook')` | Done in 079; preserves backward-compat for existing 'local'/'cloud' rows. |
| Dispatch trigger | Extension of `enqueueIssueTask` (and the mention/quick-create siblings) — synchronous DB commit, async (goroutine) HTTP POST | Avoids blocking the originating request on a 5 s outbound; keeps the existing transaction shape. |
| Skip `notifyTaskAvailable` for webhook runtimes | Yes | They don't poll; firing the in-process wakeup is wasted noise. |
| Payload shape | Existing `AgentTaskResponse` wrapped in `{"task": …, "callback": {"url": …, "token": …}}` | Reuses the canonical claim shape verbatim; the receiver matches the daemon's expectations 1:1. |
| Signature | `X-Multica-Signature: sha256=<hex>` over `timestamp + "." + body` with `webhook_secret` | Stripe-style; binds the signature to a timestamp, defeats body-replay-with-fresh-timestamp. |
| Replay window | 5 minutes | Standard. |
| Callback token | HMAC-signed JWT (HS256), claims `{ task_id, runtime_id, exp = now+60min, sub = 'multica-webhook-callback' }` | Same secret as existing JWT issuance (reuse the project's signing key). 60 min covers GH Actions' 30-minute job ceiling with margin. |
| Where the JWT secret comes from | The same secret that signs existing user JWTs (from server config) | Avoids a separate secret to manage; auth middleware is already wired to verify HS256 against this key. |
| Feature flag | `MULTICA_WEBHOOK_RUNTIME=1` env var, default off | Lets the patch ship without changing existing behavior. |
| `/api/daemon/runtimes/{id}/tasks/claim` for webhook runtimes | Returns 405 Method Not Allowed | A webhook runtime calling `claim` is a config bug worth surfacing. |
| Retry on dispatch failure | 3 attempts, 1s/2s/4s exponential backoff. On exhaustion, mark task failed with reason `webhook-dispatch-failed` | Bounded; explicit failure beats silent loss. |
| Required refactor | Extract `taskToResponse` or its logic to a service-callable location | Service layer needs to build the same payload Handler builds; current placement creates a circular import. |

## 4. Schema changes (079)

```sql
-- 079_webhook_runtime.up.sql
DO $$
DECLARE cname text;
BEGIN
    SELECT conname INTO cname
    FROM pg_constraint
    WHERE conrelid = 'agent_runtime'::regclass
      AND contype = 'c'
      AND pg_get_constraintdef(oid) LIKE '%runtime_mode%IN%';
    IF cname IS NOT NULL THEN
        EXECUTE 'ALTER TABLE agent_runtime DROP CONSTRAINT ' || quote_ident(cname);
    END IF;
END $$;

ALTER TABLE agent_runtime
    ADD CONSTRAINT agent_runtime_runtime_mode_check
        CHECK (runtime_mode IN ('local', 'cloud', 'webhook'));

ALTER TABLE agent_runtime
    ADD COLUMN webhook_url        TEXT,
    ADD COLUMN webhook_secret     TEXT,
    ADD COLUMN webhook_event_type TEXT;

ALTER TABLE agent_runtime
    ADD CONSTRAINT agent_runtime_webhook_url_required
        CHECK (runtime_mode <> 'webhook' OR webhook_url IS NOT NULL);
```

## 5. Out of scope for this patch

- **Pluggable receiver protocols.** This patch defines one shape (HMAC + JSON envelope). If we ever want the same hook to fire SQS/Pub-Sub/etc., that's a separate change.
- **Webhook runtime priority/fallback routing.** Already on the backlog (patch #4). If a webhook runtime is offline, today's behavior is "task is stuck"; the priority field will let "prefer webhook, fall back to local daemon" work cleanly.
- **Translator / GitHub Actions wiring.** Lives in `dev-command-center`, not in this fork. The fork stays GitHub-agnostic.

## 6. Open questions

- **Does the existing per-runtime `Status='online'/'offline'` machinery interact with webhook runtimes?** A webhook runtime has no daemon to heartbeat. Initial answer: register as `online`, never flip — the fact that it's a webhook runtime is enough; the heartbeat field is meaningless. Revisit if it confuses the dashboard.
- **How does this interact with `LegacyDaemonIDs` merge logic in `DaemonRegister`?** The merge logic copies state from old daemon-ID rows. For webhook runtimes the daemon_id is a stable application identifier (e.g. `github-actions-runner-pool`), so merge shouldn't fire — but worth a test.
- **Rate limiting on inbound /messages from a webhook runtime.** A misbehaving GH workflow could spam /messages. The existing daemon endpoints already have rate limits; verify they apply to JWT-authenticated requests too. If not, add a per-task rate limit.
