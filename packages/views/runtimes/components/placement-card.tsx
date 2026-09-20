"use client";

import { useEffect, useState } from "react";
import { toast } from "sonner";
import { useQuery } from "@tanstack/react-query";
import type { AgentRuntime } from "@multica/core/types";
import { useWorkspaceId } from "@multica/core/hooks";
import { runtimeListOptions } from "@multica/core/runtimes/queries";
import { useUpdateRuntime } from "@multica/core/runtimes/mutations";
import { Input } from "@multica/ui/components/ui/input";
import { useT } from "../../i18n";

// The placement settings of a webhook runtime (dev-command-center design
// 2026-09-20-runtime-placement §4): "Runs at once" — the cap — editable
// here and nowhere else, and the position in the fallback order, read-only
// here because it is dragged on the Runtimes page and nowhere else. One
// writer per field.

// parseCapInput maps the input's text to the PATCH value: "" → null (no
// limit), a non-negative integer → itself, anything else → undefined (do
// not submit). The input's min=0 stops the spinner; this stops the keyboard.
export function parseCapInput(value: string): number | null | undefined {
  const trimmed = value.trim();
  if (trimmed === "") return null;
  if (!/^\d+$/.test(trimmed)) return undefined;
  return Number(trimmed);
}

// rankAmongWebhookRuntimes is the 1-based position of this runtime in the
// workspace's fallback order (dispatch_order, ties by created_at) and the
// count — for "3rd of 4". null when the runtime is not a webhook runtime.
export function rankAmongWebhookRuntimes(
  runtime: AgentRuntime,
  all: AgentRuntime[],
): { rank: number; total: number } | null {
  if (runtime.runtime_mode !== "webhook") return null;
  const ordered = all
    .filter((r) => r.runtime_mode === "webhook")
    .sort(
      (a, b) =>
        (a.dispatch_order ?? 0) - (b.dispatch_order ?? 0) ||
        a.created_at.localeCompare(b.created_at),
    );
  const index = ordered.findIndex((r) => r.id === runtime.id);
  if (index < 0) return null;
  return { rank: index + 1, total: ordered.length };
}

export function PlacementCard({
  runtime,
  canEdit,
}: {
  runtime: AgentRuntime;
  canEdit: boolean;
}) {
  const { t } = useT("runtimes");
  const wsId = useWorkspaceId();
  const updateRuntime = useUpdateRuntime(wsId);
  const { data: runtimes = [] } = useQuery(runtimeListOptions(wsId));
  const position = rankAmongWebhookRuntimes(runtime, runtimes);

  const serverValue =
    runtime.max_concurrent_tasks == null ? "" : String(runtime.max_concurrent_tasks);
  const [draft, setDraft] = useState(serverValue);
  useEffect(() => setDraft(serverValue), [serverValue]);

  const submit = () => {
    const next = parseCapInput(draft);
    if (next === undefined) {
      setDraft(serverValue);
      return;
    }
    if (draft.trim() === serverValue) return;
    updateRuntime.mutate(
      { runtimeId: runtime.id, patch: { max_concurrent_tasks: next } },
      {
        onSuccess: () => toast.success(t(($) => $.detail.runs_at_once.toast_updated)),
        onError: (err) => {
          setDraft(serverValue);
          toast.error(
            err instanceof Error && err.message
              ? err.message
              : t(($) => $.detail.runs_at_once.toast_failed),
          );
        },
      },
    );
  };

  return (
    <div className="rounded-lg border" data-testid="placement-card">
      <div className="border-b px-4 py-2.5">
        <span className="text-xs font-semibold">{t(($) => $.detail.runs_at_once.title)}</span>
      </div>
      <div className="space-y-3 p-4">
        <div>
          <label
            htmlFor="runs-at-once"
            className="mb-1.5 block text-[11px] uppercase tracking-wide text-muted-foreground"
          >
            {t(($) => $.detail.runs_at_once.label)}
          </label>
          <Input
            id="runs-at-once"
            type="number"
            min={0}
            step={1}
            inputMode="numeric"
            className="h-8 w-28"
            placeholder={t(($) => $.detail.runs_at_once.placeholder)}
            value={draft}
            disabled={!canEdit || updateRuntime.isPending}
            onChange={(e) => setDraft(e.target.value)}
            onBlur={submit}
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                e.preventDefault();
                (e.target as HTMLInputElement).blur();
              }
            }}
          />
          <p className="mt-1.5 text-[11px] leading-relaxed text-muted-foreground">
            {t(($) => $.detail.runs_at_once.note_containers)}
          </p>
          <p className="text-[11px] leading-relaxed text-muted-foreground">
            {t(($) => $.detail.runs_at_once.zero_note)}
          </p>
        </div>
        {position && (
          <div className="border-t pt-3 text-xs text-muted-foreground" data-testid="placement-order">
            {t(($) => $.detail.runs_at_once.order, { rank: position.rank, total: position.total })}
          </div>
        )}
      </div>
    </div>
  );
}
