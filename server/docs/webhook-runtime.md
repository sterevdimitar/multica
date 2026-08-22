# Webhook Runtime

A **webhook runtime** is an `agent_runtime` row with `runtime_mode='webhook'` and a non-NULL `webhook_url`. Instead of waiting for a daemon to poll `POST /api/daemon/runtimes/{runtimeId}/tasks/claim`, the server **POSTs the task payload** to the configured URL on task assignment. The receiver streams progress and the final result back through the unchanged daemon-callback endpoints, authenticated by a short-lived JWT issued at dispatch time.

This lets agent tasks run on substrates that can't host a long-lived poller — GitHub Actions, Cloud Run jobs, AWS Batch, etc.

## Feature flag

Set `MULTICA_WEBHOOK_RUNTIME=1` in the server environment to enable dispatch. With the flag off, webhook runtimes can still be registered (schema accepts them) but no POSTs fire. This lets the schema/registration paths roll out independently of the side-effects.

## Registration

```http
POST /api/daemon/register
Authorization: Bearer <daemon-token>
Content-Type: application/json

{
  "workspace_id": "...",
  "daemon_id": "github-actions-runner-pool",
  "device_name": "github-actions",
  "runtimes": [{
    "name":               "GitHub Actions",
    "type":               "claude",
    "version":            "1.0.0",
    "status":             "online",
    "runtime_mode":       "webhook",
    "webhook_url":        "https://translator.example.com/v1/dispatch",
    "webhook_secret":     "<32+ random bytes>",
    "webhook_event_type": "multica-task"
  }]
}
```

- `runtime_mode` defaults to `"local"` for backward compat. Allowed values: `local`, `cloud`, `webhook`. Anything else → 400.
- `webhook_url` is required when `runtime_mode='webhook'` (DB CHECK + handler validation). 400 otherwise.
- `webhook_secret` is recommended but optional; without it the dispatcher sends signatures derived from an empty key (verifiable but trivially forgeable — only safe on a private network).
- `webhook_event_type` is optional; surfaces as `X-Multica-Event-Type` on the dispatch.

The webhook runtime appears in the dashboard alongside local daemons with `runtime_mode='webhook'` shown.

## Dispatch payload

POSTed to `webhook_url` on every task assignment to that runtime. Headers:

| Header | Value |
|---|---|
| `Content-Type` | `application/json` |
| `X-Multica-Signature` | `sha256=<hex>` — HMAC-SHA256 over `timestamp + "." + body` with the runtime's `webhook_secret` |
| `X-Multica-Timestamp` | `<unix_seconds>` — receiver must reject if `\|now - ts\| > 300s` (replay protection) |
| `X-Multica-Webhook-Id` | `<runtime_id>` |
| `X-Multica-Event-Type` | `<webhook_event_type>` (only when configured) |

Body:

```json
{
  "task":     <db.AgentTaskQueue serialized — task_id, runtime_id, agent_id, issue_id, etc.>,
  "callback": {
    "url":   "https://multica.example.com/api/daemon",
    "token": "<HS256 JWT, 60min TTL>"
  }
}
```

The callback token's claims:

```json
{
  "sub":        "multica-webhook-callback",
  "task_id":    "...",
  "runtime_id": "...",
  "iat":        <unix>,
  "exp":        <iat + 3600>
}
```

## Receiver responsibilities

The receiver must:

1. **Verify the signature.** Recompute HMAC over `X-Multica-Timestamp + "." + body` with the shared secret. Reject mismatched or stale (>5 min) requests with 401.
2. **Acknowledge fast.** Return 2xx within 5s. Heavy lifting goes in a follow-up worker (e.g. fires a `repository_dispatch` to GitHub then returns 202).
3. **Stream progress.** Each significant event (tool call, intermediate text, errors) POSTs to `{callback.url}/tasks/{task.id}/messages` with the `Authorization: Bearer {callback.token}` header.
4. **Finalize.** When the agent run finishes, POST to `{callback.url}/tasks/{task.id}/complete` (success) or `/fail` (error) with the result/session-id payload. After this the task is terminal.

The full callback endpoint set:

- `POST {callback.url}/tasks/{task_id}/start` — when the agent process actually starts work (distinct from "we received the dispatch")
- `POST {callback.url}/tasks/{task_id}/messages` — streaming events, mapped from claude stream-json
- `POST {callback.url}/tasks/{task_id}/usage` — token usage per provider/model (see below)
- `POST {callback.url}/tasks/{task_id}/complete` — terminal success
- `POST {callback.url}/tasks/{task_id}/fail` — terminal failure

