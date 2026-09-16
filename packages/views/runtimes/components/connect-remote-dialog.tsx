"use client";

import { useCallback, useId, useRef, useState } from "react";
import { Check, ChevronRight } from "lucide-react";
import { useQueryClient } from "@tanstack/react-query";
import { useWorkspaceId } from "@multica/core/hooks";
import { runtimeKeys } from "@multica/core/runtimes/queries";
import { useWSEvent } from "@multica/core/realtime";
import { paths, useWorkspaceSlug } from "@multica/core/paths";
import { useConfigStore } from "@multica/core/config";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import { Button } from "@multica/ui/components/ui/button";
import { Label } from "@multica/ui/components/ui/label";
import { Switch } from "@multica/ui/components/ui/switch";
import { CODE_LIGATURE_CLASS } from "@multica/ui/lib/code-style";
import { cn } from "@multica/ui/lib/utils";
import { useNavigation } from "../../navigation";
import { useT } from "../../i18n";
import { CommandStep, LiveListening } from "./connect-command-step";
import {
  ConnectWebhookInstructions,
  WebhookSecretsDetails,
} from "./connect-webhook-instructions";

type Step = "instructions" | "success";

// Two ways to add a computer. "daemon" is `multica setup`: the Multica daemon
// on the machine, polling for tasks. "webhook" is self-hosted runner
// containers plus a /api/daemon/register call whose webhook_url names the
// label they wear — a runtime the server POSTs tasks to instead.
type Mode = "daemon" | "webhook";

const DEFAULT_WEBHOOK_NAME = "Local PC";

const INSTALL_CMD =
  "curl -fsSL https://raw.githubusercontent.com/multica-ai/multica/main/scripts/install.sh | bash";
const CLOUD_SERVER_URL = "https://api.multica.ai";
const CLOUD_APP_URL = "https://multica.ai";

function normalizeCommandURL(url: string | undefined) {
  return url?.trim().replace(/\/+$/, "") ?? "";
}

function daemonCommands(serverUrl: string | undefined, appUrl: string | undefined) {
  const normalizedServerUrl = normalizeCommandURL(serverUrl);
  const normalizedAppUrl = normalizeCommandURL(appUrl);
  if (normalizedServerUrl && normalizedAppUrl) {
    return {
      setupCmd: `multica setup self-host --server-url ${normalizedServerUrl} --app-url ${normalizedAppUrl}`,
      tokenCmd: `multica config set server_url ${normalizedServerUrl}
multica config set app_url ${normalizedAppUrl}
multica login --token <YOUR_TOKEN>
multica daemon start`,
    };
  }

  return {
    setupCmd: "multica setup",
    tokenCmd: `multica config set server_url ${CLOUD_SERVER_URL}
multica config set app_url ${CLOUD_APP_URL}
multica login --token <YOUR_TOKEN>
multica daemon start`,
  };
}

// The daemon:register payload is { runtimes: AgentRuntimeResponse[] }; only
// id and name matter here, and either may be missing on a drifted server.
function registeredRuntimes(payload: unknown): { id?: string; name?: string }[] {
  const p = payload as { runtimes?: unknown } | null;
  if (!p || !Array.isArray(p.runtimes)) return [];
  return p.runtimes.map((r) => {
    const rt = r as Record<string, unknown> | null;
    return {
      id: typeof rt?.id === "string" ? rt.id : undefined,
      name: typeof rt?.name === "string" ? rt.name : undefined,
    };
  });
}

