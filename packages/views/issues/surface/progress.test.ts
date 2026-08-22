import { describe, expect, it } from "vitest";
import type { IssueProgressTask } from "@multica/core/api/schemas";
import {
  completedTotals,
  displayTokens,
  formatTokens,
  groupProgressRows,
  shortStepName,
} from "./progress";

function task(overrides: Partial<IssueProgressTask> = {}): IssueProgressTask {
  return {
    task_id: overrides.task_id ?? `t-${Math.random().toString(36).slice(2)}`,
    agent_name: "fixer",
    status: "completed",
    queued_at: "2026-08-22T10:00:00Z",
    started_at: "2026-08-22T10:00:00Z",
    completed_at: "2026-08-22T10:01:00Z",
    tokens: { input: 100, output: 50, cache_creation: 25, cache_read: 9_000_000 },
    turns: 3,
    is_live: false,
    ...overrides,
  } as IssueProgressTask;
}

describe("groupProgressRows", () => {
  it("merges consecutive tasks of the same agent into one row", () => {
    const rows = groupProgressRows([
      task({ task_id: "a", started_at: "2026-08-22T10:00:00Z", completed_at: "2026-08-22T10:01:00Z" }),
      task({ task_id: "b", started_at: "2026-08-22T10:02:00Z", completed_at: "2026-08-22T10:03:00Z" }),
      task({ task_id: "c", started_at: "2026-08-22T10:04:00Z", completed_at: "2026-08-22T10:05:00Z" }),
    ]);
    expect(rows).toHaveLength(1);
    expect(rows[0]!.count).toBe(3);
    expect(rows[0]!.tokens).toBe(3 * 175);
    expect(rows[0]!.turns).toBe(9);
    expect(rows[0]!.elapsedMs).toBe(3 * 60_000);
    expect(rows[0]!.taskIds).toEqual(["a", "b", "c"]);
  });

  it("does NOT merge a re-appearing agent across a different one", () => {
    const rows = groupProgressRows([
      task({ agent_name: "review-agent", started_at: "2026-08-22T10:00:00Z" }),
      task({ agent_name: "fixer", started_at: "2026-08-22T10:02:00Z" }),
      task({ agent_name: "review-agent", started_at: "2026-08-22T10:04:00Z" }),
    ]);
    expect(rows.map((r) => r.agentName)).toEqual(["review-agent", "fixer", "review-agent"]);
    expect(rows.every((r) => r.count === 1)).toBe(true);
  });

  it("falls back to queued_at when a task never started", () => {
    const rows = groupProgressRows([
      task({ agent_name: "a-started", queued_at: "2026-08-22T10:04:00Z", started_at: "2026-08-22T10:05:00Z" }),
      task({
        agent_name: "b-queued",
        queued_at: "2026-08-22T10:01:00Z",
        started_at: null,
        completed_at: null,
        status: "queued",
        is_live: true,
      }),
    ]);
    expect(rows[0]!.agentName).toBe("b-queued");
    expect(rows[1]!.agentName).toBe("a-started");
  });

  it("marks the live row and anchors its timer, excluding it from elapsed", () => {
    const rows = groupProgressRows([
      task({ agent_name: "review-validator-agent" }),
      task({
        agent_name: "fixer",
        status: "running",
        is_live: true,
        started_at: "2026-08-22T10:10:00Z",
        completed_at: null,
      }),
    ]);
    const live = rows[1]!;
    expect(live.status).toBe("live");
    expect(live.liveStartedAt).toBe("2026-08-22T10:10:00Z");
    expect(live.elapsedMs).toBeNull();
    expect(rows[0]!.liveStartedAt).toBeNull();
  });

  it("renders a failed run with its partial tokens", () => {
    const rows = groupProgressRows([
      task({ agent_name: "review-agent" }),
      task({
        agent_name: "fixer",
        status: "failed",
        tokens: { input: 10, output: 5, cache_creation: 1, cache_read: 500 },
        turns: 2,
      }),
    ]);
    expect(rows[1]!.status).toBe("failed");
    expect(rows[1]!.tokens).toBe(16);
    expect(completedTotals(rows).tokens).toBe(175 + 16);
  });

  it("groups all-cancelled members as cancelled", () => {
    const rows = groupProgressRows([
      task({ agent_name: "fixer", status: "cancelled" }),
      task({ agent_name: "fixer", status: "cancelled", started_at: "2026-08-22T10:02:00Z" }),
    ]);
    expect(rows).toHaveLength(1);
    expect(rows[0]!.status).toBe("cancelled");
  });
});

describe("completedTotals", () => {
  it("excludes the live row entirely", () => {
    const rows = groupProgressRows([
      task({ agent_name: "review-agent" }),
      task({
        agent_name: "fixer",
        status: "running",
        is_live: true,
        started_at: "2026-08-22T10:10:00Z",
        completed_at: null,
        tokens: { input: 999, output: 999, cache_creation: 999, cache_read: 0 },
        turns: 42,
      }),
    ]);
    const totals = completedTotals(rows);
    expect(totals.tokens).toBe(175);
    expect(totals.turns).toBe(3);
    expect(totals.elapsedMs).toBe(60_000);
  });
});

describe("displayTokens", () => {
  it("sums input + output + cache_creation and excludes cache reads", () => {
    expect(
      displayTokens({ input: 100, output: 50, cache_creation: 25, cache_read: 9_000_000 }),
    ).toBe(175);
  });
});

describe("formatTokens", () => {
  it.each([
    [0, "0"],
    [743, "743"],
    [999, "999"],
    [1_000, "1.0k"],
    [38_234, "38.2k"],
    [1_234_000, "1.2M"],
  ])("formats %i as %s", (input, expected) => {
    expect(formatTokens(input)).toBe(expected);
  });
});

describe("shortStepName", () => {
  it.each([
    ["review-agent", "review"],
    ["review-validator-agent", "validator"],
    ["fixer", "fixer"],
    ["readiness-agent", "judge"],
    ["custom-agent", "custom-agent"],
  ])("maps %s to %s", (input, expected) => {
    expect(shortStepName(input)).toBe(expected);
  });
});

describe("all-zero usage is 'no data', not zero", () => {
  // The progress endpoint COALESCEs missing usage to 0, so a run that never
  // flushed and a run that spent nothing arrive identical on the wire. The
  // execution log folds an all-zero row out of its metrics map so the row
  // renders em dashes; this pins the predicate that decision rests on.
  it("distinguishes an all-zero row from one with any work", () => {
    const zero = { input: 0, output: 0, cache_creation: 0, cache_read: 912_000 };
    const some = { input: 0, output: 0, cache_creation: 1, cache_read: 0 };
    expect(displayTokens(zero)).toBe(0);
    expect(displayTokens(some)).toBe(1);
  });
});