All five endpoints accept the callback JWT in `Authorization: Bearer ...`. The token's `task_id` claim **must** match the URL `task_id` — mismatch is 403.

> **Note on JWT validation:** the middleware change that accepts callback tokens on these endpoints is tracked separately (Task A12); until it lands these endpoints accept only daemon tokens / PATs.

## /usage — the extended payload

`ReportTaskUsage` accepts, per entry, the four token counters plus four additive fields the
GitHub-Actions runner sends. Everything after `cache_write_tokens` is optional; the in-tree
local daemon keeps posting the short shape and its rows land with `num_turns = 0` and NULL
durations.

```json
{
  "usage": [{
    "provider": "anthropic",
    "model": "claude-opus-4-6",
    "input_tokens": 1234,
    "output_tokens": 567,
    "cache_read_tokens": 890123,
    "cache_write_tokens": 4567,
    "num_turns": 7,
    "duration_ms": 463000,
    "duration_api_ms": 401000,
    "total_cost_usd": 1.23
  }]
}
```

The runner POSTs this **twice or more per run**: an incremental flush roughly every 30s while
the agent works, then one authoritative record at exit.

- `num_turns` is always sent — a live count of assistant events on incremental flushes, the
  run-level total on the final one.
- `duration_ms` / `duration_api_ms` / `total_cost_usd` are sent **only on the final POST** and
  are JSON-omitted otherwise. The upsert COALESCEs them, so a late incremental flush can never
  null out a stored final value. Token counts and `num_turns` overwrite instead: they are
  monotone and the final POST is chronologically last.
- No discriminator distinguishes incremental from final. None is needed — `UpsertTaskUsage` is
  last-write-wins on `UNIQUE (task_id, provider, model)`, and the runner's `streampost` exits
  at stdin EOF before `streampost-finalize` runs.
- Storage: `task_usage`, columns added by migration `204_task_usage_turns_duration`.
- **Every accepted ingest publishes a `task:usage` realtime event** (workspace fanout, one
  event per request with the entries summed) carrying `task_id`, `issue_id`, the four token
  counters under display names (`input` / `output` / `cache_creation` / `cache_read`), and
  `turns`. The publish is **skipped** when the task's workspace cannot be resolved: the
  realtime bridge drops any non-`daemon:` event with an empty `WorkspaceID`, so publishing one
  would be a silent no-op rather than a visible failure.
- A caller that fails this POST should log and continue. Usage is observability; it must not be
  able to fail the run it is measuring.

## /claim guard

`POST /api/daemon/runtimes/{runtimeId}/tasks/claim` returns **405 Method Not Allowed** for webhook runtimes. They don't poll, and a caller hitting `/claim` against one is almost certainly a misconfigured client — surfacing it loudly beats silent `{"task": null}`.

## Retry / failure

Dispatch is **3 attempts with exponential backoff** (1s, 2s, 4s). After exhaustion the task stays in `queued` state and the dispatch failure is logged with `task_id` + `runtime_id` + the receiver's last error body. The task is not auto-failed — that's a separate operator decision. (Task A10 will optionally wire this to a terminal `webhook-dispatch-failed` state.)

## Rotation

To rotate `webhook_secret` or change `webhook_url`: re-register the runtime with the new values. The `UpsertAgentRuntime` query treats webhook fields as DO UPDATE columns, so re-registering with the same `(workspace_id, daemon_id, provider)` updates in place.

## Threat model

- **Replay**: timestamp-bound HMAC + 5-minute window.
- **Forgery**: HMAC with a 32-byte+ secret defeats forgery; secret rotates by re-registration.
- **Token theft**: callback JWT is bound to a single `(task_id, runtime_id)` pair and expires in 60 min. A stolen token can only impersonate that one task's status; it can't fetch other tasks.
- **Endpoint confusion**: the JWT's `sub` claim is `multica-webhook-callback`; user/PAT tokens will not pass the callback path's check, and vice versa.
- **Webhook URL takeover**: an attacker with write access to the runtime row could redirect dispatches. The same threat applies to existing `agent_runtime` fields; rely on workspace-membership and daemon-token authorization on register.

## Compatibility

This feature only adds new behavior — `runtime_mode='local'` continues to work exactly as before, the existing `/claim` and heartbeat paths are unchanged, and registering a runtime without `runtime_mode` defaults to `local`. The patch can ship with `MULTICA_WEBHOOK_RUNTIME=0` (default) for any operator who wants the schema rolled out before the dispatch behavior.
