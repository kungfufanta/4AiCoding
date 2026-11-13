// Agent run UI (A3.4, docs/AGENT_PLAN_A3.md §10): the collapsed
// one-line tool cards, the final result card and the live phase line.
// Presentational only — chat-page.tsx owns the data flow (runs list,
// events SSE, cancel/revert mutations).

import { useState } from "react";
import {
  ChevronDown,
  Check,
  FilePenLine,
  FilePlus2,
  FileText,
  Flag,
  FolderTree,
  Globe,
  Loader2,
  Search,
  SquareTerminal,
  X,
} from "lucide-react";
import { cn } from "../../lib/cn";
import type { AgentRun, AgentRunResult } from "../../api";
import { Badge } from "../../ui/badge";
import { Button } from "../../ui/button";
import { CodeBlock } from "../../ui/code-block";
import type { AgentTimeline, AgentToolStep } from "./use-agent-run";
import { Markdown } from "./markdown";

// toolSummary renders the one-line subject of a tool call: path,
// pattern or command — never the written content (plan §9.3).
function toolSummary(step: AgentToolStep): string {
  const a = (step.args ?? {}) as Record<string, unknown>;
  const s = (k: string) => (typeof a[k] === "string" ? (a[k] as string) : "");
  switch (step.name) {
    case "write_file":
      return `${s("path")} · ${typeof a.bytes === "number" ? `${a.bytes} B` : "nội dung"}`;
    case "edit_file":
    case "read_file":
    case "list_files":
      return s("path") || ".";
    case "search":
      return s("pattern");
    case "run_command":
      return s("command");
    case "check_preview":
      return "kiểm tra preview";
    case "finish":
      return "kết thúc";
    default:
      return "";
  }
}

function ToolIcon({ name, className }: { name: string; className?: string }) {
  const cls = cn("size-3.5 shrink-0", className);
  switch (name) {
    case "write_file":
      return <FilePlus2 aria-hidden className={cls} />;
    case "edit_file":
      return <FilePenLine aria-hidden className={cls} />;
    case "read_file":
      return <FileText aria-hidden className={cls} />;
    case "list_files":
      return <FolderTree aria-hidden className={cls} />;
    case "search":
      return <Search aria-hidden className={cls} />;
    case "run_command":
      return <SquareTerminal aria-hidden className={cls} />;
    case "check_preview":
      return <Globe aria-hidden className={cls} />;
    case "finish":
      return <Flag aria-hidden className={cls} />;
    default:
      return <SquareTerminal aria-hidden className={cls} />;
  }
}

function fmtDuration(ms?: number): string {
  if (typeof ms !== "number" || ms < 0) return "";
  return ms >= 1000 ? `${(ms / 1000).toFixed(1)}s` : `${ms}ms`;
}

// ToolCard — the collapsed one-line card (icon, name, subject,
// ✓/✗, duration); expanded it shows the args and the output preview.
export function ToolCard({ step }: { step: AgentToolStep }) {
  const [open, setOpen] = useState(false);
  const pending = step.ok === undefined && step.error === undefined;
  const summary = toolSummary(step);
  const argsText = step.args ? JSON.stringify(step.args, null, 2) : "";
  return (
    <div className="rounded-[var(--radius)] border border-border bg-bg">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        aria-expanded={open}
        aria-label={`Tool ${step.name} ${summary}`}
        className="flex w-full items-center gap-2 px-2.5 py-1.5 text-left text-xs text-fg focus-visible:outline-2 focus-visible:outline-[var(--ring)]"
      >
        <ToolIcon name={step.name} className={pending ? "text-muted" : step.ok ? "text-success" : "text-danger"} />
        <span className="shrink-0 font-mono text-[11px] text-muted">{step.name}</span>
        <span className="min-w-0 flex-1 truncate font-mono text-[11px]">{summary}</span>
        {pending ? (
          <Loader2 aria-label="Đang chạy" className="size-3.5 shrink-0 animate-spin text-muted" />
        ) : step.error || step.ok === false ? (
          <X aria-label="Lỗi" className="size-3.5 shrink-0 text-danger" />
        ) : (
          <Check aria-label="Thành công" className="size-3.5 shrink-0 text-success" />
        )}
        {step.duration_ms !== undefined && (
          <span className="shrink-0 text-[11px] text-muted">{fmtDuration(step.duration_ms)}</span>
        )}
        <ChevronDown aria-hidden className={cn("size-3.5 shrink-0 text-muted transition-transform", open && "rotate-180")} />
      </button>
      {open && (
        <div className="space-y-2 border-t border-border px-2.5 py-2">
          {argsText && argsText !== "{}" && <CodeBlock code={argsText} ariaLabel="Tham số tool" />}
          {step.error && <p className="text-xs text-danger">Lỗi: {step.error}</p>}
          {step.output_preview && (
            <CodeBlock code={step.output_preview} ariaLabel="Kết quả tool" />
          )}
          {step.truncated && <p className="text-[11px] text-muted">Kết quả bị cắt (đủ để xem nhanh).</p>}
        </div>
      )}
    </div>
  );
}

const FILE_TONE: Record<string, string> = {
  A: "text-success",
  M: "text-warning",
  D: "text-danger",
  R: "text-brand",
};

