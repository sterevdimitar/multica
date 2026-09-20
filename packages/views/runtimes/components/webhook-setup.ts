// The "Add a computer" dialog's webhook mode: everything that turns a name
// plus the server's bring-up config into the commands the user runs. Pure
// so the dialog can stay a renderer and this file can be tested as a table.
//
// A webhook runtime is two halves — self-hosted runner containers on the
// machine, and one /api/daemon/register call whose webhook_url carries the
// label those containers wear (`?runs_on=<label>`). The label appears in
// both halves and must be the same string (nothing downstream checks it; a
// mismatch queues every job), which is why it is derived exactly once here.

export interface WebhookSetupConfig {
  /** Base URL the dispatch service listens on; "" when the operator has not set it. */
  dispatchUrl: string;
  /** Event type the dispatch service expects; "" when unset. */
  eventType: string;
  /** owner/repo holding the runner files and served by the runners; "" when unset. */
  runnerRepo: string;
  /** Path inside runnerRepo holding Dockerfile, docker-compose.yml, .env.example; "" when unset. */
  runnerPath: string;
}

export interface WebhookSetupInput {
  name: string;
  workspaceId: string;
  serverUrl: string;
  config: WebhookSetupConfig;
}

export interface WebhookSetupSteps {
  fetch: string;
  env: string;
  up: string;
  register: string;
  keepAliveWindows: string;
}

export interface WebhookSetup {
  /** slugifyLabel(name); "" when the name has no usable characters. */
  label: string;
  /** `${label}-runner`; "" when label is "". */
  daemonId: string;
  /** Env-var names for each unset config value, in a fixed order. */
  missingConfig: string[];
  /** null iff label is "". */
  steps: WebhookSetupSteps | null;
}

// dcc-entrypoint.sh joined on 2026-09-20: the Dockerfile COPYs it (the wrapper
// that mints the registration token), so a build without it fails.
export const RUNNER_FILES = ["Dockerfile", "dcc-entrypoint.sh", "docker-compose.yml", ".env.example"] as const;

/**
 * The runs_on label for a runtime name. Matches the dispatch service's
 * `^[A-Za-z0-9_.-]{1,64}$` or is "" — never anything in between.
 */
export function slugifyLabel(name: string): string {
  return name
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9_.-]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 64);
}

/** dispatchUrl with `runs_on=<label>` set (replacing any existing one); "" when there is no base. */
export function withRunsOn(dispatchUrl: string, label: string): string {
  if (!dispatchUrl) return "";
  try {
    const url = new URL(dispatchUrl);
    url.searchParams.set("runs_on", label);
    return url.toString();
  } catch {
    // Not a parseable URL (a placeholder, or operator input the browser's
    // URL parser rejects): append by hand so the label still shows up.
    const sep = dispatchUrl.includes("?") ? "&" : "?";
    return `${dispatchUrl}${sep}runs_on=${label}`;
  }
}

const PLACEHOLDERS: { [K in keyof WebhookSetupConfig]: { env: string; text: string } } = {
  dispatchUrl: { env: "WEBHOOK_RUNTIME_DISPATCH_URL", text: "<DISPATCH_URL>" },
  eventType: { env: "WEBHOOK_RUNTIME_EVENT_TYPE", text: "<EVENT_TYPE>" },
  runnerRepo: { env: "WEBHOOK_RUNTIME_RUNNER_REPO", text: "<RUNNER_REPO>" },
  runnerPath: { env: "WEBHOOK_RUNTIME_RUNNER_PATH", text: "<RUNNER_PATH>" },
};

const CONFIG_ORDER: (keyof WebhookSetupConfig)[] = [
  "dispatchUrl",
  "eventType",
  "runnerRepo",
  "runnerPath",
];

export function buildWebhookSetup(input: WebhookSetupInput): WebhookSetup {
  const label = slugifyLabel(input.name);
  const missingConfig = CONFIG_ORDER.filter((key) => !input.config[key]).map(
    (key) => PLACEHOLDERS[key].env,
  );
  if (!label) {
    return { label, daemonId: "", missingConfig, steps: null };
  }

  const value = (key: keyof WebhookSetupConfig) => input.config[key] || PLACEHOLDERS[key].text;
  const repo = value("runnerRepo");
  const path = value("runnerPath");
  const server = input.serverUrl.trim().replace(/\/+$/, "");
  const daemonId = `${label}-runner`;

  const fetch = [
    "mkdir -p ~/local-runner && cd ~/local-runner",
    `for f in ${RUNNER_FILES.join(" ")}; do`,
    `  gh api -H "Accept: application/vnd.github.raw" "repos/${repo}/contents/${path}/$f" > "$f"`,
    "done",
  ].join("\n");

  // The runtime token, not a PAT: the control plane mints the registration
  // token from it (dev-command-center design 2026-09-20), so no GitHub
  // credential lives on the machine. <RUNTIME_TOKEN> is a literal placeholder
  // like the two in the register step; the board's owner supplies the value.
  const env = `printf 'REPO_URL=https://github.com/${repo}\\nDCC_RUNTIME_URL=${server}\\nDCC_RUNTIME_TOKEN=<RUNTIME_TOKEN>\\nRUNNER_LABEL=${label}\\n' > .env && chmod 600 .env`;

  const up = "docker compose up -d --build";

  // The payload is rendered by hand rather than JSON.stringify'd as a whole
  // so the two secrets stay literal <PLACEHOLDERS> and the line breaks stay
  // readable; the one user-typed value, the name, is escaped on its own.
  const register = [
    `curl -fsS -X POST "${server}/api/daemon/register" \\`,
    `  -H "Authorization: Bearer <YOUR_TOKEN>" -H "Content-Type: application/json" \\`,
    `  -d '{"workspace_id":"${input.workspaceId}","daemon_id":"${daemonId}","device_name":"${label}",`,
    `       "runtimes":[{"name":${JSON.stringify(input.name.trim())},"type":"claude","version":"1.0.0","status":"online",`,
    `       "runtime_mode":"webhook","webhook_url":"${withRunsOn(value("dispatchUrl"), label)}",`,
    `       "webhook_secret":"<WEBHOOK_SECRET>","webhook_event_type":"${value("eventType")}"}]}'`,
  ].join("\n");

  const keepAliveWindows =
    'Start-Process wsl.exe -ArgumentList "-d","Ubuntu","-e","sleep","infinity" -WindowStyle Hidden';

  return {
    label,
    daemonId,
    missingConfig,
    steps: { fetch, env, up, register, keepAliveWindows },
  };
}