export function ConnectRemoteDialog({ onClose }: { onClose: () => void }) {
  const { t } = useT("runtimes");
  const [step, setStep] = useState<Step>("instructions");
  const [mode, setMode] = useState<Mode>("daemon");
  const [webhookName, setWebhookName] = useState(DEFAULT_WEBHOOK_NAME);
  const wsId = useWorkspaceId();
  const slug = useWorkspaceSlug();
  const qc = useQueryClient();
  const navigation = useNavigation();
  const newRuntimeIdRef = useRef<string | null>(null);

  // Both modes end in a `daemon:register` WS event; the dialog passively
  // listens and auto-advances to success. Daemon mode advances on any
  // register (the `multica setup` flow is the only thing that produces one
  // while the user is looking at this dialog); webhook mode waits for the
  // runtime with the typed name, so an unrelated daemon restarting does not
  // fake a success.
  const handleDaemonRegister = useCallback(
    (payload: unknown) => {
      if (step !== "instructions") return;
      const runtimes = registeredRuntimes(payload);
      const match =
        mode === "webhook"
          ? runtimes.find((r) => r.name === webhookName.trim())
          : runtimes[0];
      if (mode === "webhook" && !match) return;
      qc.invalidateQueries({ queryKey: runtimeKeys.all(wsId) });
      newRuntimeIdRef.current = match?.id ?? null;
      setStep("success");
    },
    [step, mode, webhookName, qc, wsId],
  );
  useWSEvent("daemon:register", handleDaemonRegister);

  const handleGoToAgents = () => {
    onClose();
    if (slug) {
      navigation.push(paths.workspace(slug).agents());
    }
  };

  const handleGoToRuntime = () => {
    onClose();
    if (slug && newRuntimeIdRef.current) {
      navigation.push(
        paths.workspace(slug).runtimeDetail(newRuntimeIdRef.current),
      );
    }
  };

  return (
    <Dialog open onOpenChange={(v) => !v && onClose()}>
      <DialogContent className="flex max-h-[85vh] flex-col gap-0 p-0 sm:max-w-lg">
        {step === "instructions" && (
          <InstructionsStep
            mode={mode}
            onModeChange={setMode}
            webhookName={webhookName}
            onWebhookNameChange={setWebhookName}
            onClose={onClose}
          />
        )}
        {step === "success" && (
          <SuccessStep
            description={
              mode === "webhook"
                ? t(($) => $.connect.webhook.success_description)
                : t(($) => $.connect.success_description)
            }
            onGoToAgents={handleGoToAgents}
            onGoToRuntime={
              newRuntimeIdRef.current ? handleGoToRuntime : undefined
            }
          />
        )}
      </DialogContent>
    </Dialog>
  );
}

// ---------------------------------------------------------------------------
// Step 1: Instructions
// ---------------------------------------------------------------------------

function InstructionsStep({
  mode,
  onModeChange,
  webhookName,
  onWebhookNameChange,
  onClose,
}: {
  mode: Mode;
  onModeChange: (mode: Mode) => void;
  webhookName: string;
  onWebhookNameChange: (name: string) => void;
  onClose: () => void;
}) {
  const { t } = useT("runtimes");
  const switchId = useId();
  return (
    <>
      <DialogHeader className="px-6 pt-6 pb-2">
        <DialogTitle className="text-base text-balance">
          {t(($) => $.connect.title)}
        </DialogTitle>
        <DialogDescription className="text-xs text-balance">
          {mode === "webhook"
            ? t(($) => $.connect.webhook.intro)
            : t(($) => $.connect.description)}
        </DialogDescription>
      </DialogHeader>

      <div className="min-h-0 flex-1 overflow-y-auto px-6 py-4">
        <div className="space-y-4">
          <div className="flex items-center gap-2.5 rounded-lg border px-3 py-2.5">
            <Switch
              id={switchId}
              checked={mode === "webhook"}
              onCheckedChange={(checked) => onModeChange(checked ? "webhook" : "daemon")}
            />
            <Label htmlFor={switchId} className="text-xs font-medium">
              {t(($) => $.connect.webhook.toggle)}
            </Label>
          </div>

          {mode === "webhook" ? (
            <>
              <ConnectWebhookInstructions
                name={webhookName}
                onNameChange={onWebhookNameChange}
              />
              <LiveListening />
              <WebhookSecretsDetails />
            </>
          ) : (
            <DaemonInstructions />
          )}
        </div>
      </div>

      <DialogFooter className="m-0 rounded-b-xl border-t bg-muted/30 px-6 py-3">
        <Button variant="outline" size="sm" onClick={onClose}>
          {t(($) => $.connect.cancel)}
        </Button>
      </DialogFooter>
    </>
  );
}

