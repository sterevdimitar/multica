/**
 * @vitest-environment jsdom
 */
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook } from "@testing-library/react";
import type { ReactNode } from "react";
import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import type { WSClient } from "../api/ws-client";
import { defaultStorage } from "../platform/storage";
import { workspaceKeys } from "../workspace/queries";
import {
  markWorkspaceDeletePending,
  unmarkWorkspaceDeletePending,
} from "../workspace/pending-delete";
import { useRealtimeSync, type RealtimeSyncStores } from "./use-realtime-sync";

vi.mock("../platform/workspace-storage", () => ({
  getCurrentWsId: () => "ws-1",
  getCurrentSlug: () => "test-ws",
}));

vi.mock("../paths", () => ({
  useHasOnboarded: () => true,
  resolvePostAuthDestination: () => "/",
}));

function createMockWs(): WSClient {
  return {
    on: vi.fn(() => () => {}),
    onAny: vi.fn(() => () => {}),
    onReconnect: vi.fn(() => () => {}),
  } as unknown as WSClient;
}

function createStores(): RealtimeSyncStores {
  return {
    authStore: Object.assign(() => ({}), {
      getState: () => ({ user: { id: "u1" } }),
      subscribe: () => () => {},
      setState: () => {},
      destroy: () => {},
    }),
  } as unknown as RealtimeSyncStores;
}

function createWrapper(qc: QueryClient) {
  // Named function (not arrow) so react/display-name lint rule passes —
  // anonymous render-fn components break that rule even in test files.
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  };
}

describe("useRealtimeSync — ws instance change", () => {
  let qc: QueryClient;
  let stores: RealtimeSyncStores;
  let invalidateSpy: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    stores = createStores();
    invalidateSpy = vi.spyOn(qc, "invalidateQueries");
  });

  it("skips invalidation on first non-null ws instance", () => {
    const ws = createMockWs();
    renderHook(() => useRealtimeSync(ws, stores), {
      wrapper: createWrapper(qc),
    });

    // The main effect calls invalidateQueries for its own setup, but the
    // ws-instance-change effect should NOT have fired invalidation.
    // The only invalidateQueries calls should come from the main effect's
    // event handlers, not from the instance-change effect.
    // We verify by checking that no call was made with workspaceKeys.list()
    // pattern from the instance-change path (it logs a specific message).
    // Simpler: count calls — first mount with a ws should not trigger the
    // workspace-scoped bulk invalidation.
    expect(invalidateSpy).not.toHaveBeenCalled();
  });

  it("does not invalidate when ws goes from instance to null", () => {
    const ws1 = createMockWs();
    const { rerender } = renderHook(
      ({ ws }) => useRealtimeSync(ws, stores),
      { initialProps: { ws: ws1 as WSClient | null }, wrapper: createWrapper(qc) },
    );

    invalidateSpy.mockClear();
    rerender({ ws: null });

    expect(invalidateSpy).not.toHaveBeenCalled();
  });

  it("invalidates exactly once when a new ws instance appears after null gap", () => {
    const ws1 = createMockWs();
    const { rerender } = renderHook(
      ({ ws }) => useRealtimeSync(ws, stores),
      { initialProps: { ws: ws1 as WSClient | null }, wrapper: createWrapper(qc) },
    );

    // Simulate workspace switch: ws -> null -> new ws
    invalidateSpy.mockClear();
    rerender({ ws: null });
    expect(invalidateSpy).not.toHaveBeenCalled();

    const ws2 = createMockWs();
    rerender({ ws: ws2 });

    // Should have called invalidateQueries for all workspace-scoped keys
    // (16 workspace-scoped [incl. property definitions] + 6 per-issue
    // prefixes + 5 per-chat prefixes + 1 workspaceKeys.list() + 1
    // cross-workspace inbox unread summary = 29 calls)
    expect(invalidateSpy).toHaveBeenCalledTimes(29);
  });

  it("does not re-invalidate when rerendered with the same ws instance", () => {
    const ws1 = createMockWs();
    const { rerender } = renderHook(
      ({ ws }) => useRealtimeSync(ws, stores),
      { initialProps: { ws: ws1 as WSClient | null }, wrapper: createWrapper(qc) },
    );

    invalidateSpy.mockClear();
    // Rerender with same instance
    rerender({ ws: ws1 });

    expect(invalidateSpy).not.toHaveBeenCalled();
  });

  it("invalidates chat, pins, labels, and invitations queries on ws instance change", () => {
    const ws1 = createMockWs();
    const { rerender } = renderHook(
      ({ ws }) => useRealtimeSync(ws, stores),
      { initialProps: { ws: ws1 as WSClient | null }, wrapper: createWrapper(qc) },
    );

    invalidateSpy.mockClear();
    rerender({ ws: null });

    const ws2 = createMockWs();
    rerender({ ws: ws2 });

    const calls = invalidateSpy.mock.calls.map((call: [{ queryKey?: unknown }, ...unknown[]]) => call[0].queryKey);
    expect(calls).toContainEqual(["chat", "ws-1"]);
    expect(calls).toContainEqual(["labels", "ws-1"]);
    expect(calls).toContainEqual(["workspaces", "ws-1", "invitations"]);
  });

  it("invalidates per-issue caches (no wsId in key) on ws instance change", () => {
    // These keys are not under the ["issues", wsId] prefix, so they need
    // their own invalidation on recovery — otherwise events missed while
    // disconnected leave them stale forever (staleTime: Infinity, #3953).
    const ws1 = createMockWs();
    const { rerender } = renderHook(
      ({ ws }) => useRealtimeSync(ws, stores),
      { initialProps: { ws: ws1 as WSClient | null }, wrapper: createWrapper(qc) },
    );

    invalidateSpy.mockClear();
    rerender({ ws: null });

    const ws2 = createMockWs();
    rerender({ ws: ws2 });

    const calls = invalidateSpy.mock.calls.map((call: [{ queryKey?: unknown }, ...unknown[]]) => call[0].queryKey);
    expect(calls).toContainEqual(["issues", "timeline"]);
    expect(calls).toContainEqual(["issues", "reactions"]);
    expect(calls).toContainEqual(["issues", "subscribers"]);
    expect(calls).toContainEqual(["issues", "usage"]);
    expect(calls).toContainEqual(["issues", "attachments"]);
    expect(calls).toContainEqual(["issues", "tasks"]);
  });

  it("invalidates per-chat-session caches (no wsId in key) on ws instance change", () => {
    // These keys are not under the ["chat", wsId] prefix, so they need their
    // own recovery invalidation when reconnecting after missed chat/task events.
    const ws1 = createMockWs();
    const { rerender } = renderHook(
      ({ ws }) => useRealtimeSync(ws, stores),
      { initialProps: { ws: ws1 as WSClient | null }, wrapper: createWrapper(qc) },
    );

    invalidateSpy.mockClear();
    rerender({ ws: null });

    const ws2 = createMockWs();
    rerender({ ws: ws2 });

    const calls = invalidateSpy.mock.calls.map((call: [{ queryKey?: unknown }, ...unknown[]]) => call[0].queryKey);
    expect(calls).toContainEqual(["chat", "messages"]);
    expect(calls).toContainEqual(["chat", "messages-page"]);
    expect(calls).toContainEqual(["chat", "pending-task"]);
    expect(calls).toContainEqual(["task-messages"]);
  });
});

