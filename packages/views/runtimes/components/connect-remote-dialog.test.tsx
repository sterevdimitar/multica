import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { I18nProvider } from "@multica/core/i18n/react";
import { configStore } from "@multica/core/config";
import { useWSEvent } from "@multica/core/realtime";
import enCommon from "../../locales/en/common.json";
import enRuntimes from "../../locales/en/runtimes.json";
import { ConnectRemoteDialog } from "./connect-remote-dialog";

const TEST_RESOURCES = { en: { common: enCommon, runtimes: enRuntimes } };

vi.mock("@multica/core/hooks", () => ({
  useWorkspaceId: () => "ws-test",
}));

vi.mock("@multica/core/paths", () => ({
  paths: {
    workspace: () => ({
      agents: () => "/agents",
      runtimeDetail: () => "/runtimes/rt-test",
    }),
  },
  useWorkspaceSlug: () => "workspace-test",
}));

vi.mock("@multica/core/realtime", () => ({
  useWSEvent: vi.fn(),
}));

vi.mock("../../navigation", () => ({
  useNavigation: () => ({ push: vi.fn() }),
}));

function resetConfigStore() {
  configStore.setState({
    cdnDomain: "",
    allowSignup: true,
    googleClientId: "",
    daemonServerUrl: "",
    daemonAppUrl: "",
    webhookRuntimeDispatchUrl: "",
    webhookRuntimeEventType: "",
    webhookRuntimeRunnerRepo: "",
    webhookRuntimeRunnerPath: "",
    workspaceCreationDisabled: false,
  });
}

// The dialog subscribes once per render; the last registration is the live
// one, and calling it is what the WS layer would do on daemon:register.
function latestRegisterHandler(): (payload: unknown) => void {
  const calls = vi.mocked(useWSEvent).mock.calls.filter(([name]) => name === "daemon:register");
  const last = calls[calls.length - 1];
  if (!last) throw new Error("dialog did not subscribe to daemon:register");
  return last[1] as (payload: unknown) => void;
}

function renderDialog(config?: {
  daemonServerUrl?: string;
  daemonAppUrl?: string;
}) {
  resetConfigStore();
  if (config) {
    configStore.getState().setDaemonConfig(config);
  }
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <ConnectRemoteDialog onClose={vi.fn()} />
      </I18nProvider>
    </QueryClientProvider>,
  );
}

const ligatureClasses = [
  "[font-variant-ligatures:none]",
  "[font-feature-settings:'liga'_0]",
];

describe("ConnectRemoteDialog", () => {
  it("uses cloud setup commands by default", () => {
    const { baseElement } = renderDialog();

    expect(baseElement).toHaveTextContent("multica setup");
    expect(baseElement).not.toHaveTextContent("multica setup self-host");
    expect(baseElement).toHaveTextContent(
      "multica config set server_url https://api.multica.ai",
    );
    expect(baseElement).toHaveTextContent(
      "multica config set app_url https://multica.ai",
    );
  });

  it("uses self-host daemon URLs from runtime config", () => {
    const { baseElement } = renderDialog({
      daemonServerUrl: "https://api.example.com/",
      daemonAppUrl: "https://app.example.com/",
    });

    expect(baseElement).toHaveTextContent(
      "multica setup self-host --server-url https://api.example.com --app-url https://app.example.com",
    );
    expect(baseElement).toHaveTextContent(
      "multica config set server_url https://api.example.com",
    );
    expect(baseElement).toHaveTextContent(
      "multica config set app_url https://app.example.com",
    );
  });

  it("disables font ligatures in setup command code", () => {
    const { baseElement } = renderDialog();

    const setupCode = Array.from(baseElement.querySelectorAll("code")).find((node) =>
      node.textContent?.includes("multica setup"),
    );

    expect(setupCode).toHaveClass(...ligatureClasses);
  });

  it("disables font ligatures in fallback token command code", () => {
    const { baseElement } = renderDialog();

    const tokenCode = Array.from(baseElement.querySelectorAll("code")).find((node) =>
      node.textContent?.includes("multica login --token <YOUR_TOKEN>"),
    );

    expect(tokenCode).toHaveClass(...ligatureClasses);
  });
});

describe("ConnectRemoteDialog webhook mode", () => {
  it("starts in daemon mode and switches to the webhook body", () => {
    const { baseElement } = renderDialog({
      daemonServerUrl: "https://m.example",
      daemonAppUrl: "https://app.example",
    });

    expect(baseElement).toHaveTextContent("multica setup self-host");
    expect(baseElement).not.toHaveTextContent("runs_on:");

    fireEvent.click(screen.getByRole("switch"));

    expect(baseElement).toHaveTextContent("runs_on: local-pc");
    expect(baseElement).not.toHaveTextContent("multica setup self-host");
    // The register command is the one thing both bodies are for.
    expect(baseElement).toHaveTextContent("/api/daemon/register");
  });

  it("daemon mode advances on any register event and links the registered runtime", () => {
    const { baseElement } = renderDialog();

    act(() => {
      latestRegisterHandler()({ runtimes: [{ id: "rt-1", name: "claude (box)" }] });
    });

    expect(baseElement).toHaveTextContent("Computer connected");
    expect(screen.getByRole("button", { name: "View runtime" })).toBeInTheDocument();
  });

  it("daemon mode still advances when the event carries no runtime id", () => {
    const { baseElement } = renderDialog();

    act(() => {
      latestRegisterHandler()({});
    });

    expect(baseElement).toHaveTextContent("Computer connected");
    expect(screen.queryByRole("button", { name: "View runtime" })).not.toBeInTheDocument();
  });

  it("webhook mode ignores a register event for a differently named runtime", () => {
    const { baseElement } = renderDialog();
    fireEvent.click(screen.getByRole("switch"));

    act(() => {
      latestRegisterHandler()({ runtimes: [{ id: "rt-9", name: "Other" }] });
    });

    expect(baseElement).not.toHaveTextContent("Computer connected");
    expect(baseElement).toHaveTextContent("runs_on: local-pc");
  });

  it("webhook mode advances when the typed name registers, with the webhook success text", () => {
    const { baseElement } = renderDialog();
    fireEvent.click(screen.getByRole("switch"));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "  Mac Mini  " } });

    act(() => {
      latestRegisterHandler()({
        runtimes: [
          { id: "rt-1", name: "Other" },
          { id: "rt-9", name: "Mac Mini" },
        ],
      });
    });

    expect(baseElement).toHaveTextContent("Runtime registered. Assign it to an agent to start dispatching.");
    expect(screen.getByRole("button", { name: "View runtime" })).toBeInTheDocument();
  });
});
