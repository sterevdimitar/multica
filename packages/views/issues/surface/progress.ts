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
   * "live" if any member is actually running, "queued" if a member is open
   * but not yet running (queued or dispatched) and none is live, "failed" if
   * any member failed and none is open, "cancelled" if every member was
   * cancelled, else "completed".
   *
   * live vs queued uses the SAME predicate as the card-face badge in
   * surface/activity.ts — `status === "running"` is the only working state.
   * `is_live` alone cannot pick a glyph: it is true for a task waiting
   * behind another run, and drawing ▶ for that contradicts the "Queued" the
   * badge already shows on the same card.
   */
  status: "completed" | "live" | "queued" | "failed" | "cancelled";
  /**
   * displayTokens summed over members that reported a non-zero figure; null
   * when no member did. Mirrors costUsd on this same struct: 0 is never a
   * measurement — every round trip bills input tokens (see
   * use-task-metrics.ts's "any real work reports something") — so an
   * all-zero member contributes nothing rather than a confident 0, and a
   * group where every member is unmeasured renders a dash, not "0".
   */
  tokens: number | null;
  turns: number;
  /**
   * Stored cost summed over the group's priced members; null when no member
   * carried one. Never coerced to 0: $0 reads as "free", and the stored
   * figure exists precisely to stop wrong numbers on this popover.
   */
  costUsd: number | null;
  /**
   * Turn budget for the group, summed the same way `turns` is. 0 means
   * UNKNOWN — either the agent declares no `--max-turns`, or at least one
   * member of the group does not, which makes the whole denominator
   * unusable. Render a bare count in that case; a partial sum understates
   * the budget and makes a healthy group look like it overran.
   */
  maxTurns: number;
  /**
   * Why the group's failed member stopped, verbatim from the run — "" when
   * nothing in the group failed or the server did not say. The FIRST failure
   * in the group wins: a later member's reason would describe a different
   * run than the ✗ the reader is looking at, and the first is the one that
   * broke the chain.
   */
  failureReason: string;
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
  /** Accepted and deliberately IGNORED — see the note above. */
  cache_read?: number;
}): number {
  return t.input + t.output + t.cache_creation;
}

/**
 * displayTokens, or null when it is exactly 0.
 *
 * 0 is never a measurement: every round trip bills input tokens, so an
 * all-zero figure means the usage never flushed, not that the step was
 * free. On the Deep Infra / GLM route Claude Code's per-block usage really
 * is all-zero mid-run — see use-task-metrics.ts. This is the one place that
 * decision is made; the badge, the popover and the metrics map all call it
 * so the three surfaces cannot disagree.
 */
