// @vitest-environment jsdom

import { cleanup, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { AgentTask } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";

const mockState = vi.hoisted(() => ({
  snapshot: [] as AgentTask[],
  liveUsage: undefined as unknown,
}));

vi.mock("@multica/core/hooks", () => ({
  useWorkspaceId: () => "ws-1",
}));

vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({
    getActorName: (_type: string, id: string) =>
      ({ "agent-1": "review-agent" })[id] ?? "Unknown Agent",
    getActorInitials: () => "RA",
    getActorAvatarUrl: () => undefined,
  }),
}));

vi.mock("@multica/core/agents", () => ({
  agentTaskSnapshotOptions: () => ({ queryKey: ["agent-task-snapshot"] }),
}));

vi.mock("../../agents/components/agent-avatar-stack", () => ({
  AgentAvatarStack: () => <span data-testid="avatar-stack" />,
}));

// The popover body is exercised by its own test file; here we only care that
// the trigger exists and that the card face is right.
vi.mock("./issue-progress-hover-content", () => ({
  IssueProgressHoverContent: ({ issueId }: { issueId: string }) => (
    <div data-testid="progress-hover">{issueId}</div>
  ),
  formatElapsed: (ms: number | null) =>
    ms === null ? "—" : `${Math.floor(ms / 60000)}:${String(Math.floor(ms / 1000) % 60).padStart(2, "0")}`,
}));

vi.mock("@tanstack/react-query", async () => {
  const actual =
    await vi.importActual<typeof import("@tanstack/react-query")>(
      "@tanstack/react-query",
    );
  return {
    ...actual,
    useQuery: (opts: {
      queryKey?: readonly unknown[];
      select?: (d: unknown) => unknown;
    }) => {
      if (opts.queryKey?.[0] === "agent-task-snapshot") {
        return {
          data: opts.select
            ? opts.select(mockState.snapshot)
            : mockState.snapshot,
        };
      }
      if (opts.queryKey?.[1] === "live-usage") {
        return { data: mockState.liveUsage };
      }
      return { data: undefined };
    },
  };
});

import { IssueAgentActivityIndicator } from "./issue-agent-activity-indicator";

function makeTask(overrides: Partial<AgentTask> = {}): AgentTask {
  return {
    id: "task-1",
    agent_id: "agent-1",
    runtime_id: "runtime-1",
    issue_id: "issue-1",
    status: "running",
    priority: 0,
    dispatched_at: null,
    started_at: "2026-08-22T10:10:00Z",
    completed_at: null,
    result: null,
    error: null,
    created_at: "2026-08-22T10:09:00Z",
    ...overrides,
  } as AgentTask;
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(new Date("2026-08-22T10:11:30Z"));
  mockState.snapshot = [];
  mockState.liveUsage = undefined;
});

afterEach(() => {
  vi.useRealTimers();
  cleanup();
});

describe("IssueAgentActivityIndicator", () => {
  it("renders a hover trigger on a card with no active tasks", () => {
    // Previously this returned null. The trigger is now unconditional so a
    // finished or never-run issue still offers its history on hover — the
    // board snapshot cannot tell the two apart without a per-card fetch.
    const { container } = renderWithI18n(
      <IssueAgentActivityIndicator issueId="issue-1" />,
    );

    expect(container.firstChild).not.toBeNull();
    expect(screen.getByLabelText("Agent run history")).toBeTruthy();
    // No badge line and no avatars when nothing is active.
    expect(screen.queryByTestId("avatar-stack")).toBeNull();
    expect(screen.queryByText("Working")).toBeNull();
    expect(container.textContent).not.toContain("⏱");
  });

  it("keeps the running badge line unchanged", () => {
    mockState.snapshot = [makeTask()];

    const { container } = renderWithI18n(
      <IssueAgentActivityIndicator issueId="issue-1" />,
    );

    expect(screen.getByTestId("avatar-stack")).toBeTruthy();
    expect(screen.getByText("Working")).toBeTruthy();
    // started 10:10:00, now 10:11:30 → 1:30; no token glyph before the first
    // usage flush lands in the live-usage cache. The step label is the SHORT
    // name (review-agent → review), not the raw agent name.
    expect(container.textContent).toContain("review ⏱ 1:30");
    expect(container.textContent).not.toContain("🪙");
    expect(screen.queryByLabelText("Agent run history")).toBeNull();
  });

  it("adds the token count once a usage flush has landed for that task", () => {
    mockState.snapshot = [makeTask()];
    mockState.liveUsage = {
      task_id: "task-1",
      tokens: { input: 30_000, output: 8_000, cache_creation: 234, cache_read: 9_000_000 },
      turns: 3,
    };

    const { container } = renderWithI18n(
      <IssueAgentActivityIndicator issueId="issue-1" />,
    );

    expect(container.textContent).toContain("🪙 38.2k");
  });

  // On the Deep Infra / GLM route the per-block usage is all-zero while
  // turns still move. The card face must never show "🪙 0" for that step.
  it("shows no token glyph when the live-usage entry is all-zero", () => {
    mockState.snapshot = [makeTask()];
    mockState.liveUsage = {
      task_id: "task-1",
      tokens: { input: 0, output: 0, cache_creation: 0, cache_read: 0 },
      turns: 5,
    };

    const { container } = renderWithI18n(
      <IssueAgentActivityIndicator issueId="issue-1" />,
    );

    expect(container.textContent).not.toContain("🪙");
  });

  it("ignores a live-usage entry belonging to a previous task", () => {
    mockState.snapshot = [makeTask()];
    mockState.liveUsage = {
      task_id: "some-older-task",
      tokens: { input: 999, output: 999, cache_creation: 999, cache_read: 0 },
      turns: 9,
    };

    const { container } = renderWithI18n(
      <IssueAgentActivityIndicator issueId="issue-1" />,
    );

    expect(container.textContent).not.toContain("🪙");
  });
});
