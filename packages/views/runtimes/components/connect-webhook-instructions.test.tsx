import { useState } from "react";
import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { I18nProvider } from "@multica/core/i18n/react";
import { configStore } from "@multica/core/config";
import enCommon from "../../locales/en/common.json";
import enRuntimes from "../../locales/en/runtimes.json";
import { ConnectWebhookInstructions } from "./connect-webhook-instructions";

const TEST_RESOURCES = { en: { common: enCommon, runtimes: enRuntimes } };

vi.mock("@multica/core/hooks", () => ({
  useWorkspaceId: () => "ws-test",
}));

const PROD_CONFIG = {
  webhookRuntimeDispatchUrl: "http://translator:8090/v1/dispatch",
  webhookRuntimeEventType: "multica-task",
  webhookRuntimeRunnerRepo: "acme/pipeline",
  webhookRuntimeRunnerPath: "deployment/local-runner",
};

function Harness({ initialName = "Local PC" }: { initialName?: string }) {
  const [name, setName] = useState(initialName);
  return <ConnectWebhookInstructions name={name} onNameChange={setName} />;
}

function renderBody(opts?: {
  config?: Partial<typeof PROD_CONFIG> | null;
  daemonServerUrl?: string;
  initialName?: string;
}) {
  configStore.getState().setDaemonConfig({ daemonServerUrl: opts?.daemonServerUrl ?? "" });
  configStore.getState().setWebhookRuntimeConfig(
    opts?.config === null ? {} : { ...PROD_CONFIG, ...opts?.config },
  );
  return render(
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      <Harness initialName={opts?.initialName} />
    </I18nProvider>,
  );
}

function codeTexts(baseElement: HTMLElement): string[] {
  return Array.from(baseElement.querySelectorAll("code")).map((c) => c.textContent ?? "");
}

describe("ConnectWebhookInstructions", () => {
  it("renders the name, the derived label, the three OS tabs and the filled-in register command", () => {
    const { baseElement } = renderBody({ daemonServerUrl: "https://m.example/" });

    expect(screen.getByLabelText("Name")).toHaveValue("Local PC");
    expect(baseElement).toHaveTextContent("runs_on: local-pc");
    expect(screen.getByRole("tab", { name: "Windows (WSL)" })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "macOS" })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Linux" })).toBeInTheDocument();

    const register = codeTexts(baseElement).find((c) => c.includes("/api/daemon/register"));
    expect(register).toContain("https://m.example/api/daemon/register");
    expect(register).toContain('"workspace_id":"ws-test"');
    expect(register).toContain("runs_on=local-pc");
    expect(register).toContain('"webhook_event_type":"multica-task"');
    expect(register).toContain("<YOUR_TOKEN>");
    expect(register).toContain("<WEBHOOK_SECRET>");
  });

  it("falls back to the page origin when no daemon server URL is configured", () => {
    const { baseElement } = renderBody();
    const register = codeTexts(baseElement).find((c) => c.includes("/api/daemon/register"));
    expect(register).toContain(`${window.location.origin}/api/daemon/register`);
  });

  it("shows placeholders and names the unset server variables when config is missing", () => {
    const { baseElement } = renderBody({ config: null });

    expect(baseElement).toHaveTextContent("WEBHOOK_RUNTIME_DISPATCH_URL");
    expect(baseElement).toHaveTextContent("WEBHOOK_RUNTIME_RUNNER_REPO");
    const register = codeTexts(baseElement).find((c) => c.includes("/api/daemon/register"));
    expect(register).toContain("<DISPATCH_URL>");
    expect(register).toContain("<EVENT_TYPE>");
  });

  it("does not show the missing-config note when everything is set", () => {
    const { baseElement } = renderBody();
    expect(baseElement).not.toHaveTextContent("WEBHOOK_RUNTIME_DISPATCH_URL");
  });

  it("re-derives the label as the name changes", () => {
    const { baseElement } = renderBody();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Mac Mini #2" } });
    expect(baseElement).toHaveTextContent("runs_on: mac-mini-2");
    expect(codeTexts(baseElement).some((c) => c.includes("RUNNER_LABEL=mac-mini-2"))).toBe(true);
  });

  it("hides the steps and explains when the name has no usable characters", () => {
    const { baseElement } = renderBody();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "!!!" } });
    expect(baseElement).toHaveTextContent("Name must contain a letter or digit.");
    expect(baseElement.querySelectorAll("code")).toHaveLength(0);
  });

  it("shows the WSL keep-alive step on the Windows tab only", () => {
    const { baseElement } = renderBody();
    expect(baseElement).toHaveTextContent("Keep WSL alive");
    expect(codeTexts(baseElement).some((c) => c.startsWith("Start-Process wsl.exe"))).toBe(true);

    fireEvent.click(screen.getByRole("tab", { name: "macOS" }));
    expect(baseElement).not.toHaveTextContent("Keep WSL alive");
    expect(codeTexts(baseElement).some((c) => c.startsWith("Start-Process wsl.exe"))).toBe(false);
    expect(baseElement).toHaveTextContent("Docker Desktop or OrbStack");
  });
});
