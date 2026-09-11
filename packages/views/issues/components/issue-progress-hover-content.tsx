"use client";

import { useEffect, useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { issueProgressOptions } from "@multica/core/issues/queries";
import { cn } from "@multica/ui/lib/utils";
import {
  completedTotals,
  formatTokens,
  formatCost,
  formatTurns,
  groupProgressRows,
  shortFailureReason,
  shortStepName,
  type ProgressRow,
} from "../surface/progress";
import { useT } from "../../i18n";

/** Decorative row markers. Not translatable copy — glyphs, like the
 *  STATUS_MARK set below. */
const PENDING_MARK = "○";

/**
 * Row marker per outcome.
 *
 * A FINISHED RUN IS NOT AUTOMATICALLY A SUCCESSFUL ONE. This used to be a
 * `status === "live" ? "▶" : "✓"` ternary, so failed and cancelled runs drew
 * the same tick as a clean completion — a review that died mid-run looked
 * identical to one that produced findings, on the surface a person checks
 * first. `ProgressRow.status` has carried all four states all along; only
 * this renderer collapsed them.
 *
 * Glyphs, not translatable copy — same convention as PENDING_MARK.
 */
const STATUS_MARK: Record<ProgressRow["status"], string> = {
  live: "▶",
  completed: "✓",
  failed: "✗",
  cancelled: "⊘",
};

interface IssueProgressHoverContentProps {
  issueId: string;
}

/**
 * Tick once per second so the live row's timer advances while the popover is
 * open. The interval only exists while this component is mounted, and the
 * hover card tears its content down on close — so a board full of cards costs
 * nothing until someone actually hovers one.
 */
function useProgressNow(): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, []);
  return now;
}

/** `M:SS`, or `H:MM:SS` past an hour. Null renders as an em dash. */
export function formatElapsed(ms: number | null): string {
  if (ms === null) return "—";
  const total = Math.max(0, Math.round(ms / 1000));
  const sec = total % 60;
  const min = Math.floor(total / 60) % 60;
  const hr = Math.floor(total / 3600);
  const ss = String(sec).padStart(2, "0");
  if (hr === 0) return `${min}:${ss}`;
  return `${hr}:${String(min).padStart(2, "0")}:${ss}`;
}

/**
 * Per-step progress table behind the board card's hover.
 *
 * Fetch-on-open: this component mounts when the hover card opens, so the
 * query fires then rather than on every card render. While it is open the
 * task:usage WS handler writes the same cache key, so the numbers stay live
 * without polling.
 *
 * Read-only throughout — it renders what the run already did and never
 * writes status, comments, or assignees.
 */
