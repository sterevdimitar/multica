import type {
  DashboardUsageDaily,
  DashboardUsageByAgent,
  DashboardAgentRunTime,
  DashboardRunTimeDaily,
} from "@multica/core/types";
import {
  addDaysIso,
  formatShortDate,
  todayIso,
  weekStartIso,
  type DailyTokenData,
  type WeeklyTokenData,
} from "../runtimes/utils";
import type {
  DailyTimeData,
  DailyTasksData,
  WeeklyTimeData,
  WeeklyTasksData,
} from "../runtimes/components/charts";

// ---------------------------------------------------------------------------
// Dashboard data aggregations
//
// The workspace dashboard returns the same per-(date, model) and
// per-(agent, model) shapes the runtime page does, PLUS the stored
// `total_cost_usd` rolled up from task_usage — and every dollar figure here
// is a sum of that stored value. This package deliberately does NOT import
// the runtimes utils' client-side price estimators: the pipeline prices each
// run by the engine that actually ran it, with a rate table the fork's own
// price table does not have (it knows `glm-5`, not the LiteLLM aliases
// `dcc-glm-*`), so estimating here is how GLM spend came to read as $0 while
// the card showed it at 33x. A null cost contributes nothing and is never
// replaced by a guess. A test greps this file for the estimator's name to
// keep it that way — which is also why this comment does not spell it.
// ---------------------------------------------------------------------------

/** One point of the daily cost chart: the stored cost summed over the day. */
export interface DailyCostPoint {
  date: string;
  label: string;
  total: number;
}

function formatDateLabel(d: string): string {
  // Anchor to local midnight so the formatted label matches the bucket the
  // server picked (which is already in workspace time). Pasting the raw
  // date as the body of `new Date()` would interpret it as UTC and shift
  // by the user's offset.
  const date = new Date(d + "T00:00:00");
  return `${date.getMonth() + 1}/${date.getDate()}`;
}

// Per-(date, model) rows → 1 point per date: the STORED cost summed across
// models. One series, not the input / output / cache-write stack the runtime
// page draws — the stored figure is a total, and it is not fabricated back
// into parts. Stable sort by date asc so the chart x-axis reads oldest →
// newest.
export function aggregateDailyCost(usage: DashboardUsageDaily[]): DailyCostPoint[] {
  const map = new Map<string, number>();
  for (const u of usage) {
    map.set(u.date, (map.get(u.date) ?? 0) + (u.total_cost_usd ?? 0));
  }
  return Array.from(map.entries())
    .toSorted(([a], [b]) => a.localeCompare(b))
    .map(([date, total]) => ({ date, label: formatDateLabel(date), total: round2(total) }));
}

/** One bar of the weekly cost chart: a decorated week plus its stored cost. */
export type WeeklyCostPoint = Pick<
  WeeklyTokenData,
  "weekStart" | "weekEnd" | "label" | "rangeLabel" | "partial" | "daysCovered"
> & { total: number };

// The stored cost summed into the weeks `aggregateByWeek` already decorated
// for the tokens chart (same window, same partial-week flag), so the two
// weekly charts are the same weeks by construction. A row outside those
// weeks is dropped, exactly as aggregateByWeek drops it.
export function aggregateWeeklyCost(
  usage: DashboardUsageDaily[],
  weeks: WeeklyTokenData[],
): WeeklyCostPoint[] {
  const totals = new Map<string, number>(weeks.map((w) => [w.weekStart, 0]));
  for (const u of usage) {
    const wk = weekStartIso(u.date);
    if (!totals.has(wk)) continue;
    totals.set(wk, (totals.get(wk) ?? 0) + (u.total_cost_usd ?? 0));
  }
  return weeks
    .toSorted((a, b) => a.weekStart.localeCompare(b.weekStart))
    .map(({ weekStart, weekEnd, label, rangeLabel, partial, daysCovered }) => ({
      weekStart,
      weekEnd,
      label,
      rangeLabel,
      partial,
      daysCovered,
      total: round2(totals.get(weekStart) ?? 0),
    }));
}

const round2 = (n: number) => Math.round(n * 100) / 100;

