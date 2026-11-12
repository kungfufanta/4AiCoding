// agent-run-card.tsx — A3.4 minimal agent run rendering inside the
// Chat page (docs/AGENT_PLAN_A3.md §10): a status header, the streamed
// assistant text, collapsed one-line tool cards that expand to an
// output preview, the build/preview lines and the final result block
// with Revert / Continue / Stop. Live runs ride the BFF SSE stream
// (EventSource reconnects with Last-Event-ID on its own; the server
// replays from seq 0 so a mid-run reload rebuilds the whole view).

import { useEffect, useMemo, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import {
  Check,
  ChevronDown,
  ChevronRight,
  FileText,
  Flag,
  FolderOpen,
  Globe,
  Hammer,
  Loader2,
  PenLine,
  RotateCcw,
  Search,
  Square,
  Terminal,
  TriangleAlert,
  Wrench,
  X,
} from "lucide-react";
import { api, type AgentEvent, type AgentRun } from "../../api";
import { cn } from "../../lib/cn";
import { Badge } from "../../ui/badge";
import { Button } from "../../ui/button";
import { useConfirm } from "../../ui/dialog";
import { useToast } from "../../ui/toast";
import { Markdown } from "./markdown";

const LIVE_STATUSES = new Set(["queued", "running"]);

// One merged tool view: the tool_call event opens the row, the
// matching tool_result event completes it.
type ToolView = {
  id: string;
  name: string;
  args: string;
  ok?: boolean;
  exitCode?: number;
  durationMs?: number;
  output?: string;
  truncated?: boolean;
  errorCode?: string;
};

function toolIcon(name: string) {
  switch (name) {
    case "write_file":
    case "edit_file":
      return PenLine;
    case "read_file":
      return FileText;
    case "list_files":
      return FolderOpen;
    case "search":
      return Search;
    case "run_command":
      return Terminal;
    case "check_preview":
      return Globe;
    case "finish":
      return Flag;
    default:
      return Wrench;
  }
}

// Short, human key argument per tool (plan §10: "path/lệnh") — never
// the full write_file content (tool_call events already cap preview).
function argSummary(name: string, argsJson: string): string {
  let a: Record<string, unknown> = {};
  try {
    a = JSON.parse(argsJson) as Record<string, unknown>;
  } catch {
    return argsJson.slice(0, 80);
  }
  switch (name) {
    case "write_file":
    case "edit_file":
    case "read_file":
      return String(a.path ?? "");
    case "list_files":
      return String(a.path ?? ".");
    case "search":
      return String(a.pattern ?? a.query ?? "");
    case "run_command":
      return String(a.command ?? a.cmd ?? "");
    case "check_preview":
      return String(a.path ?? "/");
    case "finish":
      return "";
    default:
      return argsJson.slice(0, 80);
  }
}

export function useAgentRunEvents(orgID: string, runID: string, active: boolean, onFinished?: () => void) {
  const [events, setEvents] = useState<AgentEvent[]>([]);
  const sawDone = useRef(false);
  const qc = useQueryClient();

  useEffect(() => {
    if (!active || sawDone.current || !runID) return;
    const types = ["status", "message", "tool_call", "tool_result", "build", "reverted", "done"];
    const es = new EventSource(api.agentRunEventsUrl(orgID, runID));
    const push = (type: string) => (e: MessageEvent) => {
      let data: Record<string, unknown> = {};
      try {
        data = JSON.parse(e.data as string) as Record<string, unknown>;
      } catch {
        // A malformed frame is skipped; seq ordering comes from the id.
      }
      const seq = Number(e.lastEventId || 0);
      setEvents((prev) => (prev.some((p) => p.seq === seq) ? prev : [...prev, { seq, type, data }]));
      if (type === "done") {
        sawDone.current = true;
        es.close();
        // The worker wrote the final row + the summary chat message in
        // the same tx — refresh both the runs list and the transcript.
        void qc.invalidateQueries({ queryKey: ["agent-runs", orgID] });
        void qc.invalidateQueries({ queryKey: ["chat-messages", orgID] });
        onFinished?.();
      }
    };
    for (const t of types) es.addEventListener(t, push(t) as EventListener);
    // EventSource reconnects (with Last-Event-ID) on transient drops;
    // a stream closed by the server after `done` is handled above.
    return () => es.close();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [orgID, runID, active]);

  return events;
}

export function AgentRunCard({
  orgID,
  run,
  onContinue,
}: {
  orgID: string;
  run: AgentRun;
  onContinue?: (prompt: string) => void;
}) {
  const toast = useToast();
  const confirm = useConfirm();
  const qc = useQueryClient();
  const live = LIVE_STATUSES.has(run.status);
  const events = useAgentRunEvents(orgID, run.id, live);
  const [openTools, setOpenTools] = useState<Record<string, boolean>>({});
  const [busy, setBusy] = useState(false);

  // Merge events into view state. `run.result` (REST) and the `done`
  // event carry the same payload; the REST row wins once invalidated.
  const view = useMemo(() => {
    const tools: ToolView[] = [];
    const byId = new Map<string, ToolView>();
    const segments: string[] = [];
    let current = "";
    let phase = "";
    let build: { status: string; command: string; tail: string } | null = null;
    let reverted: { restored: number; removed: number; remaining: number } | null = null;
    for (const e of events) {
      const d = e.data;
      switch (e.type) {
        case "status":
          phase = String(d.phase ?? d.status ?? "");
          break;
        case "message":
          current += String(d.delta ?? "");
          break;
        case "tool_call": {
          const id = String(d.id ?? "");
          const t: ToolView = {
            id,
            name: String(d.name ?? "?"),
            args: typeof d.args_preview === "string" ? d.args_preview : JSON.stringify(d.args_preview ?? {}),
          };
          if (current.trim()) {
            segments.push(current);
            current = "";
          }
          byId.set(id, t);
          tools.push(t);
          break;
        }
        case "tool_result": {
          const t = byId.get(String(d.id ?? ""));
          if (!t) break;
          t.ok = Boolean(d.ok);
          t.exitCode = typeof d.exit_code === "number" ? d.exit_code : undefined;
          t.durationMs = typeof d.duration_ms === "number" ? d.duration_ms : undefined;
          t.output = typeof d.output_preview === "string" ? d.output_preview : undefined;
          t.truncated = Boolean(d.truncated);
          t.errorCode = typeof d.error_code === "string" ? d.error_code : undefined;
          if (!t.ok && typeof d.error === "string") t.errorCode = d.error;
          break;
        }
        case "build":
          build = {
            status: String(d.status ?? ""),
            command: String(d.command ?? ""),
            tail: typeof d.tail === "string" ? d.tail : "",
          };
          break;
        case "reverted":
          reverted = {
            restored: Number(d.files_restored ?? 0),
            removed: Number(d.files_removed ?? 0),
            remaining: Number(d.files_remaining ?? 0),
          };
          break;
      }
    }
    if (current.trim()) segments.push(current);
    return { tools, segments, phase, build, reverted };
  }, [events]);

  const result = run.result;
  const startedAt = run.started_at ? new Date(run.started_at).getTime() : null;
  const finishedAt = run.finished_at ? new Date(run.finished_at).getTime() : null;
  const durationS = startedAt && finishedAt ? Math.round((finishedAt - startedAt) / 1000) : null;

  async function cancel() {
    setBusy(true);
    try {
      await api.cancelAgentRun(orgID, run.id);
      toast("success", "Đã gửi yêu cầu dừng — run kết thúc trong ≤ 5 s.");
      await qc.invalidateQueries({ queryKey: ["agent-runs", orgID] });
    } catch (err) {
      toast("error", err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  async function revert() {
    const ok = await confirm({
      title: "Revert run này?",
      description: "Khôi phục sandbox về checkpoint trước khi run chạy: file đã thêm bị xoá, file đã sửa trở lại nội dung cũ. File bạn tạo ngoài run không bị đụng tới.",
      confirmLabel: "Revert",
      danger: true,
    });
    if (!ok) return;
    setBusy(true);
    try {
      await api.revertAgentRun(orgID, run.id);
      toast("success", "Đã revert run về checkpoint.");
      await qc.invalidateQueries({ queryKey: ["agent-runs", orgID] });
    } catch (err) {
      toast("error", err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  const statusBadge = () => {
    switch (run.status) {
      case "queued":
        return <Badge variant="neutral">queued</Badge>;
      case "running":
        return (
          <Badge variant="brand">
            <Loader2 aria-hidden className="size-3 animate-spin" />
            {run.cancel_requested ? "stopping…" : "running"}
          </Badge>
        );
      case "succeeded":
        return (
          <Badge variant="success">
            <Check aria-hidden className="size-3" />
            succeeded
          </Badge>
        );
      case "cancelled":
        return <Badge variant="warning">cancelled</Badge>;
      default:
        return (
          <Badge variant="danger">
            <X aria-hidden className="size-3" />
            failed{run.error_code ? ` · ${run.error_code}` : ""}
          </Badge>
        );
    }
  };

  const canRevert =
    run.status === "succeeded" && !run.reverted_at && (result?.files_changed?.length ?? 0) >= 0;

  return (
    <div
      className="ml-10 rounded-xl border border-border bg-surface"
      data-testid="agent-run-card"
      data-status={run.status}
      data-run-id={run.id}
    >
      {/* header: status + counters + actions */}
      <div className="flex flex-wrap items-center gap-2 border-b border-border px-3 py-2">
        {statusBadge()}
        <span className="text-xs text-muted" data-testid="run-meta">
          {run.model}
          {result ? ` · step ${result.steps ?? run.steps}/${run.max_steps}` : ""}
          {result ? ` · ↑${result.input_tokens ?? run.input_tokens} ↓${result.output_tokens ?? run.output_tokens} tok${result.usage_estimated ? "~" : ""}` : ""}
          {durationS !== null ? ` · ${durationS}s` : ""}
        </span>
        {live && (
          <Button
            variant="secondary"
            size="sm"
            className="ml-auto"
            aria-label="Dừng run"
            onClick={() => void cancel()}
            disabled={busy || run.cancel_requested}
          >
            <Square aria-hidden className="size-3.5" />
            Dừng
          </Button>
        )}
        {canRevert && (
          <Button
            variant="secondary"
            size="sm"
            className={live ? "" : "ml-auto"}
            aria-label="Revert run"
            onClick={() => void revert()}
            disabled={busy}
          >
            <RotateCcw aria-hidden className="size-3.5" />
            Revert
          </Button>
        )}
        {!live && (run.status === "failed" || run.status === "cancelled") && onContinue && (
          <Button
            variant="secondary"
            size="sm"
            className="ml-auto"
            aria-label="Tiếp tục"
            onClick={() => onContinue(run.prompt)}
            disabled={busy}
          >
            Tiếp tục
          </Button>
        )}
      </div>

      {/* streamed assistant text between tool rounds */}
      {view.segments.length > 0 && (
        <div className="space-y-2 px-3 pt-3">
          {view.segments.map((s, i) => (
            <div key={i} className="text-sm text-fg">
              <Markdown text={s} />
            </div>
          ))}
        </div>
      )}

      {/* tool rows: collapsed one-liners, expandable output preview */}
      {view.tools.length > 0 && (
        <ul className="space-y-1 px-3 py-3">
          {view.tools.map((t) => {
            const Icon = toolIcon(t.name);
            const open = openTools[t.id] ?? false;
            const hasDetail = Boolean(t.output || t.errorCode);
            return (
              <li key={t.id} data-testid="tool-row" data-tool={t.name}>
                <button
                  className={cn(
                    "flex w-full items-center gap-2 rounded-[var(--radius)] px-2 py-1.5 text-left text-xs transition-colors hover:bg-surface-2",
                    hasDetail && "cursor-pointer",
                  )}
                  aria-expanded={open}
                  onClick={() => hasDetail && setOpenTools((o) => ({ ...o, [t.id]: !o[t.id] }))}
                >
                  {t.ok === undefined ? (
                    <Loader2 aria-hidden className="size-3.5 shrink-0 animate-spin text-muted" />
                  ) : t.ok ? (
                    <Check aria-hidden className="size-3.5 shrink-0 text-success" />
                  ) : (
                    <X aria-hidden className="size-3.5 shrink-0 text-danger" />
                  )}
                  <Icon aria-hidden className="size-3.5 shrink-0 text-muted" />
                  <span className="font-medium text-fg">{t.name}</span>
                  <span className="min-w-0 flex-1 truncate font-mono text-muted">{argSummary(t.name, t.args)}</span>
                  {t.durationMs !== undefined && <span className="shrink-0 text-muted">{t.durationMs}ms</span>}
                  {hasDetail ? (
                    open ? (
                      <ChevronDown aria-hidden className="size-3.5 shrink-0 text-muted" />
                    ) : (
                      <ChevronRight aria-hidden className="size-3.5 shrink-0 text-muted" />
                    )
                  ) : null}
                </button>
                {open && (
                  <div className="mt-1 max-h-40 overflow-auto rounded-[var(--radius)] border border-border bg-bg px-2 py-1.5 font-mono text-xs whitespace-pre-wrap text-fg">
                    {t.output || t.errorCode || "(không có output)"}
                    {t.truncated ? "\n… (output bị cắt)" : ""}
                  </div>
                )}
              </li>
            );
          })}
        </ul>
      )}

      {/* build + reverted lines */}
      {(view.build || view.reverted) && (
        <div className="space-y-1 px-3 pb-3 text-xs text-muted">
          {view.build && (
            <p className="flex items-center gap-1.5" data-testid="build-line">
              <Hammer aria-hidden className="size-3.5" />
              build: <span className={view.build.status === "passed" ? "text-success" : "text-fg"}>{view.build.status}</span>
              {view.build.command ? <code className="font-mono">{view.build.command}</code> : null}
            </p>
          )}
          {view.reverted && (
            <p className="flex items-center gap-1.5" data-testid="reverted-line">
              <RotateCcw aria-hidden className="size-3.5" />
              đã revert: {view.reverted.restored} file khôi phục · {view.reverted.removed} file xoá
            </p>
          )}
        </div>
      )}

      {/* result footer */}
      {result && !live && (
        <div className="space-y-2 border-t border-border px-3 py-3" data-testid="run-result">
          <p className="whitespace-pre-wrap text-sm text-fg" data-testid="result-summary">
            {result.summary}
          </p>
          {(result.files_changed?.length ?? 0) > 0 && (
            <div className="flex flex-wrap gap-1.5" data-testid="files-changed">
              {result.files_changed!.map((f) => (
                <Badge key={f.path} variant={f.status === "A" ? "success" : f.status === "D" ? "danger" : "neutral"}>
                  {f.path} · {f.status}
                </Badge>
              ))}
            </div>
          )}
          <div className="flex flex-wrap items-center gap-3 text-xs text-muted">
            {result.build_status && result.build_status !== "skipped" && (
              <span>build: {result.build_status}</span>
            )}
            {result.preview_ok !== undefined && (
              <span className={result.preview_ok ? "text-success" : ""}>
                preview: {result.preview_ok ? "ok" : "no"}
              </span>
            )}
            <span>
              ↑{result.input_tokens ?? 0} ↓{result.output_tokens ?? 0} tok{result.usage_estimated ? "~" : ""}
            </span>
          </div>
          {result.warning && (
            <p className="flex items-start gap-1.5 text-xs text-warning">
              <TriangleAlert aria-hidden className="mt-0.5 size-3.5 shrink-0" />
              {result.warning}
            </p>
          )}
          {run.reverted_at && (
            <p className="flex items-center gap-1.5 text-xs text-muted" data-testid="reverted-badge">
              <RotateCcw aria-hidden className="size-3.5" />
              run đã được revert
            </p>
          )}
        </div>
      )}

      {/* failed without result footer: show the error line */}
      {!result && !live && run.error_code && (
        <div className="px-3 py-3 text-sm text-danger">Run thất bại ({run.error_code}).</div>
      )}

      {/* live phase hint */}
      {live && !result && (
        <div className="px-3 pb-3 text-xs text-muted">
          {view.phase === "postcheck" ? "hậu kiểm (build/preview)…" : view.phase === "checkpoint" ? "tạo checkpoint git…" : view.phase === "running" ? "đang chạy tool…" : view.phase === "starting" ? "chuẩn bị sandbox…" : "đang khởi động…"}
        </div>
      )}
    </div>
  );
}
