"use client";

import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { issueProgressOptions } from "@multica/core/issues/queries";
import { knownTokens } from "./progress";

/** Per-run metrics folded out of the read-only progress projection. */
export interface TaskRowMetrics {
  /** null when the flushed usage is all-zero — unmeasured, not free. See
   *  knownTokens. */
  tokens: number | null;
  turns: number;
}

/**
 * task_id → {tokens, turns} for one issue.
 *
 * Kept fresh by the task:usage WS handler writing the same cache key, so a
 * running row's numbers move without any polling here. Both the Execution
 * log and the issue-header chip call this, so they can never disagree.
 *
 * A task ABSENT from the returned map means "no usage data at all", which
 * the rows render as em dashes. That absence is load-bearing: the progress
 * endpoint returns a row for every task and COALESCEs missing usage to zero,
 * so a run that never flushed anything and a run that spent nothing arrive
 * identical on the wire. A row that is all-zero on BOTH tokens and turns is
 * the former in practice — any real work reports something — so it is
 * dropped here rather than displayed as a confident 0.
 *
 * A row with turns > 0 but all-zero tokens is different: it is a live GLM
 * step whose per-block usage hasn't carried real counts yet (Deep Infra
 * sends zero usage per block), and its turns ARE real progress. That row
 * stays in the map with tokens: null, so the turn count moves live and the
 * tokens column renders a dash instead of "0".
 */
export function useTaskMetricsMap(issueId: string): Map<string, TaskRowMetrics> {
  const { data } = useQuery(issueProgressOptions(issueId));
  return useMemo(() => {
    const map = new Map<string, TaskRowMetrics>();
    for (const t of data?.tasks ?? []) {
      const tokens = knownTokens(t.tokens);
      if (tokens === null && t.turns === 0) continue;
      map.set(t.task_id, { tokens, turns: t.turns });
    }
    return map;
  }, [data?.tasks]);
}