// ResultCard — the end-of-run card: summary, files changed, build,
// preview, tokens; Revert (confirm dialog in the caller) and Tiếp tục.
export function ResultCard({
  run,
  result,
  canRevert,
  reverting,
  onRevert,
  onContinue,
}: {
  run: AgentRun;
  result: AgentRunResult | undefined;
  canRevert: boolean;
  reverting: boolean;
  onRevert: () => void;
  onContinue: () => void;
}) {
  if (!run.result && !result) return null;
  const res = (result ?? run.result)!;
  const failed = run.status === "failed";
  const cancelled = run.status === "cancelled";
  const tokens = res.input_tokens + res.output_tokens;
  return (
    <div className="ml-10 rounded-xl border border-border bg-surface px-3.5 py-3" data-testid="agent-result">
      <div className="flex flex-wrap items-center gap-1.5">
        {run.status === "succeeded" && <Badge variant="success">Hoàn thành</Badge>}
        {failed && <Badge variant="danger">Thất bại{run.error_code ? ` · ${run.error_code}` : ""}</Badge>}
        {cancelled && <Badge variant="warning">Đã dừng</Badge>}
        {run.reverted_at && <Badge variant="neutral">Đã hoàn tác</Badge>}
        {res.build_status === "passed" && <Badge variant="success">build ✓</Badge>}
        {res.build_status === "failed" && <Badge variant="danger">build ✗</Badge>}
        {res.preview_ok === true && <Badge variant="success">preview ✓</Badge>}
        {res.preview_ok === false && <Badge variant="warning">preview ✗</Badge>}
      </div>
      {res.summary && <div className="mt-2 text-sm text-fg"><Markdown text={res.summary} /></div>}
      {res.warning && <p className="mt-2 text-xs text-warning">{res.warning}</p>}
      {res.files_changed.length > 0 && (
        <div className="mt-2.5 flex flex-wrap gap-1.5" aria-label="Files changed">
          {res.files_changed.map((f) => (
            <span
              key={`${f.status}:${f.path}`}
              className="inline-flex max-w-full items-center gap-1 rounded-[var(--radius)] border border-border bg-bg px-1.5 py-0.5 font-mono text-[11px]"
            >
              <span className={cn("font-semibold", FILE_TONE[f.status] ?? "text-muted")}>{f.status}</span>
              <span className="truncate text-fg">{f.path}</span>
            </span>
          ))}
        </div>
      )}
      <p className="mt-2.5 text-[11px] text-muted">
        {res.steps} bước · {tokens.toLocaleString("vi-VN")} token
        {res.usage_estimated ? " (ước lượng)" : ""}
      </p>
      {(canRevert || failed || cancelled) && (
        <div className="mt-2.5 flex gap-2">
          {canRevert && (
            <Button variant="secondary" size="sm" onClick={onRevert} disabled={reverting} aria-label="Revert">
              {reverting ? <Loader2 aria-hidden className="size-4 animate-spin" /> : null}
              Hoàn tác
            </Button>
          )}
          {(failed || cancelled) && (
            <Button variant="secondary" size="sm" onClick={onContinue} aria-label="Continue">
              Tiếp tục
            </Button>
          )}
        </div>
      )}
    </div>
  );
}

// RunView — the live/finished block of the latest run: phase line,
// streaming text, tool cards, result card.
export function RunView({
  run,
  timeline,
  canRevert,
  reverting,
  canceling,
  onCancel,
  onRevert,
  onContinue,
}: {
  run: AgentRun;
  timeline: AgentTimeline;
  canRevert: boolean;
  reverting: boolean;
  canceling: boolean;
  onCancel: () => void;
  onRevert: () => void;
  onContinue: () => void;
}) {
  const active = run.status === "queued" || run.status === "running";
  const res = timeline.done?.result;
  return (
    <div className="space-y-2" data-testid="agent-run">
      {active && (
        <div className="ml-10 flex items-center gap-2 text-xs text-muted" data-testid="agent-phase">
          <Loader2 aria-hidden className="size-3.5 animate-spin" />
          {timeline.phase === "queued"
            ? "Đang chờ worker…"
            : timeline.phase === "checkpoint"
              ? "Đang tạo checkpoint git…"
              : timeline.phase === "postcheck"
                ? "Đang hậu kiểm (build, files_changed)…"
                : timeline.phase === "running"
                  ? `Đang chạy · bước ${Math.max(run.steps, timeline.tools.length ? 1 : 0)}`
                  : "Đang chuẩn bị…"}
          {timeline.tool_mode === "fallback" && <span className="text-warning">· chế độ dự phòng JSON</span>}
          <Button
            variant="secondary"
            size="sm"
            className="ml-auto"
            onClick={onCancel}
            disabled={canceling || run.cancel_requested}
            aria-label="Stop run"
          >
            {canceling || run.cancel_requested ? <Loader2 aria-hidden className="size-4 animate-spin" /> : "Dừng"}
          </Button>
        </div>
      )}
      {timeline.warning && <p className="ml-10 text-xs text-warning">{timeline.warning}</p>}
      {timeline.text && (
        <div className="ml-10 max-w-[85%] rounded-xl border border-border bg-surface px-3.5 py-2.5">
          <Markdown text={timeline.text} />
        </div>
      )}
      {timeline.tools.length > 0 && (
        <div className="ml-10 space-y-1.5" aria-label="Tool calls">
          {timeline.tools.map((t) => (
            <ToolCard key={`${t.id}:${t.name}`} step={t} />
          ))}
        </div>
      )}
      {timeline.build && timeline.build.status !== "skipped" && (
        <p className="ml-10 text-xs text-muted">
          build {timeline.build.status === "passed" ? "✓" : "✗"} · <span className="font-mono">{timeline.build.command}</span>
        </p>
      )}
      {!active && (
        <ResultCard
          run={run}
          result={res}
          canRevert={canRevert}
          reverting={reverting}
          onRevert={onRevert}
          onContinue={onContinue}
        />
      )}
      {timeline.reverted && (
        <p className="ml-10 text-xs text-muted" data-testid="agent-reverted">
          Đã hoàn tác: {timeline.reverted.files_removed} file xoá, {timeline.reverted.files_restored} file khôi phục.
        </p>
      )}
    </div>
  );
}
