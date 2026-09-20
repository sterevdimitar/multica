import { describe, expect, it } from "vitest";
import {
  buildWebhookSetup,
  slugifyLabel,
  withRunsOn,
  type WebhookSetupConfig,
} from "./webhook-setup";

const PROD_CONFIG: WebhookSetupConfig = {
  dispatchUrl: "http://translator:8090/v1/dispatch",
  eventType: "multica-task",
  runnerRepo: "sterevdimitar/dev-command-center",
  runnerPath: "deployment/local-runner",
};

const EMPTY_CONFIG: WebhookSetupConfig = {
  dispatchUrl: "",
  eventType: "",
  runnerRepo: "",
  runnerPath: "",
};

describe("slugifyLabel", () => {
  it.each([
    ["Local PC", "local-pc"],
    ["Mac Mini #2", "mac-mini-2"],
    ["  Büro  ", "b-ro"],
    ["!!!", ""],
    ["already_ok.v2", "already_ok.v2"],
  ])("%j → %j", (name, label) => {
    expect(slugifyLabel(name)).toBe(label);
  });

  it("caps the label at the 64 characters runs_on allows", () => {
    expect(slugifyLabel("a".repeat(70))).toHaveLength(64);
  });

  it("only ever produces a value runs_on accepts, or nothing", () => {
    for (const name of ["Local PC", "x y z", "--a--", "Ünïcødé", "a.b_c-d"]) {
      const label = slugifyLabel(name);
      expect(label === "" || /^[A-Za-z0-9_.-]{1,64}$/.test(label)).toBe(true);
    }
  });
});

describe("withRunsOn", () => {
  it("appends runs_on to a bare dispatch URL", () => {
    expect(withRunsOn("http://t:8090/v1/dispatch", "local-pc")).toBe(
      "http://t:8090/v1/dispatch?runs_on=local-pc",
    );
  });

  it("keeps an existing query string", () => {
    expect(withRunsOn("http://t:8090/v1/dispatch?x=1", "local-pc")).toBe(
      "http://t:8090/v1/dispatch?x=1&runs_on=local-pc",
    );
  });

  it("replaces an existing runs_on rather than adding a second", () => {
    expect(withRunsOn("http://t:8090/v1/dispatch?runs_on=old", "new")).toBe(
      "http://t:8090/v1/dispatch?runs_on=new",
    );
  });

  it("returns an empty string when there is no base URL", () => {
    expect(withRunsOn("", "x")).toBe("");
  });
});

