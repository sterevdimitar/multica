"use client";

import { memo, useCallback, useEffect, useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  HoverCard,
  HoverCardTrigger,
  HoverCardContent,
} from "@multica/ui/components/ui/hover-card";
import { useWorkspaceId } from "@multica/core/hooks";
import { useActorName } from "@multica/core/workspace/hooks";
import { agentTaskSnapshotOptions } from "@multica/core/agents";
import { issueKeys } from "@multica/core/issues/queries";
import type { AgentTask } from "@multica/core/types";
import { cn } from "@multica/ui/lib/utils";
import type { AvatarSize } from "@multica/ui/lib/avatar-size";
import { AgentAvatarStack } from "../../agents/components/agent-avatar-stack";
import { selectIssueTasks, type IssueTaskGroups } from "../surface/activity";
import { displayTokens, formatTokens, shortStepName } from "../surface/progress";
import { IssueProgressHoverContent, formatElapsed } from "./issue-progress-hover-content";
import { useT } from "../../i18n";

const EMPTY_GROUPS: IssueTaskGroups = { running: [], queued: [] };

/** Written exclusively by the task:usage WS handler; this cache entry has no
 *  queryFn and is never fetched. */
interface LiveUsageEntry {
  task_id: string;
  tokens: { input: number; output: number; cache_creation: number; cache_read: number };
  turns: number;
}

interface IssueAgentActivityIndicatorProps {
  issueId: string;
  // Avatar tier. Kept very small — this is a corner-of-card cue, not a
  // primary control. Default xs (16 px) reads as a dot at typical board
  // densities while still showing the agent's face on hover-zoom.
  size?: AvatarSize;
}

/**
 * Tick once per second so the badge's elapsed timer advances. The interval
 * exists only while a task is actually running on this issue — see the guard
 * at the call site — so an idle board pays nothing.
 */
function useBadgeNow(active: boolean): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!active) return;
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, [active]);
  return now;
}

/**
 * Small "is there an agent working on this issue right now" badge shown
 * in the top-right of board cards and right after the identifier in list
 * rows. Derives state from the workspace-wide agent task snapshot:
 *
 *   - has ≥1 running task  → tiny avatar stack + shimmering "Working"
 *   - 0 running, ≥1 queued → half-opacity stack + muted "Queued"
 *   - nothing               → return null (no chrome, no placeholder)
 *
 * When a task IS running the badge additionally carries a progress line:
 * `[step] ⏱ M:SS 🪙 NN.Nk`. The timer runs off the running task's started_at;
 * the token count comes from the WS-fed live-usage cache and appears only
 * once the first ~30s usage flush has landed, so a step under 30s shows the
 * timer alone rather than a misleading zero.
 *
 * The card face deliberately does NOT skew-correct: there is no server_now
 * without a fetch, and fetching per card would defeat the point of a badge.
 * The popover's numbers are the corrected, authoritative ones.
 *
 * Hover opens IssueProgressHoverContent — the per-step table. The trigger is
 * rendered whenever the issue has any tasks at all, not only running ones,
 * so a parked or finished card still offers its history on hover.
 *
 * Subscribes to the one shared workspace snapshot query but narrows it to
 * this issue's tasks with a `select`. React Query's structural sharing keeps
 * that selected value referentially stable when this issue's tasks are
 * unchanged, so a snapshot invalidation (WS task:* events, driven by
 * use-realtime-sync) only re-renders the rows whose own tasks actually moved
 * — not the whole list. This is the de-amplification that keeps large issue
 * lists cheap when agents are busy (MUL-4474). 30s staleTime is the offline
 * fallback only.
 */
export const IssueAgentActivityIndicator = memo(function IssueAgentActivityIndicator({
  issueId,
  size = "xs",
}: IssueAgentActivityIndicatorProps) {
  const { t } = useT("issues");
  const wsId = useWorkspaceId();
  const { getActorName } = useActorName();
  const select = useCallback(
    (snapshot: AgentTask[]) => selectIssueTasks(snapshot, issueId),
    [issueId],
  );
  const { data: groups = EMPTY_GROUPS } = useQuery({
    ...agentTaskSnapshotOptions(wsId),
    select,
  });

  // Read-only subscription to the WS-written cache: no queryFn, never
  // fetched, never refetched.
  const { data: liveUsage } = useQuery<LiveUsageEntry | undefined>({
    queryKey: issueKeys.liveUsage(issueId),
    queryFn: () => undefined,
    enabled: false,
    staleTime: Infinity,
  });

  const { agentIds, opacity } = useMemo(() => {
    // Stack heads: prefer running. If 0 running, fall back to queued.
    // Each case is visually distinct (running gets shimmer, queued gets
    // muted text) so the indicator always offers a face to hover.
    const primary = groups.running.length > 0 ? groups.running : groups.queued;
    const uniqueAgents = [...new Set(primary.map((t) => t.agent_id))];
    return {
      agentIds: uniqueAgents,
      opacity: (groups.running.length > 0 ? "full" : "half") as "full" | "half",
    };
  }, [groups]);

  const runningTask = groups.running[0];
  const now = useBadgeNow(Boolean(runningTask));

  if (agentIds.length === 0) return null;
  const isRunning = opacity === "full";

  // Tokens are shown only when the flushed usage belongs to the task that is
  // running right now — a stale count from the previous step would read as
  // this step's progress.
  const liveTokens =
    runningTask && liveUsage?.task_id === runningTask.id
      ? formatTokens(displayTokens(liveUsage.tokens))
      : null;

  const startedAt = runningTask?.started_at;
  const elapsed = startedAt
    ? formatElapsed(now - Date.parse(startedAt))
    : null;

  const stepName = runningTask
    ? shortStepName(getActorName("agent", runningTask.agent_id))
    : null;

  return (
    <HoverCard>
      <HoverCardTrigger
        render={
          <span className="inline-flex shrink-0 items-center gap-1" />
        }
      >
        <AgentAvatarStack
          agentIds={agentIds}
          size={size}
          opacity={opacity}
          max={3}
        />
        <span
          className={cn(
            "text-[10px] leading-none",
            isRunning
              ? "animate-chat-text-shimmer"
              : "text-muted-foreground",
          )}
        >
          {isRunning
            ? t(($) => $.agent_activity.status_running)
            : t(($) => $.agent_activity.status_queued)}
        </span>
        {isRunning && stepName && (
          <span className="text-[10px] leading-none tabular-nums text-muted-foreground">
            {[
              stepName,
              elapsed ? `⏱ ${elapsed}` : t(($) => $.progress.queued),
              liveTokens ? `🪙 ${liveTokens}` : null,
            ]
              .filter(Boolean)
              .join(" ")}
          </span>
        )}
      </HoverCardTrigger>
      <HoverCardContent align="end" className="w-80">
        <IssueProgressHoverContent issueId={issueId} />
      </HoverCardContent>
    </HoverCard>
  );
});