// Per-(date, model) rows → 1 row per date with raw token counts split
// across the four chart segments. Independent of pricing — unmapped
// models still contribute here, even if they're excluded from cost.
// Mirrors `aggregateByDate(...).dailyTokens` from the runtimes utils so
// the Tokens chart on the Usage page consumes the same shape as the one
// on the runtime-detail page.
export function aggregateDailyTokens(usage: DashboardUsageDaily[]): DailyTokenData[] {
  const map = new Map<
    string,
    { input: number; output: number; cacheRead: number; cacheWrite: number }
  >();
  for (const u of usage) {
    const entry = map.get(u.date) ?? {
      input: 0,
      output: 0,
      cacheRead: 0,
      cacheWrite: 0,
    };
    entry.input += u.input_tokens;
    entry.output += u.output_tokens;
    entry.cacheRead += u.cache_read_tokens;
    entry.cacheWrite += u.cache_write_tokens;
    map.set(u.date, entry);
  }
  return Array.from(map.entries())
    .toSorted(([a], [b]) => a.localeCompare(b))
    .map(([date, t]) => ({
      date,
      label: formatDateLabel(date),
      input: t.input,
      output: t.output,
      cacheRead: t.cacheRead,
      cacheWrite: t.cacheWrite,
    }));
}

export interface DashboardTokenTotals {
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
  cost: number;
  taskCount: number;
}

// Whole-window totals for the KPI tiles. taskCount sums DISTINCT task counts
// per row — these are already collapsed server-side per (date, model), so
// the value can over-count if the same task has tokens in two days; that's
// acceptable for a KPI ("rough volume") and the per-agent run-time card
// gives the precise figure.
export function computeDailyTotals(usage: DashboardUsageDaily[]): DashboardTokenTotals {
  return usage.reduce<DashboardTokenTotals>(
    (acc, u) => ({
      input: acc.input + u.input_tokens,
      output: acc.output + u.output_tokens,
      cacheRead: acc.cacheRead + u.cache_read_tokens,
      cacheWrite: acc.cacheWrite + u.cache_write_tokens,
      cost: acc.cost + (u.total_cost_usd ?? 0),
      taskCount: acc.taskCount + u.task_count,
    }),
    { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cost: 0, taskCount: 0 },
  );
}

export interface AgentCostRow {
  agentId: string;
  tokens: number;
  cost: number;
  taskCount: number;
}

// Fold per-(agent, model) rows into one row per agent. Cost is the STORED
// sum across this agent's models (null contributes nothing), which is the
// figure the user cares about. Sort by cost desc so the heaviest spender
// lands first.
export function aggregateAgentTokens(rows: DashboardUsageByAgent[]): AgentCostRow[] {
  const map = new Map<string, AgentCostRow>();
  for (const r of rows) {
    const entry = map.get(r.agent_id) ?? {
      agentId: r.agent_id,
      tokens: 0,
      cost: 0,
      taskCount: 0,
    };
    entry.tokens +=
      r.input_tokens + r.output_tokens + r.cache_read_tokens + r.cache_write_tokens;
    entry.cost += r.total_cost_usd ?? 0;
    entry.taskCount += r.task_count;
    map.set(r.agent_id, entry);
  }
  return Array.from(map.values()).toSorted((a, b) => b.cost - a.cost);
}

export interface AgentDashboardRow {
  agentId: string;
  tokens: number;
  cost: number;
  seconds: number;
  taskCount: number;
}

// Merge per-agent token totals with per-agent run-time totals into one
// row per agent.
//
// taskCount comes from `runTimeRows` when available — that rollup is a
// true per-agent distinct count (`COUNT(*)` on (agent, terminal-task) in
// SQL). The token rollup's per-(agent, model) counts double-count a task
// when it spans multiple models, so we only fall back to it for agents
// with no terminal run yet (in-flight tasks reported tokens but haven't
// completed). Sorted by cost desc, then run time desc.
export function mergeAgentDashboardRows(
  tokenRows: AgentCostRow[],
  runTimeRows: DashboardAgentRunTime[],
): AgentDashboardRow[] {
  const runTimeByAgent = new Map(
    runTimeRows.map((r) => [r.agent_id, r] as const),
  );
  const merged = new Map<string, AgentDashboardRow>();
  for (const r of tokenRows) {
    const rt = runTimeByAgent.get(r.agentId);
    merged.set(r.agentId, {
      agentId: r.agentId,
      tokens: r.tokens,
      cost: r.cost,
      seconds: rt?.total_seconds ?? 0,
      taskCount: rt ? rt.task_count : r.taskCount,
    });
  }
  // Agents with run-time rows but zero tokens still belong on the list
  // (a task that errored before producing usage). Their token columns
  // stay at 0.
  for (const r of runTimeRows) {
    if (merged.has(r.agent_id)) continue;
    merged.set(r.agent_id, {
      agentId: r.agent_id,
      tokens: 0,
      cost: 0,
      seconds: r.total_seconds,
      taskCount: r.task_count,
    });
  }
  return Array.from(merged.values()).toSorted((a, b) => {
    if (b.cost !== a.cost) return b.cost - a.cost;
    return b.seconds - a.seconds;
  });
}