function DaemonInstructions() {
  const { t } = useT("runtimes");
  const daemonServerUrl = useConfigStore((s) => s.daemonServerUrl);
  const daemonAppUrl = useConfigStore((s) => s.daemonAppUrl);
  const { setupCmd, tokenCmd } = daemonCommands(daemonServerUrl, daemonAppUrl);
  return (
    <>
      <CommandStep
        n={1}
        label={t(($) => $.connect.step1_label)}
        cmd={INSTALL_CMD}
        copyAria={t(($) => $.connect.copy_aria)}
      />

      <div>
        <CommandStep
          n={2}
          label={t(($) => $.connect.step2_label)}
          cmd={setupCmd}
          copyAria={t(($) => $.connect.copy_aria)}
        />
        <p className="mt-1.5 text-[11px] leading-[1.55] text-muted-foreground">
          {t(($) => $.connect.step2_hint)}
        </p>
      </div>

      <LiveListening />

      <TroubleshootingDetails tokenCmd={tokenCmd} />
    </>
  );
}

function TroubleshootingDetails({ tokenCmd }: { tokenCmd: string }) {
  const { t } = useT("runtimes");
  return (
    <details className="group rounded-lg border border-dashed">
      <summary className="flex cursor-pointer list-none items-center gap-1.5 px-3 py-2 text-xs font-medium text-muted-foreground hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">
        <ChevronRight
          className="h-3 w-3 transition-transform group-open:rotate-90"
          aria-hidden
        />
        {t(($) => $.connect.troubleshooting)}
      </summary>
      <div className="space-y-2 border-t px-3 pt-2.5 pb-3 text-[11px] leading-[1.55] text-muted-foreground">
        <p>{t(($) => $.connect.trouble_intro)}</p>
        <CommandStep
          n={2}
          label={t(($) => $.connect.step2_label)}
          cmd={tokenCmd}
          copyAria={t(($) => $.connect.copy_aria)}
        />
        <p>
          {t(($) => $.connect.trouble_token_hint_prefix)}
          <span className="font-medium text-foreground">
            {t(($) => $.connect.trouble_token_hint_destination)}
          </span>
          {t(($) => $.connect.trouble_token_hint_suffix)}
        </p>
        <ul className="space-y-1">
          <li className="flex items-center gap-1.5">
            <span>{t(($) => $.connect.trouble_check_status)}</span>
            {/* CLI command — literal shell string, not i18n content. */}
            {/* eslint-disable-next-line i18next/no-literal-string */}
            <code
              className={cn(
                "rounded bg-muted px-1.5 py-0.5 font-mono text-[10px] text-foreground",
                CODE_LIGATURE_CLASS,
              )}
            >
              {"multica daemon status"}
            </code>
          </li>
          <li className="flex items-center gap-1.5">
            <span>{t(($) => $.connect.trouble_view_logs)}</span>
            {/* CLI command — literal shell string, not i18n content. */}
            {/* eslint-disable-next-line i18next/no-literal-string */}
            <code
              className={cn(
                "rounded bg-muted px-1.5 py-0.5 font-mono text-[10px] text-foreground",
                CODE_LIGATURE_CLASS,
              )}
            >
              {"multica daemon logs -f"}
            </code>
          </li>
        </ul>
      </div>
    </details>
  );
}

// ---------------------------------------------------------------------------
// Step 2: Success
// ---------------------------------------------------------------------------

function SuccessStep({
  description,
  onGoToAgents,
  onGoToRuntime,
}: {
  description: string;
  onGoToAgents: () => void;
  onGoToRuntime?: () => void;
}) {
  const { t } = useT("runtimes");
  return (
    <>
      <DialogHeader className="px-6 pt-6 pb-2">
        <DialogTitle className="text-base text-balance">
          {t(($) => $.connect.success_title)}
        </DialogTitle>
        <DialogDescription className="text-xs text-balance">
          {description}
        </DialogDescription>
      </DialogHeader>

      <div className="flex flex-col items-center gap-3 px-6 py-8">
        <div
          className="flex h-12 w-12 items-center justify-center rounded-full bg-success/10"
          aria-hidden
        >
          <Check className="h-6 w-6 text-success" />
        </div>
      </div>

      <DialogFooter className="m-0 rounded-b-xl border-t bg-muted/30 px-6 py-3">
        {onGoToRuntime && (
          <Button variant="ghost" size="sm" onClick={onGoToRuntime}>
            {t(($) => $.connect.view_runtime)}
          </Button>
        )}
        <Button size="sm" onClick={onGoToAgents}>
          {t(($) => $.connect.create_agent)}
          <ChevronRight className="h-3.5 w-3.5" aria-hidden />
        </Button>
      </DialogFooter>
    </>
  );
}
