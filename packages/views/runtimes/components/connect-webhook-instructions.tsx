"use client";

import { useId, useState } from "react";
import { ChevronRight } from "lucide-react";
import { useWorkspaceId } from "@multica/core/hooks";
import { useConfigStore } from "@multica/core/config";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@multica/ui/components/ui/tabs";
import { CODE_LIGATURE_CLASS } from "@multica/ui/lib/code-style";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";
import { CommandStep } from "./connect-command-step";
import { buildWebhookSetup, type WebhookSetupSteps } from "./webhook-setup";

// The "Add a computer" dialog's webhook body: a name, the label it derives,
// and — per OS — the commands that start the runner containers and register
// the runtime. All values come from webhook-setup.ts; this file only lays
// them out. The dialog renders LiveListening after this component.

type Os = "windows" | "macos" | "linux";

export function ConnectWebhookInstructions({
  name,
  onNameChange,
}: {
  name: string;
  onNameChange: (name: string) => void;
}) {
  const { t } = useT("runtimes");
  const wsId = useWorkspaceId();
  const daemonServerUrl = useConfigStore((s) => s.daemonServerUrl);
  const dispatchUrl = useConfigStore((s) => s.webhookRuntimeDispatchUrl);
  const eventType = useConfigStore((s) => s.webhookRuntimeEventType);
  const runnerRepo = useConfigStore((s) => s.webhookRuntimeRunnerRepo);
  const runnerPath = useConfigStore((s) => s.webhookRuntimeRunnerPath);
  const [os, setOs] = useState<Os>("windows");
  const nameId = useId();

  const setup = buildWebhookSetup({
    name,
    workspaceId: wsId,
    // Same source the daemon body uses for its --server-url; the page's own
    // origin is the right answer whenever the operator has not set one.
    serverUrl: daemonServerUrl || (typeof window !== "undefined" ? window.location.origin : ""),
    config: { dispatchUrl, eventType, runnerRepo, runnerPath },
  });

  return (
    <div className="space-y-4">
      <div className="space-y-1.5">
        <Label htmlFor={nameId} className="text-xs">
          {t(($) => $.connect.webhook.name_label)}
        </Label>
        <Input
          id={nameId}
          value={name}
          onChange={(e) => onNameChange(e.target.value)}
          autoComplete="off"
          spellCheck={false}
        />
        {setup.label ? (
          <p className="text-[11px] leading-[1.55] text-muted-foreground">
            {t(($) => $.connect.webhook.label_prefix)}{" "}
            <code
              className={cn(
                "rounded bg-muted px-1.5 py-0.5 font-mono text-[10px] text-foreground",
                CODE_LIGATURE_CLASS,
              )}
            >
              {setup.label}
            </code>
          </p>
        ) : (
          <p className="text-[11px] leading-[1.55] text-destructive" role="alert">
            {t(($) => $.connect.webhook.name_invalid)}
          </p>
        )}
      </div>

      {setup.steps && (
        <>
          <p className="text-[11px] leading-[1.55] text-muted-foreground">
            {t(($) => $.connect.webhook.intro)}
          </p>

          {setup.missingConfig.length > 0 && (
            <p className="rounded-lg border border-dashed px-3 py-2 text-[11px] leading-[1.55] text-muted-foreground">
              {t(($) => $.connect.webhook.missing_config, {
                vars: setup.missingConfig.join(", "),
              })}
            </p>
          )}

          <Tabs value={os} onValueChange={(v) => setOs(v as Os)}>
            <TabsList className="grid w-full grid-cols-3">
              <TabsTrigger value="windows">{t(($) => $.connect.webhook.tab_windows)}</TabsTrigger>
              <TabsTrigger value="macos">{t(($) => $.connect.webhook.tab_macos)}</TabsTrigger>
              <TabsTrigger value="linux">{t(($) => $.connect.webhook.tab_linux)}</TabsTrigger>
            </TabsList>
            <TabsContent value="windows" className="space-y-4 pt-3">
              <Requirements text={t(($) => $.connect.webhook.req_windows)} />
              <Steps steps={setup.steps} keepAlive />
            </TabsContent>
            <TabsContent value="macos" className="space-y-4 pt-3">
              <Requirements text={t(($) => $.connect.webhook.req_macos)} />
              <Steps steps={setup.steps} />
            </TabsContent>
            <TabsContent value="linux" className="space-y-4 pt-3">
              <Requirements text={t(($) => $.connect.webhook.req_linux)} />
              <Steps steps={setup.steps} />
            </TabsContent>
          </Tabs>
        </>
      )}
    </div>
  );
}

function Requirements({ text }: { text: string }) {
  return <p className="text-[11px] leading-[1.55] text-muted-foreground">{text}</p>;
}

function Steps({ steps, keepAlive = false }: { steps: WebhookSetupSteps; keepAlive?: boolean }) {
  const { t } = useT("runtimes");
  const copyAria = t(($) => $.connect.copy_aria);
  return (
    <div className="space-y-4">
      <CommandStep n={1} label={t(($) => $.connect.webhook.step_fetch)} cmd={steps.fetch} copyAria={copyAria} />
      <CommandStep n={2} label={t(($) => $.connect.webhook.step_env)} cmd={steps.env} copyAria={copyAria} />
      <CommandStep n={3} label={t(($) => $.connect.webhook.step_up)} cmd={steps.up} copyAria={copyAria} />
      <CommandStep n={4} label={t(($) => $.connect.webhook.step_register)} cmd={steps.register} copyAria={copyAria} />
      {keepAlive && (
        <CommandStep
          n={5}
          label={t(($) => $.connect.webhook.step_keepalive)}
          cmd={steps.keepAliveWindows}
          copyAria={copyAria}
        />
      )}
    </div>
  );
}

export function WebhookSecretsDetails() {
  const { t } = useT("runtimes");
  return (
    <details className="group rounded-lg border border-dashed">
      <summary className="flex cursor-pointer list-none items-center gap-1.5 px-3 py-2 text-xs font-medium text-muted-foreground hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">
        <ChevronRight className="h-3 w-3 transition-transform group-open:rotate-90" aria-hidden />
        {t(($) => $.connect.webhook.secrets_summary)}
      </summary>
      <ul className="space-y-1.5 border-t px-3 pt-2.5 pb-3 text-[11px] leading-[1.55] text-muted-foreground">
        <li>{t(($) => $.connect.webhook.secret_token)}</li>
        <li>{t(($) => $.connect.webhook.secret_webhook)}</li>
        <li>{t(($) => $.connect.webhook.secret_runtime)}</li>
      </ul>
    </details>
  );
}