describe("useRealtimeSync — workspace:deleted self-initiated suppression", () => {
  let qc: QueryClient;
  let stores: RealtimeSyncStores;

  beforeEach(() => {
    qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    stores = createStores();
  });

  afterEach(() => {
    unmarkWorkspaceDeletePending("ws-2");
    localStorage.clear();
  });

  // getCurrentWsId is mocked to "ws-1" at module level, so deleting "ws-2"
  // never enters the relocate branch — these tests only exercise the
  // storage-cleanup path, which is the observable difference between a
  // handled and a suppressed event.
  const dispatchWorkspaceDeleted = (ws: WSClient, workspaceId: string) => {
    const call = vi
      .mocked(ws.on)
      .mock.calls.find(([event]) => event === "workspace:deleted");
    expect(call).toBeDefined();
    (call![1] as (p: unknown) => void)({ workspace_id: workspaceId });
  };

  it("ignores the event for a delete this client initiated", () => {
    const ws = createMockWs();
    renderHook(() => useRealtimeSync(ws, stores), {
      wrapper: createWrapper(qc),
    });
    qc.setQueryData(workspaceKeys.list(), [{ id: "ws-2", slug: "delete-me" }]);
    defaultStorage.setItem("multica_issue_draft:delete-me", "draft");

    markWorkspaceDeletePending("ws-2");
    dispatchWorkspaceDeleted(ws, "ws-2");

    // useDeleteWorkspace.onSuccess owns cleanup for self-initiated deletes;
    // the handler must not have touched storage.
    expect(defaultStorage.getItem("multica_issue_draft:delete-me")).toBe("draft");
  });

  it("still cleans up for a delete initiated elsewhere", () => {
    const ws = createMockWs();
    renderHook(() => useRealtimeSync(ws, stores), {
      wrapper: createWrapper(qc),
    });
    qc.setQueryData(workspaceKeys.list(), [{ id: "ws-2", slug: "delete-me" }]);
    defaultStorage.setItem("multica_issue_draft:delete-me", "draft");

    dispatchWorkspaceDeleted(ws, "ws-2");

    expect(defaultStorage.getItem("multica_issue_draft:delete-me")).toBeNull();
  });
});

