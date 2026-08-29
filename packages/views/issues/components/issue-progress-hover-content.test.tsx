// @vitest-environment jsdom

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, screen } from "@testing-library/react";
import type { ReactElement } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { IssueProgress } from "@multica/core/api/schemas";
import { renderWithI18n } from "../../test/i18n";

const mockState = vi.hoisted(() => ({
  progress: undefined as IssueProgress | undefined,
}));

// The component fetches on open; the test supplies the response through
// initialData so every assertion is synchronous and deterministic.
vi.mock("@multica/core/issues/queries", () => ({
  issueProgressOptions: (issueId: string) => ({
    queryKey: ["issues", "progress", issueId],
    queryFn: () => mockState.progress,
    initialData: mockState.progress,
    staleTime: Infinity,
  }),
}));

import { IssueProgressHoverContent } from "./issue-progress-hover-content";

function renderContent(ui: ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithI18n(
    <QueryClientProvider client={qc}>{ui}</QueryClientProvider>,
  );
}

/** Cell texts of the table row whose Step cell contains `label`. Scoping by
 *  row matters: the footer repeats the same numbers, so a bare getByText
 *  would be ambiguous exactly when the table is working. */
function rowCells(label: string): string[] {
  const cell = screen
    .getAllByRole("cell")
    .find((c) => c.textContent?.includes(label) && c.closest("tbody"));
  if (!cell) throw new Error(`no row for ${label}`);
  const row = cell.closest("tr")!;
  return Array.from(row.querySelectorAll("td")).map((td) => td.textContent ?? "");
}

function tokens(input = 0, output = 0, cache_creation = 0, cache_read = 0) {
  return { input, output, cache_creation, cache_read };
}

const NOW = Date.parse("2026-08-22T10:11:30Z");

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(NOW);
});

afterEach(() => {
  vi.useRealTimers();
  cleanup();
  mockState.progress = undefined;
});