export function knownTokens(t: {
  input: number;
  output: number;
  cache_creation: number;
  cache_read?: number;
}): number | null {
  const n = displayTokens(t);
  return n === 0 ? null : n;
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
              tokens: null,
              turns: 0,
              costUsd: null,
              maxTurns: 0,
              failureReason: "",
              elapsedMs: null,
              liveStartedAt: null,
              taskIds: [],
            };
            rows.push(fresh);
            return fresh;
          })();

    row.count += 1;
    row.taskIds.push(t.task_id);
    // A member whose figure is all-zero is unmeasured, not free — it
    // contributes nothing, same as an unpriced member's costUsd below.
    const memberTokens = knownTokens(t.tokens);
    if (memberTokens !== null) {
      row.tokens = (row.tokens ?? 0) + memberTokens;
    }
    row.turns += t.turns;
    // Wire data, read defensively like failure_reason below: an older
    // server omits the field and the schema defaults it to null.
    if (typeof t.cost_usd === "number") {
      row.costUsd = (row.costUsd ?? 0) + t.cost_usd;
    }

    // The cap aggregates like turns, but one missing cap poisons the group:
    // summing only the members that declare one would print a denominator
    // smaller than the budget actually available, which reads as an overrun.
    if (row.count === 1) {
      row.maxTurns = t.max_turns > 0 ? t.max_turns : 0;
    } else if (row.maxTurns > 0) {
      row.maxTurns = t.max_turns > 0 ? row.maxTurns + t.max_turns : 0;
    }

    if (t.is_live) {
      if (t.status === "running") {
        row.status = "live";
        // The most recent live member owns the timer anchor.
        row.liveStartedAt = t.started_at ?? null;
      } else if (row.status !== "live") {
        // Queued or dispatched: waiting, not working. A running member
        // already in the group keeps the row live and its timer anchor.
        row.status = "queued";
      }
      continue; // an open member has no finished span to add
    }

    const span = spanMs(t.started_at, t.completed_at);
    if (span !== null) row.elapsedMs = (row.elapsedMs ?? 0) + span;

    if (row.status !== "live" && row.status !== "queued") {
      if (FAILED_STATUSES.has(t.status)) {
        row.status = "failed";
        // Read defensively rather than trusting the declared type: this is
        // wire data, and a server that predates the field omits it entirely.
        // The schema defaults it to "", but the payload is .loose() and a
        // non-string here must degrade to "no reason given", never crash a
        // popover whose job is to explain a failure.
        if (!row.failureReason && typeof t.failure_reason === "string") {
          row.failureReason = t.failure_reason;
        }
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
 * Cumulative footer: every FINISHED run, live and queued excluded.
 *
 * Excluding the live row is deliberate — a total that grows while you watch
 * it cannot be compared against the previous run, which is the only thing
 * the number is for. A queued row has done nothing yet, so it is excluded
 * for the same reason.
 */
export function completedTotals(rows: ProgressRow[]): {
  elapsedMs: number;
  /** Sum of the measured completed rows; null when none reported tokens —
   *  same rule as costUsd: three dashes must total a dash, not 0. */
  tokens: number | null;
  turns: number;
  /** Sum of the priced completed rows; null when none is priced. */
  costUsd: number | null;
} {
  let elapsedMs = 0;
  let tokens: number | null = null;
  let turns = 0;
  let costUsd: number | null = null;
  for (const r of rows) {
    if (r.status === "live" || r.status === "queued") continue;
    elapsedMs += r.elapsedMs ?? 0;
    // An unmeasured row contributes nothing and does not turn the total into
    // a number on its own — same as costUsd immediately below.
    if (r.tokens !== null) tokens = (tokens ?? 0) + r.tokens;
    turns += r.turns;
    // An unpriced row contributes nothing and does not turn the total into
    // a number on its own — three dashes must total a dash, not $0.00.
    if (r.costUsd !== null) costUsd = (costUsd ?? 0) + r.costUsd;
  }
  return { elapsedMs, tokens, turns, costUsd };
}

/**
 * Dollars for the popover. null is a dash: it means "not priced", which is
 * a different fact from "$0.00" and must not be rendered as one. Below a
 * cent the value is shown as "<$0.01" rather than rounded to "$0.00" — a
 * GLM step really does cost fractions of a cent, and that is the point.
 */
export function formatCost(usd: number | null): string {
  if (usd === null) return "—";
  if (usd > 0 && usd < 0.005) return "<$0.01";
  return `$${usd.toFixed(2)}`;
}

/**
 * The turn count, bare.
 *
 * IT USED TO RENDER "21/20", and that was wrong — not imprecise, wrong. The
 * numerator and the denominator count different things: `--max-turns` counts
 * tool-use turns ONLY, while the reported count also includes the final
 * text-only turn and (measured, not documented) each parallel tool call. A
 * run that stops exactly at its budget therefore always reports above it, and
 * so can one that never came close. Readers were diagnosing overruns that had
 * not happened.
 *
 * The signal the ratio was carrying — "this run died at its ceiling" — is now
 * carried by `failureReason`, which is the run's own statement of what
 * stopped it rather than an inference from two incompatible numbers.
 *
 * maxTurns is still on ProgressRow: it is a real fact about the agent, and a
 * future display that counts tool-use turns could compare against it honestly.
 */
export function formatTurns(turns: number, _maxTurns: number): string {
  return String(turns);
}

/**
 * A failure reason shortened for the row: `claude-max-turns` -> "max turns".
 * Unknown reasons pass through with separators softened, because a reason
 * this code has not heard of is still the run's own word and is better than
 * a generic "failed".
 */
export function shortFailureReason(reason: string): string {
  if (!reason) return "";
  return reason.replace(/^claude-/, "").replace(/[-_]/g, " ");
}

/**
 * Compact token count: exact below 1000, then one decimal with a unit.
 * null is a dash, mirroring formatCost — see knownTokens for why 0 is never
 * a measurement.
 */
export function formatTokens(n: number | null): string {
  if (n === null) return "—";
  if (n < 1_000) return String(n);
  if (n < 1_000_000) return `${(n / 1_000).toFixed(1)}k`;
  return `${(n / 1_000_000).toFixed(1)}M`;
}

/** Short step labels for the badge and the popover's Step column. */
const SHORT_STEP_NAMES: Record<string, string> = {
  "review-agent": "review",
  "review-validator-agent": "validator",
  fixer: "fixer",
  "readiness-agent": "readiness",
};

/** Unknown agents fall back to their raw name — never to a placeholder. */
export function shortStepName(agentName: string): string {
  return SHORT_STEP_NAMES[agentName] ?? agentName;
}