// An emittable WS mock: records every ws.on / ws.onAny handler so a test can
// push a real event through the hook's subscription wiring.
function createEmittableWs() {
  const handlers = new Map<string, ((p: unknown) => void)[]>();
  const anyHandlers: ((m: { type: string; payload: unknown }) => void)[] = [];
  const ws = {
    on: (type: string, h: (p: unknown) => void) => {
      const list = handlers.get(type) ?? [];
      list.push(h);
      handlers.set(type, list);
      return () => {};
    },
    onAny: (h: (m: { type: string; payload: unknown }) => void) => {
      anyHandlers.push(h);
      return () => {};
    },
    onReconnect: () => () => {},
  } as unknown as WSClient;

  return {
    ws,
    emit(type: string, payload: unknown) {
      for (const h of handlers.get(type) ?? []) h(payload);
      for (const h of anyHandlers) h({ type, payload });
    },
  };
}

describe("useRealtimeSync — task:usage", () => {
  let qc: QueryClient;
  let stores: RealtimeSyncStores;

  beforeEach(() => {
    qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    stores = createStores();
  });

  const usagePayload = {
    task_id: "t2",
    issue_id: "i1",
    tokens: { input: 12000, output: 3100, cache_creation: 900, cache_read: 400000 },
    turns: 7,
  };

  it("writes the progress and live-usage caches", () => {
    const { ws, emit } = createEmittableWs();
    renderHook(() => useRealtimeSync(ws, stores), { wrapper: createWrapper(qc) });

    const setSpy = vi.spyOn(qc, "setQueryData");
    emit("task:usage", usagePayload);

    const keys = setSpy.mock.calls.map((c) => JSON.stringify(c[0]));
    expect(keys).toContain(JSON.stringify(["issues", "progress", "i1"]));
    expect(keys).toContain(JSON.stringify(["issues", "live-usage", "i1"]));
    expect(qc.getQueryData(["issues", "live-usage", "i1"])).toEqual({
      task_id: "t2",
      tokens: usagePayload.tokens,
      turns: 7,
    });
  });

  it("does not feed the debounced task: prefix invalidator", () => {
    vi.useFakeTimers();
    try {
      const { ws, emit } = createEmittableWs();
      renderHook(() => useRealtimeSync(ws, stores), { wrapper: createWrapper(qc) });

      const invalidateSpy = vi.spyOn(qc, "invalidateQueries");
      emit("task:usage", usagePayload);
      // Let any debounce window elapse — a 30s flush cadence must not fire
      // the six-query task: fan-out per tick.
      vi.advanceTimersByTime(5_000);

      expect(invalidateSpy).not.toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });
});

// The progress popover's query key is written by exactly one thing — the
// task:usage fold above — and that fold never fabricates a row. So while the
// popover is open, a step that COMPLETES kept rendering as live and a step
// that STARTED afterwards never appeared (MUL-124, 2026-08-30: `review` still
// "running" while the header chip already showed the readiness judge agent).
// Lifecycle events must make the projection refetch; task:usage must not.
describe("useRealtimeSync — task lifecycle refreshes the progress projection", () => {
  let qc: QueryClient;
  let stores: RealtimeSyncStores;

  beforeEach(() => {
    qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    stores = createStores();
  });

  const progressKeys = (spy: { mock: { calls: unknown[][] } }) =>
    spy.mock.calls
      .map((c: unknown[]) => (c[0] as { queryKey?: unknown } | undefined)?.queryKey)
      .filter((k: unknown) => JSON.stringify(k) === JSON.stringify(["issues", "progress"]));

  for (const type of ["task:running", "task:completed", "task:failed", "task:cancelled"]) {
    it(`refetches every issue's progress on ${type}`, () => {
      vi.useFakeTimers();
      try {
        const { ws, emit } = createEmittableWs();
        renderHook(() => useRealtimeSync(ws, stores), { wrapper: createWrapper(qc) });

        const invalidateSpy = vi.spyOn(qc, "invalidateQueries");
        emit(type, { task_id: "t9", issue_id: "i1" });
        // The task: prefix path is debounced; let it fire.
        vi.advanceTimersByTime(500);

        expect(progressKeys(invalidateSpy)).toHaveLength(1);
      } finally {
        vi.useRealTimers();
      }
    });
  }

  it("does not refetch progress on task:usage (a 30s tick per running task)", () => {
    vi.useFakeTimers();
    try {
      const { ws, emit } = createEmittableWs();
      renderHook(() => useRealtimeSync(ws, stores), { wrapper: createWrapper(qc) });

      const invalidateSpy = vi.spyOn(qc, "invalidateQueries");
      emit("task:usage", {
        task_id: "t2",
        issue_id: "i1",
        tokens: { input: 1, output: 1, cache_creation: 0, cache_read: 0 },
        turns: 1,
      });
      vi.advanceTimersByTime(5_000);

      expect(progressKeys(invalidateSpy)).toHaveLength(0);
    } finally {
      vi.useRealTimers();
    }
  });
});