// Synthetic agentId for the row that aggregates all hard-deleted agents.
// Sentinel (not a real UUID) so the component can detect it and render a
// placeholder instead of looking the id up in the agent list.
export const DELETED_AGENTS_ROW_ID = "__deleted_agents__";

// Fold usage rows whose agent no longer exists in the workspace into a single
// aggregated "Deleted agents" row instead of dropping them. The agent list is
// fetched with `include_archived: true`, so archived agents keep their names
// and stay on the leaderboard as themselves; only hard-deleted agents fall out
// of `knownAgentIds` and collapse into the bucket.
//
// MUL-3771 (PR #4637) originally *dropped* these rows so they'd stop rendering
// as a bare UUID — but the top-line Cost/Tokens KPIs still count their spend
// (those totals aggregate `task_usage_hourly` without joining `agent`), so the
// per-agent breakdown no longer reconciled with the totals (MUL-3776, #4640).
// Aggregating instead of dropping keeps `sum(visible rows) == KPI total` while
// still never exposing a UUID. The bucket carries tokens + cost only; seconds
// and taskCount stay 0 because the run-time rollups inner-join `agent`, so
// deleted agents already contribute nothing to the Time/Tasks KPIs — the
// component renders those two columns as "—" for this row.
//
// `knownAgentIds` is `null` while the agent list is still loading; callers
// pass `null` in that case so the rows pass through untouched instead of the
// whole leaderboard collapsing into one bucket on a slow fetch.
export function bucketUnknownAgentRows(
  rows: AgentDashboardRow[],
  knownAgentIds: ReadonlySet<string> | null,
): AgentDashboardRow[] {
  if (!knownAgentIds) return rows;
  const known: AgentDashboardRow[] = [];
  const bucket: AgentDashboardRow = {
    agentId: DELETED_AGENTS_ROW_ID,
    tokens: 0,
    cost: 0,
    seconds: 0,
    taskCount: 0,
  };
  let hasDeleted = false;
  for (const r of rows) {
    if (knownAgentIds.has(r.agentId)) {
      known.push(r);
      continue;
    }
    hasDeleted = true;
    bucket.tokens += r.tokens;
    bucket.cost += r.cost;
  }
  return hasDeleted ? [...known, bucket] : known;
}

// ---------------------------------------------------------------------------
// Weekly fold for run-time + tasks. Mirrors `aggregateByWeek` in
// `runtimes/utils.ts` which already covers cost / tokens — same calendar
// week semantics (Mon–Sun anchored at today-in-tz), same pre-zeroed buckets,
// same partial-week metadata. Workspace dashboard uses the user-chosen
// timezone here; the runtime page uses the runtime's IANA tz. Behaviour is
// identical apart from where the tz comes from.
// ---------------------------------------------------------------------------

interface WeekShell {
  weekStart: string;
  weekEnd: string;
  label: string;
  rangeLabel: string;
  partial: boolean;
  daysCovered: number;
}

// Build N trailing calendar week shells anchored at today-in-tz. Each shell
// carries the labels and partial-week metadata the chart components consume;
// downstream aggregators fold their own per-week values onto the matching
// shell.
function buildWeekShells(tz: string, weekCount: number): WeekShell[] {
  const count = Math.max(1, Math.floor(weekCount));
  const today = todayIso(tz);
  const currentWeekStart = weekStartIso(today);
  const firstWeekStart = addDaysIso(currentWeekStart, -(count - 1) * 7);
  const shells: WeekShell[] = [];
  for (let i = 0; i < count; i++) {
    const weekStart = addDaysIso(firstWeekStart, i * 7);
    const weekEnd = addDaysIso(weekStart, 6);
    const partial = today < weekEnd;
    // Inclusive count of how many days of this week have actually elapsed.
    // Closed weeks sit at 7; the current week reports 1..6.
    const clampedToday =
      today < weekStart ? weekStart : today < weekEnd ? today : weekEnd;
    const elapsed = Math.min(7, Math.max(1, diffDaysIso(weekStart, clampedToday) + 1));
    shells.push({
      weekStart,
      weekEnd,
      label: formatShortDate(weekStart),
      rangeLabel: `${formatShortDate(weekStart)} – ${formatShortDate(weekEnd)}`,
      partial,
      daysCovered: partial ? elapsed : 7,
    });
  }
  return shells;
}