describe("buildWebhookSetup", () => {
  const full = buildWebhookSetup({
    name: "Local PC",
    workspaceId: "ws-1",
    serverUrl: "https://m.example",
    config: PROD_CONFIG,
  });

  it("derives the label and daemon id from the name", () => {
    expect(full.label).toBe("local-pc");
    expect(full.daemonId).toBe("local-pc-runner");
    expect(full.missingConfig).toEqual([]);
  });

  it("fetches the three runner files from the configured repository", () => {
    expect(full.steps?.fetch).toContain(
      'repos/sterevdimitar/dev-command-center/contents/deployment/local-runner/$f',
    );
    expect(full.steps?.fetch).toContain("Dockerfile docker-compose.yml .env.example");
    expect(full.steps?.fetch).toContain("mkdir -p ~/local-runner && cd ~/local-runner");
  });

  // The runtime token, not a PAT: the control plane mints the registration
  // token (dev-command-center design 2026-09-20), so no GitHub credential is
  // written on the machine.
  it("writes .env with the repo, the runtime token and the label", () => {
    expect(full.steps?.env).toContain(
      "REPO_URL=https://github.com/sterevdimitar/dev-command-center",
    );
    expect(full.steps?.env).toContain("DCC_RUNTIME_URL=https://m.example");
    expect(full.steps?.env).toContain("DCC_RUNTIME_TOKEN=<RUNTIME_TOKEN>");
    expect(full.steps?.env).not.toContain("%s");
    expect(full.steps?.env).toContain("RUNNER_LABEL=local-pc");
    expect(full.steps?.env).not.toContain("ACCESS_TOKEN=");
    expect(full.steps?.env).not.toContain("gh auth token");
    expect(full.steps?.env).toContain("chmod 600 .env");
  });

  it("starts the containers", () => {
    expect(full.steps?.up).toBe("docker compose up -d --build");
  });

  it("registers the runtime with every deployment value filled in", () => {
    const register = full.steps?.register ?? "";
    expect(register).toContain("https://m.example/api/daemon/register");
    expect(register).toContain('"workspace_id":"ws-1"');
    expect(register).toContain('"daemon_id":"local-pc-runner"');
    expect(register).toContain('"device_name":"local-pc"');
    expect(register).toContain('"name":"Local PC"');
    expect(register).toContain('"runtime_mode":"webhook"');
    expect(register).toContain(
      '"webhook_url":"http://translator:8090/v1/dispatch?runs_on=local-pc"',
    );
    expect(register).toContain('"webhook_event_type":"multica-task"');
  });

  it("never fills in the two secrets", () => {
    const register = full.steps?.register ?? "";
    expect(register).toContain("Bearer <YOUR_TOKEN>");
    expect(register).toContain('"webhook_secret":"<WEBHOOK_SECRET>"');
  });

  it("has the Windows keep-alive step", () => {
    expect(full.steps?.keepAliveWindows).toBe(
      'Start-Process wsl.exe -ArgumentList "-d","Ubuntu","-e","sleep","infinity" -WindowStyle Hidden',
    );
  });

  it("strips a trailing slash from the server URL", () => {
    const setup = buildWebhookSetup({
      name: "Local PC",
      workspaceId: "ws-1",
      serverUrl: "https://m.example/",
      config: PROD_CONFIG,
    });
    expect(setup.steps?.register).toContain("https://m.example/api/daemon/register");
  });

  it("JSON-escapes the name inside the register payload", () => {
    const setup = buildWebhookSetup({
      name: 'Say "hi"\\',
      workspaceId: "ws-1",
      serverUrl: "https://m.example",
      config: PROD_CONFIG,
    });
    expect(setup.steps?.register).toContain('"name":"Say \\"hi\\"\\\\"');
  });

  it("names every unset server variable and renders its placeholder", () => {
    const setup = buildWebhookSetup({
      name: "Local PC",
      workspaceId: "ws-1",
      serverUrl: "https://m.example",
      config: EMPTY_CONFIG,
    });
    expect(setup.missingConfig).toEqual([
      "WEBHOOK_RUNTIME_DISPATCH_URL",
      "WEBHOOK_RUNTIME_EVENT_TYPE",
      "WEBHOOK_RUNTIME_RUNNER_REPO",
      "WEBHOOK_RUNTIME_RUNNER_PATH",
    ]);
    expect(setup.steps?.register).toContain('"webhook_url":"<DISPATCH_URL>?runs_on=local-pc"');
    expect(setup.steps?.register).toContain('"webhook_event_type":"<EVENT_TYPE>"');
    expect(setup.steps?.fetch).toContain("repos/<RUNNER_REPO>/contents/<RUNNER_PATH>/$f");
    expect(setup.steps?.env).toContain("REPO_URL=https://github.com/<RUNNER_REPO>");
  });

  it("names only the variables that are actually unset", () => {
    const setup = buildWebhookSetup({
      name: "Local PC",
      workspaceId: "ws-1",
      serverUrl: "https://m.example",
      config: { ...PROD_CONFIG, eventType: "" },
    });
    expect(setup.missingConfig).toEqual(["WEBHOOK_RUNTIME_EVENT_TYPE"]);
  });

  it("produces no steps when the name has no usable characters", () => {
    const setup = buildWebhookSetup({
      name: "!!!",
      workspaceId: "ws-1",
      serverUrl: "https://m.example",
      config: PROD_CONFIG,
    });
    expect(setup.label).toBe("");
    expect(setup.daemonId).toBe("");
    expect(setup.steps).toBeNull();
  });
});
