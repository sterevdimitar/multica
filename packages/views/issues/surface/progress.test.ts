import { describe, expect, it } from "vitest";
import type { IssueProgressTask } from "@multica/core/api/schemas";
import {
  completedTotals,
  displayTokens,
  formatTokens,
  formatTurns,
  knownTokens,
  shortFailureReason,
  groupProgressRows,
  shortStepName,
  formatCost,
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
    cost_usd: 0.1,
    max_turns: 0,
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

  // A task that is queued behind another run is not being worked on. The
  // card face already says "Queued" for it; the popover must not say ▶.
  it("marks a queued-only group as queued with no timer anchor", () => {
    const rows = groupProgressRows([
      task({ agent_name: "review-agent" }),
      task({
        agent_name: "readiness-agent",
        status: "queued",
        is_live: true,
        started_at: null,
        completed_at: null,
        queued_at: "2026-08-22T10:10:00Z",
      }),
    ]);
    const queued = rows[1]!;
    expect(queued.status).toBe("queued");
    expect(queued.liveStartedAt).toBeNull();
    expect(queued.elapsedMs).toBeNull();
  });

  // The runner has been dispatched but claude is not running yet: still
  // waiting, same predicate as the badge in surface/activity.ts.
  it("treats dispatched as queued, not live", () => {
    const rows = groupProgressRows([
      task({ agent_name: "fixer", status: "dispatched", is_live: true, started_at: null, completed_at: null }),
    ]);
    expect(rows[0]!.status).toBe("queued");
  });

  it("lets a running member win over a queued one in the same group", () => {
    const rows = groupProgressRows([
      task({
        task_id: "q",
        agent_name: "fixer",
        status: "queued",
        is_live: true,
        started_at: null,
        completed_at: null,
        queued_at: "2026-08-22T10:09:00Z",
      }),
      task({
        task_id: "r",
        agent_name: "fixer",
        status: "running",
        is_live: true,
        started_at: "2026-08-22T10:10:00Z",
        completed_at: null,
        queued_at: "2026-08-22T10:08:00Z",
      }),
    ]);
    expect(rows).toHaveLength(1);
    expect(rows[0]!.status).toBe("live");
    expect(rows[0]!.liveStartedAt).toBe("2026-08-22T10:10:00Z");
  });

  it("excludes a queued row from the completed totals", () => {
    const rows = groupProgressRows([
      task({ agent_name: "review-agent" }),
      task({ agent_name: "readiness-agent", status: "queued", is_live: true, started_at: null, completed_at: null, tokens: { input: 1, output: 1, cache_creation: 0, cache_read: 0 }, turns: 1 }),
    ]);
    const totals = completedTotals(rows);
    expect(totals.tokens).toBe(175);
    expect(totals.turns).toBe(3);
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

describe("turn caps", () => {
  it("sums the cap across a group, like turns", () => {
    const rows = groupProgressRows([
      task({ task_id: "a", turns: 30, max_turns: 40 }),
      task({ task_id: "b", turns: 12, max_turns: 40 }),
    ]);
    expect(rows[0]!.turns).toBe(42);
    expect(rows[0]!.maxTurns).toBe(80);
  });

  // One capless member makes the whole denominator unusable. Summing only
  // the members that declare a cap would print a budget smaller than the one
  // actually available, so a healthy group would read as an overrun.
  it("treats the group cap as unknown when any member declares none", () => {
    const trailing = groupProgressRows([
      task({ task_id: "a", turns: 30, max_turns: 40 }),
      task({ task_id: "b", turns: 12, max_turns: 0 }),
    ]);
    expect(trailing[0]!.maxTurns).toBe(0);

    // ...and the same when the capless member comes first, so the rule does
    // not depend on arrival order.
    const leading = groupProgressRows([
      task({ task_id: "a", turns: 12, max_turns: 0 }),
      task({ task_id: "b", turns: 30, max_turns: 40 }),
    ]);
    expect(leading[0]!.maxTurns).toBe(0);
  });
});

describe("formatTurns", () => {
  // The cap is deliberately NOT rendered. --max-turns counts tool-use turns
  // only, while `turns` also counts the final text turn and each parallel
  // tool call, so "21/20" invited readers to diagnose an overrun from two
  // quantities that were never comparable. A run that stops exactly at its
  // budget reports above it, and so can one that never came close.
  it("renders the bare count, never a ratio against the cap", () => {
    expect(formatTurns(21, 20)).toBe("21");
    expect(formatTurns(7, 40)).toBe("7");
    expect(formatTurns(7, 0)).toBe("7");
  });
});

describe("shortFailureReason", () => {
  it("shortens the reasons the pipeline records", () => {
    expect(shortFailureReason("claude-max-turns")).toBe("max turns");
    expect(shortFailureReason("claude-max-budget")).toBe("max budget");
    expect(shortFailureReason("gha-workflow-failure")).toBe("gha workflow failure");
  });

  // A reason this code has not heard of is still the run's own word, and is
  // better than showing nothing next to a ✗.
  it("passes through an unknown reason", () => {
    expect(shortFailureReason("something_new")).toBe("something new");
  });

  it("renders nothing for a run that did not fail", () => {
    expect(shortFailureReason("")).toBe("");
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

describe("cost", () => {
  it("sums a group's cost and leaves an unpriced group null, not $0", () => {
    const rows = groupProgressRows([
      task({ agent_name: "fixer", cost_usd: 0.1 }),
      task({ agent_name: "fixer", cost_usd: 0.05 }),
      task({ agent_name: "readiness-agent", cost_usd: null }),
    ]);
    expect(rows[0]!.costUsd).toBeCloseTo(0.15, 9);
    expect(rows[1]!.costUsd).toBeNull();
  });

  it("a group with one priced and one unpriced member sums the priced one", () => {
    const rows = groupProgressRows([
      task({ agent_name: "fixer", cost_usd: null }),
      task({ agent_name: "fixer", cost_usd: 0.05 }),
    ]);
    expect(rows[0]!.costUsd).toBeCloseTo(0.05, 9);
  });

  // I6: completed steps only, and null contributes nothing — never zero.
  it("totals completed steps only and skips null", () => {
    const rows = groupProgressRows([
      task({ agent_name: "review-agent", cost_usd: 0.2 }),
      task({ agent_name: "readiness-agent", cost_usd: null }),
      task({
        agent_name: "fixer",
        status: "running",
        is_live: true,
        completed_at: null,
        cost_usd: 1.0,
      }),
    ]);
    expect(completedTotals(rows).costUsd).toBeCloseTo(0.2, 9);
  });

  it("totals null when no completed step is priced", () => {
    const rows = groupProgressRows([
      task({ agent_name: "review-agent", cost_usd: null }),
      task({ agent_name: "fixer", cost_usd: null }),
    ]);
    expect(completedTotals(rows).costUsd).toBeNull();
  });
});

describe("formatCost", () => {
  it("renders a dash for unknown, never $0.00", () => {
    expect(formatCost(null)).toBe("—");
  });
  it("renders two decimals", () => {
    expect(formatCost(0.157)).toBe("$0.16");
    expect(formatCost(3.35)).toBe("$3.35");
    expect(formatCost(0.005)).toBe("$0.01");
  });
  it("renders sub-cent as <$0.01 rather than rounding to nothing", () => {
    expect(formatCost(0.004)).toBe("<$0.01");
  });
  it("renders a genuine zero as $0.00", () => {
    expect(formatCost(0)).toBe("$0.00");
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

  it("renders a dash for null, never 0", () => {
    expect(formatTokens(null)).toBe("—");
  });
});

describe("shortStepName", () => {
  it.each([
    ["review-agent", "review"],
    ["review-validator-agent", "validator"],
    ["fixer", "fixer"],
    ["readiness-agent", "readiness"],
    ["custom-agent", "custom-agent"],
  ])("maps %s to %s", (input, expected) => {
    expect(shortStepName(input)).toBe(expected);
  });
});

describe("knownTokens", () => {
  it("returns displayTokens when non-zero", () => {
    expect(knownTokens({ input: 100, output: 50, cache_creation: 25, cache_read: 9_000_000 })).toBe(175);
  });

  // 0 is never a measurement — every round trip bills input tokens — so an
  // all-zero figure is the "unknown" signal, not "free". Mirrors
  // use-task-metrics.ts's "any real work reports something".
  it("returns null when input/output/cache_creation are all zero, whatever cache_read says", () => {
    expect(knownTokens({ input: 0, output: 0, cache_creation: 0, cache_read: 912_000 })).toBeNull();
  });
});

describe("ProgressRow.tokens is null, not 0, for unmeasured usage", () => {
  // On the GLM route Claude Code's per-block usage is all-zero while turns
  // still move — a live step's tokens must read as unknown, not "free".
  it("is null for a single live member with all-zero tokens and turns > 0", () => {
    const rows = groupProgressRows([
      task({
        agent_name: "fixer",
        status: "running",
        is_live: true,
        started_at: "2026-08-22T10:10:00Z",
        completed_at: null,
        tokens: { input: 0, output: 0, cache_creation: 0, cache_read: 0 },
        turns: 5,
      }),
    ]);
    expect(rows[0]!.tokens).toBeNull();
    expect(rows[0]!.turns).toBe(5);
  });

  it("sums only the known member of a group with one all-zero and one measured", () => {
    const rows = groupProgressRows([
      task({
        agent_name: "fixer",
        tokens: { input: 0, output: 0, cache_creation: 0, cache_read: 0 },
        turns: 5,
      }),
      task({
        agent_name: "fixer",
        tokens: { input: 100, output: 50, cache_creation: 25, cache_read: 0 },
        turns: 3,
      }),
    ]);
    expect(rows[0]!.tokens).toBe(175);
  });

  it("is null when every member of a group is all-zero", () => {
    const rows = groupProgressRows([
      task({
        agent_name: "fixer",
        tokens: { input: 0, output: 0, cache_creation: 0, cache_read: 0 },
        turns: 5,
      }),
      task({
        agent_name: "fixer",
        tokens: { input: 0, output: 0, cache_creation: 0, cache_read: 0 },
        turns: 2,
      }),
    ]);
    expect(rows[0]!.tokens).toBeNull();
  });
});

describe("completedTotals(rows).tokens is null, not 0, for unmeasured usage", () => {
  it("sums the numeric row and skips the null one", () => {
    const rows = groupProgressRows([
      task({
        agent_name: "review-agent",
        tokens: { input: 0, output: 0, cache_creation: 0, cache_read: 0 },
        turns: 4,
      }),
      task({
        agent_name: "fixer",
        tokens: { input: 100, output: 50, cache_creation: 25, cache_read: 0 },
      }),
    ]);
    expect(completedTotals(rows).tokens).toBe(175);
  });

  it("is null when every completed row is null — three dashes must total a dash", () => {
    const rows = groupProgressRows([
      task({
        agent_name: "review-agent",
        tokens: { input: 0, output: 0, cache_creation: 0, cache_read: 0 },
        turns: 4,
      }),
      task({
        agent_name: "fixer",
        tokens: { input: 0, output: 0, cache_creation: 0, cache_read: 0 },
        turns: 2,
      }),
    ]);
    expect(completedTotals(rows).tokens).toBeNull();
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