export function IssueProgressHoverContent({ issueId }: IssueProgressHoverContentProps) {
  const { t } = useT("issues");
  const { data, isPending } = useQuery(issueProgressOptions(issueId));
  const now = useProgressNow();

  const rows = useMemo(() => groupProgressRows(data?.tasks ?? []), [data?.tasks]);
  const totals = useMemo(() => completedTotals(rows), [rows]);

  // Clock-skew correction. The badge on the card face has no server_now and
  // accepts the skew; here we have one, so the popover's numbers are the
  // authoritative ones. Recomputed per fetch, not per tick.
  const offsetMs = useMemo(() => {
    const serverNow = Date.parse(data?.server_now ?? "");
    if (Number.isNaN(serverNow)) return 0;
    return Date.now() - serverNow;
  }, [data?.server_now]);

  const expectedSteps = data?.expected_steps ?? null;

  // Pending rows: declared steps no run has touched yet. Only shown when the
  // chain derivation succeeded — a guessed chain is worse than none.
  const pendingSteps = useMemo(() => {
    if (!expectedSteps) return [];
    const seen = new Set(rows.map((r) => r.agentName));
    return expectedSteps.filter((name) => !seen.has(name));
  }, [expectedSteps, rows]);

  const stepCounter = useMemo(() => {
    if (!expectedSteps || expectedSteps.length === 0) return null;
    const finished = rows.filter((r) => r.status !== "live").length;
    return { n: Math.min(finished + 1, expectedSteps.length), m: expectedSteps.length };
  }, [expectedSteps, rows]);

  // The trigger is on every board card now, so "this issue has never had a
  // run" is a normal, common state — it gets a sentence rather than an empty
  // table. Nothing is rendered while the first fetch is still in flight: a
  // flash of "no runs yet" that then becomes a table reads as a bug.
  if (rows.length === 0 && pendingSteps.length === 0) {
    if (isPending) return null;
    return (
      <p className="text-xs text-muted-foreground">
        {t(($) => $.progress.empty)}
      </p>
    );
  }

  const liveElapsed = (row: ProgressRow): number | null => {
    if (row.status !== "live" || !row.liveStartedAt) return row.elapsedMs;
    const started = Date.parse(row.liveStartedAt);
    if (Number.isNaN(started)) return row.elapsedMs;
    return (now - offsetMs) - started + (row.elapsedMs ?? 0);
  };

  return (
    <div className="flex flex-col gap-2">
      {stepCounter && (
        <div className="text-xs font-medium text-muted-foreground">
          {t(($) => $.progress.step_of, { n: stepCounter.n, m: stepCounter.m })}
        </div>
      )}

      <table className="w-full text-xs tabular-nums">
        <thead>
          <tr className="text-[10px] uppercase text-muted-foreground">
            <th className="w-[26%] text-left font-normal">{t(($) => $.progress.step)}</th>
            <th className="text-right font-normal">{t(($) => $.progress.elapsed)}</th>
            <th className="text-right font-normal">{t(($) => $.progress.tokens)}</th>
            <th className="text-right font-normal">{t(($) => $.progress.cost)}</th>
            <th className="text-right font-normal">{t(($) => $.progress.turns)}</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr
              key={row.taskIds.join(",")}
              className={cn(
                row.status === "live" && "font-medium text-foreground",
                row.status === "failed" && "text-destructive",
              )}
            >
              <td className="truncate text-left">
                <span
                  className={cn("mr-1", row.status !== "failed" && "text-muted-foreground")}
                  aria-hidden
                >
                  {STATUS_MARK[row.status]}
                </span>
                {shortStepName(row.agentName)}
                {row.count > 1 && (
                  <span className="ml-1 text-muted-foreground">{`×${row.count}`}</span>
                )}
                {/* The reason a failed step failed, in its own words. This
                    replaces the "21/20" ratio that used to hint at it and
                    could not do so correctly — see formatTurns. */}
                {row.status === "failed" && row.failureReason && (
                  <span className="ml-1 text-muted-foreground" title={row.failureReason}>
                    {shortFailureReason(row.failureReason)}
                  </span>
                )}
              </td>
              <td className="text-right">{formatElapsed(liveElapsed(row))}</td>
              <td className="text-right">{formatTokens(row.tokens)}</td>
              <td className="text-right">{formatCost(row.costUsd)}</td>
              <td className="text-right">{formatTurns(row.turns, row.maxTurns)}</td>
            </tr>
          ))}

          {pendingSteps.map((name) => (
            <tr key={`pending-${name}`} className="text-muted-foreground/60">
              <td className="truncate text-left">
                <span className="mr-1" aria-hidden>{PENDING_MARK}</span>
                {shortStepName(name)}
              </td>
              <td className="text-right">—</td>
              <td className="text-right">—</td>
              <td className="text-right">—</td>
            </tr>
          ))}
        </tbody>
        <tfoot>
          {/* Completed steps only: a total that grows while you watch it
              cannot be compared against the last run. */}
          <tr className="border-t border-border text-muted-foreground">
            <td className="text-left">{t(($) => $.progress.completed_footer)}</td>
            <td className="text-right">{formatElapsed(totals.elapsedMs)}</td>
            <td className="text-right">{formatTokens(totals.tokens)}</td>
            <td className="text-right">{formatCost(totals.costUsd)}</td>
            <td className="text-right">{totals.turns}</td>
          </tr>
        </tfoot>
      </table>
    </div>
  );
}