describe("IssueProgressHoverContent", () => {
  it("renders a live row whose timer advances, with formatted tokens", () => {
    mockState.progress = {
      tasks: [
        {
          task_id: "t1",
          agent_name: "review-agent",
          status: "completed",
          queued_at: "2026-08-22T10:00:00Z",
          started_at: "2026-08-22T10:00:00Z",
          completed_at: "2026-08-22T10:04:00Z",
          tokens: tokens(30_000, 8_000, 234, 900_000),
          turns: 14,
          is_live: false,
        },
        {
          task_id: "t2",
          agent_name: "fixer",
          status: "running",
          queued_at: "2026-08-22T10:09:00Z",
          started_at: "2026-08-22T10:10:00Z",
          completed_at: null,
          tokens: tokens(1_000, 200, 34, 5_000),
          turns: 3,
          is_live: true,
        },
      ],
      expected_steps: null,
      // No skew in this case.
      server_now: "2026-08-22T10:11:30Z",
    };

    const { rerender } = renderContent(<IssueProgressHoverContent issueId="i1" />);

    // 30000 + 8000 + 234 = 38234 → "38.2k" (cache reads excluded)
    expect(rowCells("review")).toEqual(["✓review", "4:00", "38.2k", "14"]);
    // live row started 10:10:00, now 10:11:30 → 1:30
    expect(rowCells("fixer")).toEqual(["▶fixer", "1:30", "1.2k", "3"]);

    vi.advanceTimersByTime(1000);
    rerender(<div />); // flush the interval-driven state update
    expect(screen.queryByText("1:30")).toBeNull();
  });

  // A finished run is not automatically a successful one. Before this, every
  // non-live row drew "✓", so a review that died on max-turns was
  // indistinguishable from one that completed cleanly.
  it("marks failed and cancelled steps distinctly from a clean completion", () => {
    mockState.progress = {
      tasks: [
        {
          task_id: "t1",
          agent_name: "review-agent",
          status: "failed",
          queued_at: "2026-08-22T10:00:00Z",
          started_at: "2026-08-22T10:00:00Z",
          completed_at: "2026-08-22T10:06:00Z",
          tokens: tokens(100, 50, 25, 900),
          turns: 21,
          is_live: false,
        },
        {
          task_id: "t2",
          agent_name: "fixer",
          status: "cancelled",
          queued_at: "2026-08-22T10:07:00Z",
          started_at: "2026-08-22T10:07:00Z",
          completed_at: "2026-08-22T10:07:30Z",
          tokens: tokens(10, 5, 0, 0),
          turns: 1,
          is_live: false,
        },
        {
          task_id: "t3",
          agent_name: "readiness-agent",
          status: "completed",
          queued_at: "2026-08-22T10:08:00Z",
          started_at: "2026-08-22T10:08:00Z",
          completed_at: "2026-08-22T10:09:00Z",
          tokens: tokens(20, 10, 0, 0),
          turns: 2,
          is_live: false,
        },
      ],
      expected_steps: null,
      server_now: "2026-08-22T10:11:30Z",
    };

    renderContent(<IssueProgressHoverContent issueId="i1" />);

    expect(rowCells("review")[0]).toBe("✗review");
    expect(rowCells("fixer")[0]).toBe("⊘fixer");
    expect(rowCells("readiness")[0]).toBe("✓readiness");
  });

  it("renders history with no live row and a Completed footer when nothing runs", () => {
    mockState.progress = {
      tasks: [
        {
          task_id: "t1",
          agent_name: "review-agent",
          status: "completed",
          queued_at: "2026-08-22T10:00:00Z",
          started_at: "2026-08-22T10:00:00Z",
          completed_at: "2026-08-22T10:02:00Z",
          tokens: tokens(100, 50, 25, 900),
          turns: 4,
          is_live: false,
        },
      ],
      expected_steps: null,
      server_now: "2026-08-22T10:11:30Z",
    };

    renderContent(<IssueProgressHoverContent issueId="i1" />);

    expect(screen.getByText("Completed")).toBeTruthy();
    // 2 minutes of completed elapsed
    expect(screen.getAllByText("2:00").length).toBeGreaterThan(0);
    expect(screen.getAllByText("175").length).toBeGreaterThan(0);
  });

  it("omits pending rows and the step counter when expected_steps is null", () => {
    mockState.progress = {
      tasks: [
        {
          task_id: "t1",
          agent_name: "review-agent",
          status: "completed",
          queued_at: "2026-08-22T10:00:00Z",
          started_at: "2026-08-22T10:00:00Z",
          completed_at: "2026-08-22T10:02:00Z",
          tokens: tokens(1, 1, 1, 1),
          turns: 1,
          is_live: false,
        },
      ],
      expected_steps: null,
      server_now: "2026-08-22T10:11:30Z",
    };

    renderContent(<IssueProgressHoverContent issueId="i1" />);

    expect(screen.queryByText(/Step \d+ of \d+/)).toBeNull();
    expect(screen.queryByText("validator")).toBeNull();
  });

  it("shows pending rows and the step counter when expected_steps is known", () => {
    mockState.progress = {
      tasks: [
        {
          task_id: "t1",
          agent_name: "review-agent",
          status: "completed",
          queued_at: "2026-08-22T10:00:00Z",
          started_at: "2026-08-22T10:00:00Z",
          completed_at: "2026-08-22T10:02:00Z",
          tokens: tokens(1, 1, 1, 1),
          turns: 1,
          is_live: false,
        },
      ],
      expected_steps: ["review-agent", "review-validator-agent", "fixer"],
      server_now: "2026-08-22T10:11:30Z",
    };

    renderContent(<IssueProgressHoverContent issueId="i1" />);

    expect(screen.getByText("Step 2 of 3")).toBeTruthy();
    expect(screen.getByText("validator")).toBeTruthy();
    expect(screen.getByText("fixer")).toBeTruthy();
  });

  it("collapses consecutive same-agent runs into one ×N row", () => {
    mockState.progress = {
      tasks: [0, 1, 2].map((i) => ({
        task_id: `t${i}`,
        agent_name: "fixer",
        status: "completed",
        queued_at: `2026-08-22T10:0${i}:00Z`,
        started_at: `2026-08-22T10:0${i}:00Z`,
        completed_at: `2026-08-22T10:0${i + 1}:00Z`,
        tokens: tokens(100, 50, 25, 900),
        turns: 2,
        is_live: false,
      })),
      expected_steps: null,
      server_now: "2026-08-22T10:11:30Z",
    };

    renderContent(<IssueProgressHoverContent issueId="i1" />);

    expect(screen.getByText("×3")).toBeTruthy();
    // 3 × 175 tokens, 3 × 2 turns, 3 × 60s
    expect(screen.getAllByText("525").length).toBeGreaterThan(0);
    expect(screen.getAllByText("3:00").length).toBeGreaterThan(0);
  });

  it("corrects the live elapsed for clock skew using server_now", () => {
    mockState.progress = {
      tasks: [
        {
          task_id: "t2",
          agent_name: "fixer",
          status: "running",
          queued_at: "2026-08-22T10:09:00Z",
          started_at: "2026-08-22T10:10:00Z",
          completed_at: null,
          tokens: tokens(0, 0, 0, 0),
          turns: 0,
          is_live: true,
        },
      ],
      expected_steps: null,
      // Server is 90s behind this client's clock: naive elapsed would read
      // 1:30, the corrected one reads 0:00.
      server_now: "2026-08-22T10:10:00Z",
    };

    renderContent(<IssueProgressHoverContent issueId="i1" />);

    expect(rowCells("fixer")[1]).toBe("0:00");
    expect(screen.queryByText("1:30")).toBeNull();
  });
});

describe("IssueProgressHoverContent — no runs", () => {
  it("renders an empty state instead of an empty table", () => {
    mockState.progress = {
      tasks: [],
      expected_steps: null,
      server_now: "2026-08-22T10:11:30Z",
    };

    renderContent(<IssueProgressHoverContent issueId="i1" />);

    expect(screen.getByText("No agent runs yet")).toBeTruthy();
    expect(document.querySelector("table")).toBeNull();
  });

  it("renders the localized empty state", () => {
    mockState.progress = {
      tasks: [],
      expected_steps: null,
      server_now: "2026-08-22T10:11:30Z",
    };

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    renderWithI18n(
      <QueryClientProvider client={qc}>
        <IssueProgressHoverContent issueId="i1" />
      </QueryClientProvider>,
      { locale: "zh-Hans" },
    );

    expect(screen.getByText("尚无智能体运行记录")).toBeTruthy();
  });

  it("renders the table for an issue with only historical runs", () => {
    mockState.progress = {
      tasks: [
        {
          task_id: "t1",
          agent_name: "review-agent",
          status: "completed",
          queued_at: "2026-08-22T10:00:00Z",
          started_at: "2026-08-22T10:00:00Z",
          completed_at: "2026-08-22T10:02:00Z",
          tokens: tokens(100, 50, 25, 900),
          turns: 4,
          is_live: false,
        },
      ],
      expected_steps: null,
      server_now: "2026-08-22T10:11:30Z",
    };

    renderContent(<IssueProgressHoverContent issueId="i1" />);

    // A card with no ACTIVE task still gets its history — that is the whole
    // reason the trigger is unconditional.
    expect(document.querySelector("table")).toBeTruthy();
    expect(rowCells("review")).toEqual(["✓review", "2:00", "175", "4"]);
    expect(screen.queryByText("No agent runs yet")).toBeNull();
  });
});
