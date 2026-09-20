// @vitest-environment jsdom

import { describe, it, expect, vi, beforeEach } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import type { AgentRuntime } from "@multica/core/types";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enRuntimes from "../../locales/en/runtimes.json";
import { PlacementCard, parseCapInput, rankAmongWebhookRuntimes } from "./placement-card";

const TEST_RESOURCES = { en: { common: enCommon, runtimes: enRuntimes } };

const mockUpdateRuntime = vi.hoisted(() => vi.fn());
const mockRuntimes = vi.hoisted(() => ({ list: [] as AgentRuntime[] }));

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/api", () => ({
  api: {
    updateRuntime: (...args: unknown[]) => mockUpdateRuntime(...args),
    listRuntimes: () => Promise.resolve(mockRuntimes.list),
  },
  ApiError: class ApiError extends Error {},
}));
vi.mock("@multica/core/runtimes/queries", () => ({
  runtimeKeys: { all: (ws: string) => ["runtimes", ws], list: (ws: string) => ["runtimes", ws, "list"] },
  runtimeListOptions: (ws: string) => ({ queryKey: ["runtimes", ws, "list"], queryFn: () => mockRuntimes.list }),
}));
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

function webhook(id: string, order: number, extra: Partial<AgentRuntime> = {}): AgentRuntime {
  return {
    id,
    workspace_id: "ws-1",
    daemon_id: `${id}-runner`,
    name: id,
    runtime_mode: "webhook",
    provider: "claude",
    launch_header: "",
    status: "online",
    device_info: "",
    metadata: {},
    owner_id: "u1",
    visibility: "private",
    last_seen_at: null,
    created_at: `2026-09-1${order}T00:00:00Z`,
    updated_at: "2026-09-19T00:00:00Z",
    dispatch_order: order,
    max_concurrent_tasks: null,
    ...extra,
  };
}

function renderCard(runtime: AgentRuntime, canEdit = true) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <PlacementCard runtime={runtime} canEdit={canEdit} />
      </I18nProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  mockUpdateRuntime.mockReset();
  mockUpdateRuntime.mockResolvedValue({});
  mockRuntimes.list = [webhook("local-pc", 0), webhook("circleci", 1), webhook("dpm-laptop", 2)];
});

describe("parseCapInput", () => {
  it("maps blank to null, digits to a number, anything else to undefined", () => {
    expect(parseCapInput("")).toBeNull();
    expect(parseCapInput("  ")).toBeNull();
    expect(parseCapInput("0")).toBe(0);
    expect(parseCapInput("3")).toBe(3);
    expect(parseCapInput("-1")).toBeUndefined();
    expect(parseCapInput("1.5")).toBeUndefined();
    expect(parseCapInput("x")).toBeUndefined();
  });
});

describe("rankAmongWebhookRuntimes", () => {
  it("ranks by dispatch_order among webhook runtimes only", () => {
    const all = [...mockRuntimes.list, { ...webhook("daemon", 0), runtime_mode: "local" as const }];
    expect(rankAmongWebhookRuntimes(all[1]!, all)).toEqual({ rank: 2, total: 3 });
    expect(rankAmongWebhookRuntimes(all[3]!, all)).toBeNull();
  });
});

describe("PlacementCard", () => {
  it("posts a number on blur and null for blank", async () => {
    renderCard(webhook("circleci", 1, { max_concurrent_tasks: 3 }));
    const input = screen.getByLabelText("Runs at once") as HTMLInputElement;
    expect(input.value).toBe("3");

    fireEvent.change(input, { target: { value: "2" } });
    fireEvent.blur(input);
    await waitFor(() =>
      expect(mockUpdateRuntime).toHaveBeenCalledWith("circleci", { max_concurrent_tasks: 2 }),
    );

    fireEvent.change(input, { target: { value: "" } });
    fireEvent.blur(input);
    await waitFor(() =>
      expect(mockUpdateRuntime).toHaveBeenCalledWith("circleci", { max_concurrent_tasks: null }),
    );
  });

  it("does not submit a negative value or an unchanged one", async () => {
    renderCard(webhook("circleci", 1, { max_concurrent_tasks: 3 }));
    const input = screen.getByLabelText("Runs at once") as HTMLInputElement;
    fireEvent.change(input, { target: { value: "-1" } });
    fireEvent.blur(input);
    fireEvent.change(input, { target: { value: "3" } });
    fireEvent.blur(input);
    await new Promise((r) => setTimeout(r, 20));
    expect(mockUpdateRuntime).not.toHaveBeenCalled();
    expect(input.value).toBe("3");
  });

  it("shows the order read-only and the two notes; disables the input without edit rights", async () => {
    renderCard(webhook("circleci", 1), false);
    expect((screen.getByLabelText("Runs at once") as HTMLInputElement).disabled).toBe(true);
    expect(screen.getByText(/Multica does not start or stop containers/)).toBeInTheDocument();
    expect(screen.getByText(/0 takes this runtime out of the rotation/)).toBeInTheDocument();
    await waitFor(() =>
      expect(screen.getByTestId("placement-order")).toHaveTextContent("2 of 3 in the fallback order"),
    );
  });
});
