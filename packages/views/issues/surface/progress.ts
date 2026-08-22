import type { IssueProgressTask } from "@multica/core/api/schemas";

/**
 * Pure transforms behind the ticket-progress badge and popover.
 *
 * Nothing here fetches or renders — grouping is a RENDER-TIME concern and is
 * never persisted: the server stores one row per task, and the "fixer ×3"
 * shape exists only in this file's output.
 */

/** A render-time group of adjacent same-agent tasks. */
export interface ProgressRow {
  /** Raw agent name of the group (short names are a display concern). */
  agentName: string;
  /** Run-length of the group; render "×N" when > 1. */
  count: number;
  /**
   * "live" if any member is still running, "failed" if any member failed and
   * none is live, "cancelled" if every member was cancelled, else
   * "completed".
   */
  status: "completed" | "live" | "failed" | "cancelled";
  /** displayTokens summed over the group. */
  tokens: number;
  turns: number;
  /**
   * Summed completed_at − started_at over members that have both. null when
   * no member does. The live member contributes nothing — the client adds
   * the ticking part from liveStartedAt.
   */
  elapsedMs: number | null;
  /** started_at of the live member, the timer anchor. null when not live. */
  liveStartedAt: string | null;
  taskIds: string[];
}

/** Statuses that count as a finished-but-not-successful run. */
const FAILED_STATUSES = new Set(["failed", "timeout", "error"]);

/**
 * Display tokens = input + output + cache_creation.
 *
 * Cache READS are stored and available but excluded from every headline
 * number: they routinely dwarf the real work by two orders of magnitude and
 * would make every step look identical.
 */
export function displayTokens(t: {
  input: number;
  output: number;
  cache_creation: number;
}): number {
  return t.input + t.output + t.cache_creation;
}

/** Milliseconds between two timestamps, or null if either is missing/unparseable. */
function spanMs(
  startedAt: string | null | undefined,
  completedAt: string | null | undefined,
): number | null {
  if (!startedAt || !completedAt) return null;
  const start = Date.parse(startedAt);
  const end = Date.parse(completedAt);
  if (Number.isNaN(start) || Number.isNaN(end)) return null;
  return end - start;
}

/** Sort anchor: when a step is running the queue wait is behind it, so
 *  started_at wins; a never-started step sorts by when it was queued. */
function sortAnchor(t: IssueProgressTask): number {
  const raw = t.started_at ?? t.queued_at;
  const parsed = Date.parse(raw ?? "");
  return Number.isNaN(parsed) ? 0 : parsed;
}

/**
 * Run-length encode tasks into rows. The server already orders by
 * COALESCE(started_at, created_at), but this re-sorts defensively so a
 * WS-updated or hand-assembled list cannot silently mis-group.
 *
 * Only ADJACENT same-agent tasks merge — an agent that reappears after a
 * different one starts a new row, because that is a genuinely later phase of
 * the run and collapsing it would hide the ordering.
 */
export function groupProgressRows(tasks: IssueProgressTask[]): ProgressRow[] {
  const ordered = [...tasks].sort((a, b) => sortAnchor(a) - sortAnchor(b));

  const rows: ProgressRow[] = [];
  for (const t of ordered) {
    const last = rows[rows.length - 1];
    const row =
      last && last.agentName === t.agent_name
        ? last
        : (() => {
            const fresh: ProgressRow = {
              agentName: t.agent_name,
              count: 0,
              status: "completed",
              tokens: 0,
              turns: 0,
              elapsedMs: null,
              liveStartedAt: null,
              taskIds: [],
            };
            rows.push(fresh);
            return fresh;
          })();

    row.count += 1;
    row.taskIds.push(t.task_id);
    row.tokens += displayTokens(t.tokens);
    row.turns += t.turns;

    if (t.is_live) {
      row.status = "live";
      // The most recent live member owns the timer anchor.
      row.liveStartedAt = t.started_at ?? null;
      continue; // a live member has no finished span to add
    }

    const span = spanMs(t.started_at, t.completed_at);
    if (span !== null) row.elapsedMs = (row.elapsedMs ?? 0) + span;

    if (row.status !== "live") {
      if (FAILED_STATUSES.has(t.status)) {
        row.status = "failed";
      } else if (row.status === "completed" && t.status === "cancelled") {
        // "cancelled" only survives while every member so far is cancelled.
        row.status = row.count === 1 ? "cancelled" : row.status;
      } else if (row.status === "cancelled" && t.status !== "cancelled") {
        row.status = "completed";
      }
    }
  }

  return rows;
}

/**
 * Cumulative footer: every FINISHED run, live excluded.
 *
 * Excluding the live row is deliberate — a total that grows while you watch
 * it cannot be compared against the previous run, which is the only thing
 * the number is for.
 */
export function completedTotals(rows: ProgressRow[]): {
  elapsedMs: number;
  tokens: number;
  turns: number;
} {
  let elapsedMs = 0;
  let tokens = 0;
  let turns = 0;
  for (const r of rows) {
    if (r.status === "live") continue;
    elapsedMs += r.elapsedMs ?? 0;
    tokens += r.tokens;
    turns += r.turns;
  }
  return { elapsedMs, tokens, turns };
}

/** Compact token count: exact below 1000, then one decimal with a unit. */
export function formatTokens(n: number): string {
  if (n < 1_000) return String(n);
  if (n < 1_000_000) return `${(n / 1_000).toFixed(1)}k`;
  return `${(n / 1_000_000).toFixed(1)}M`;
}

/** Short step labels for the badge and the popover's Step column. */
const SHORT_STEP_NAMES: Record<string, string> = {
  "review-agent": "review",
  "review-validator-agent": "validator",
  fixer: "fixer",
  "readiness-agent": "judge",
};

/** Unknown agents fall back to their raw name — never to a placeholder. */
export function shortStepName(agentName: string): string {
  return SHORT_STEP_NAMES[agentName] ?? agentName;
}