function diffDaysIso(from: string, to: string): number {
  const [y1, m1, d1] = from.split("-").map(Number);
  const [y2, m2, d2] = to.split("-").map(Number);
  const a = Date.UTC(y1 ?? 1970, (m1 ?? 1) - 1, d1 ?? 1);
  const b = Date.UTC(y2 ?? 1970, (m2 ?? 1) - 1, d2 ?? 1);
  return Math.round((b - a) / 86_400_000);
}

export function aggregateWeeklyTime(
  rows: DashboardRunTimeDaily[],
  tz: string,
  weekCount: number,
): WeeklyTimeData[] {
  const shells = buildWeekShells(tz, weekCount);
  const totals = new Map<string, number>();
  for (const shell of shells) totals.set(shell.weekStart, 0);
  for (const r of rows) {
    const wkStart = weekStartIso(r.date);
    if (!totals.has(wkStart)) continue;
    totals.set(wkStart, (totals.get(wkStart) ?? 0) + r.total_seconds);
  }
  return shells.map((s) => ({ ...s, totalSeconds: totals.get(s.weekStart) ?? 0 }));
}

export function aggregateWeeklyTasks(
  rows: DashboardRunTimeDaily[],
  tz: string,
  weekCount: number,
): WeeklyTasksData[] {
  const shells = buildWeekShells(tz, weekCount);
  const buckets = new Map<string, { completed: number; failed: number }>();
  for (const shell of shells)
    buckets.set(shell.weekStart, { completed: 0, failed: 0 });
  for (const r of rows) {
    const wkStart = weekStartIso(r.date);
    const bucket = buckets.get(wkStart);
    if (!bucket) continue;
    const failed = r.failed_count;
    const completed = Math.max(0, r.task_count - failed);
    bucket.completed += completed;
    bucket.failed += failed;
  }
  return shells.map((s) => {
    const b = buckets.get(s.weekStart) ?? { completed: 0, failed: 0 };
    return { ...s, completed: b.completed, failed: b.failed };
  });
}

// Per-date run-time rows → one row per date with `totalSeconds` for the
// DailyTimeChart. Sorted ascending so the x-axis reads oldest-to-newest,
// matching the cost / tokens aggregators.
export function aggregateDailyTime(rows: DashboardRunTimeDaily[]): DailyTimeData[] {
  return rows.toSorted((a, b) => a.date.localeCompare(b.date))
    .map((r) => ({
      date: r.date,
      label: formatDateLabel(r.date),
      totalSeconds: r.total_seconds,
    }));
}

// Per-date run-time rows → one row per date with `completed` and `failed`
// counts for the DailyTasksChart's stacked bar (failed_count is a subset
// of task_count, so completed = task_count - failed_count).
export function aggregateDailyTasks(rows: DashboardRunTimeDaily[]): DailyTasksData[] {
  return rows.toSorted((a, b) => a.date.localeCompare(b.date))
    .map((r) => {
      const failed = r.failed_count;
      const completed = Math.max(0, r.task_count - failed);
      return {
        date: r.date,
        label: formatDateLabel(r.date),
        completed,
        failed,
      };
    });
}

// Compact human duration: "1h 23m" / "12m 30s" / "45s" / "<1m". Used for
// the dashboard run-time KPI and the per-agent run-time column. Keeps two
// segments max — three segments adds visual noise without precision the
// dashboard actually needs.
export function formatDuration(seconds: number, lessThanMinuteLabel: string): string {
  if (seconds < 0 || !Number.isFinite(seconds)) return lessThanMinuteLabel;
  if (seconds < 60) {
    if (seconds < 1) return lessThanMinuteLabel;
    return `${Math.round(seconds)}s`;
  }
  const totalMinutes = Math.floor(seconds / 60);
  const hours = Math.floor(totalMinutes / 60);
  const mins = totalMinutes % 60;
  if (hours === 0) {
    const secs = Math.floor(seconds) % 60;
    return secs > 0 ? `${mins}m ${secs}s` : `${mins}m`;
  }
  if (hours >= 24) {
    const days = Math.floor(hours / 24);
    const h = hours % 24;
    return h > 0 ? `${days}d ${h}h` : `${days}d`;
  }
  return mins > 0 ? `${hours}h ${mins}m` : `${hours}h`;
}
