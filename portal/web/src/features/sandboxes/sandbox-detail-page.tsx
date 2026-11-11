// Sandbox detail (PORTAL_PLAN_V2 P4): status header with start/stop/
// delete actions, then four tabs — Logs (SSE follow with auto-scroll
// that pauses when you scroll up), Exec (command box + output + exit
// code), Metrics (live CPU/RAM from the SSE stream) and Network
// (egress + ports, read-only).

import { useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate, useParams } from "react-router";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import {
  ArrowLeft,
  CircleStop,
  Cpu,
  Globe,
  Play,
  Square,
  Terminal,
  Trash2,
} from "lucide-react";
import { ApiError, api, type ExecResult, type Sandbox } from "../../api";
import { formatAge, formatBytes, formatDateTime } from "../../lib/format";
import { cn } from "../../lib/cn";
import { useShell } from "../../layout/app-shell";
import { Badge } from "../../ui/badge";
import { Button } from "../../ui/button";
import { Card } from "../../ui/card";
import { useConfirm } from "../../ui/dialog";
import { EmptyState, FullPageSpinner } from "../../ui/skeleton";
import { useToast } from "../../ui/toast";
import { statusTone, useSandbox } from "./hooks";

type Tab = "logs" | "exec" | "metrics" | "network";

const TABS: { id: Tab; label: string; icon: typeof Terminal }[] = [
  { id: "logs", label: "Logs", icon: Terminal },
  { id: "exec", label: "Exec", icon: Square },
  { id: "metrics", label: "Metrics", icon: Cpu },
  { id: "network", label: "Network", icon: Globe },
];

export function SandboxDetailPage() {
  const { sandboxId } = useParams();
  const { org } = useShell();
  const navigate = useNavigate();
  const toast = useToast();
  const confirm = useConfirm();
  const qc = useQueryClient();
  const [tab, setTab] = useState<Tab>("logs");

  const detail = useSandbox(org.id, sandboxId ?? "");
  const invalidate = async () => {
    await qc.invalidateQueries({ queryKey: ["sandboxes", org.id] });
    await qc.invalidateQueries({ queryKey: ["sandbox", org.id, sandboxId] });
  };

  const stop = useMutation({
    mutationFn: () => api.stopSandbox(org.id, sandboxId ?? ""),
    onSuccess: async () => {
      await invalidate();
      toast("success", "Stop signal sent");
    },
    onError: (err) => toast("error", err instanceof Error ? err.message : String(err)),
  });
  const start = useMutation({
    mutationFn: () => api.startSandbox(org.id, sandboxId ?? ""),
    onSuccess: async () => {
      await invalidate();
      toast("success", "Start signal sent");
    },
    onError: (err) => toast("error", err instanceof Error ? err.message : String(err)),
  });
  const remove = useMutation({
    mutationFn: () => api.deleteSandbox(org.id, sandboxId ?? ""),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ["sandboxes", org.id] });
      toast("success", "Sandbox deleted");
      navigate(`/orgs/${org.slug}/sandboxes`, { replace: true });
    },
    onError: (err) => toast("error", err instanceof Error ? err.message : String(err)),
  });

  if (detail.isPending) return <FullPageSpinner label="Loading sandbox…" />;

  const notFound = detail.isError && detail.error instanceof ApiError && detail.error.status === 404;
  if (notFound || detail.isError) {
    return (
      <div className="space-y-4">
        <BackLink slug={org.slug} />
        <Card className="p-0">
          <EmptyState
            icon={<Square aria-hidden className="size-8" />}
            title={notFound ? "Sandbox not found" : "Could not load sandbox"}
            description={
              notFound
                ? "It was deleted, expired or never belonged to this org."
                : detail.error instanceof Error
                  ? detail.error.message
                  : "The sandbox backend is unreachable."
            }
          />
        </Card>
      </div>
    );
  }

  const sb = detail.data;

  async function deleteSandbox() {
    const ok = await confirm({
      title: "Delete this sandbox?",
      description:
        "The container, its workspace files and the sandbox itself are destroyed. This cannot be undone.",
      confirmLabel: "Delete sandbox",
      danger: true,
    });
    if (ok) remove.mutate();
  }

  const running = sb.status === "running";

  return (
    <div className="space-y-4">
      <BackLink slug={org.slug} />

      <div className="flex flex-wrap items-center gap-3">
        <h1 className="font-mono text-base font-semibold tracking-tight text-fg">{sb.id}</h1>
        <Badge variant={statusTone(sb.status)}>{sb.status}</Badge>
        <div className="ml-auto flex items-center gap-2">
          {running ? (
            <Button
              variant="secondary"
              size="sm"
              loading={stop.isPending}
              onClick={() => stop.mutate()}
            >
              <CircleStop aria-hidden className="size-4" />
              Stop
            </Button>
          ) : (
            <Button
              variant="secondary"
              size="sm"
              loading={start.isPending}
              onClick={() => start.mutate()}
            >
              <Play aria-hidden className="size-4" />
              Start
            </Button>
          )}
          <Button
            variant="ghost"
            size="sm"
            className="text-danger hover:bg-danger-soft"
            loading={remove.isPending}
            onClick={() => void deleteSandbox()}
          >
            <Trash2 aria-hidden className="size-4" />
            Delete
          </Button>
        </div>
      </div>

      {sb.error_message && (
        <Card className="border-danger/40 bg-danger/5 p-3.5 text-sm text-fg">
          {sb.error_message}
        </Card>
      )}

      <Card className="p-0">
        <div className="flex items-center justify-between gap-3 border-b border-border px-4 py-2.5">
          <p className="text-sm text-muted">
            {sb.image} · {sb.resources.cpus} vCPU · {sb.resources.memory_mb} MB · created{" "}
            {formatAge(sb.created_at)} ago
          </p>
          <p className="hidden text-xs text-muted sm:block">{formatDateTime(sb.created_at)}</p>
        </div>

        <div role="tablist" aria-label="Sandbox panels" className="flex gap-1 border-b border-border px-2 pt-1">
          {TABS.map(({ id, label, icon: Icon }) => (
            <button
              key={id}
              role="tab"
              aria-selected={tab === id}
              onClick={() => setTab(id)}
              className={cn(
                "flex items-center gap-1.5 rounded-t-[var(--radius)] px-3 py-2 text-sm transition-colors",
                tab === id
                  ? "border-b-2 border-brand font-medium text-brand"
                  : "text-muted hover:bg-surface-2 hover:text-fg",
              )}
            >
              <Icon aria-hidden className="size-4" />
              {label}
            </button>
          ))}
        </div>

        <div className="p-4">
          {tab === "logs" && <LogsPanel orgID={org.id} sandboxID={sb.id} running={running} />}
          {tab === "exec" && <ExecPanel orgID={org.id} sandboxID={sb.id} running={running} />}
          {tab === "metrics" && <MetricsPanel orgID={org.id} sandboxID={sb.id} running={running} />}
          {tab === "network" && <NetworkPanel sandbox={sb} />}
        </div>
      </Card>
    </div>
  );
}

function BackLink({ slug }: { slug: string }) {
  return (
    <Link
      to={`/orgs/${slug}/sandboxes`}
      className="inline-flex items-center gap-1.5 text-sm text-muted hover:text-fg"
    >
      <ArrowLeft aria-hidden className="size-4" />
      Sandboxes
    </Link>
  );
}

// --- Logs ------------------------------------------------------------

// LogsPanel streams ?follow=true over SSE (cookies ride along with
// native EventSource) and keeps the newest line in view unless the
// reader scrolled up — exactly the plan's auto-scroll + pause rule.
function LogsPanel({ orgID, sandboxID, running }: { orgID: string; sandboxID: string; running: boolean }) {
  const preRef = useRef<HTMLPreElement>(null);
  const stickRef = useRef(true);
  const [lines, setLines] = useState<string[]>([]);
  const [live, setLive] = useState(false);
  const [connected, setConnected] = useState(false);

  useEffect(() => {
    stickRef.current = true;
    setLines([]);
    setConnected(false);
    const es = new EventSource(api.sandboxLogsUrl(orgID, sandboxID, { tail: 500, follow: true }));
    es.onopen = () => setConnected(true);
    es.onmessage = (ev) => {
      setConnected(true);
      setLive(true);
      setLines((cur) => [...cur.slice(-2000), ev.data]);
    };
    es.addEventListener("end", () => es.close());
    es.onerror = () => setConnected(false);
    return () => es.close();
  }, [orgID, sandboxID]);

  // Auto-scroll only while the reader is at (or near) the bottom.
  useEffect(() => {
    const el = preRef.current;
    if (el && stickRef.current) el.scrollTop = el.scrollHeight;
  }, [lines]);

  return (
    <div className="space-y-2">
      <div className="flex items-center justify-between gap-2 text-xs text-muted">
        <span aria-live="polite">
          {!connected
            ? "Connecting…"
            : live
              ? "Live — new lines stream in as they are written"
              : "Streaming backlog…"}
        </span>
        <span>{lines.length} lines</span>
      </div>
      <pre
        ref={preRef}
        onScroll={(e) => {
          const el = e.currentTarget;
          stickRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 30;
        }}
        tabIndex={0}
        aria-label="Sandbox logs"
        className="max-h-[26rem] min-h-48 overflow-auto rounded-[var(--radius)] border border-border bg-bg p-3 font-mono text-xs leading-relaxed text-fg"
      >
        {lines.length > 0 ? lines.join("\n") : running ? "No output yet — the sandbox has not written to stdout/stderr." : "No log output. Start the sandbox to produce logs."}
      </pre>
    </div>
  );
}

// --- Exec ------------------------------------------------------------

function ExecPanel({ orgID, sandboxID, running }: { orgID: string; sandboxID: string; running: boolean }) {
  const [cmd, setCmd] = useState("");
  const [result, setResult] = useState<ExecResult | null>(null);
  const toast = useToast();

  const exec = useMutation({
    mutationFn: () => api.execSandbox(orgID, sandboxID, cmd),
    onSuccess: (res) => setResult(res),
    onError: (err) => toast("error", err instanceof Error ? err.message : "exec failed"),
  });

  return (
    <form
      className="space-y-3"
      onSubmit={(e) => {
        e.preventDefault();
        if (cmd.trim() && running && !exec.isPending) {
          setResult(null);
          exec.mutate();
        }
      }}
    >
      <div className="flex flex-col gap-2 sm:flex-row">
        <input
          value={cmd}
          onChange={(e) => setCmd(e.target.value)}
          placeholder={running ? "echo hello" : "start the sandbox to run commands"}
          aria-label="Command"
          spellCheck={false}
          autoComplete="off"
          disabled={!running}
          className="h-9 flex-1 rounded-[var(--radius)] border border-border bg-bg px-3 font-mono text-sm text-fg placeholder:text-muted/70 disabled:opacity-60"
        />
        <Button type="submit" loading={exec.isPending} disabled={!running || !cmd.trim()}>
          <Play aria-hidden className="size-4" />
          Run
        </Button>
      </div>
      {!running && (
        <p className="text-xs text-muted">
          The sandbox is <strong>{running ? "running" : "not running"}</strong>. Commands need a
          running container.
        </p>
      )}
      {result && (
        <div className="space-y-2">
          <div className="flex items-center gap-2 text-xs text-muted">
            <Badge variant={result.exit_code === 0 ? "success" : "danger"}>
              exit {result.exit_code}
            </Badge>
            {typeof result.duration_ms === "number" && <span>{result.duration_ms} ms</span>}
            {result.timed_out && <Badge variant="warning">timed out</Badge>}
            {result.truncated && <Badge variant="warning">output truncated</Badge>}
          </div>
          <pre
            aria-label="Command output"
            className="max-h-80 overflow-auto rounded-[var(--radius)] border border-border bg-bg p-3 font-mono text-xs leading-relaxed whitespace-pre-wrap text-fg"
          >
            {(result.stdout || result.stderr || "(no output)").trimEnd()}
          </pre>
        </div>
      )}
    </form>
  );
}

// --- Metrics ---------------------------------------------------------

type StatsSample = {
  sandbox_id: string;
  status: string;
  at: string;
  stats: {
    cpu_percent: number;
    mem_usage_bytes: number;
    mem_limit_bytes: number;
    mem_percent: number;
  };
};

const HISTORY = 60; // ~2 min at the 2 s default interval

function MetricsPanel({ orgID, sandboxID, running }: { orgID: string; sandboxID: string; running: boolean }) {
  const [samples, setSamples] = useState<StatsSample[]>([]);

  useEffect(() => {
    if (!running) return;
    const es = new EventSource(api.sandboxMetricsStreamUrl(orgID, sandboxID));
    es.onmessage = (ev) => {
      try {
        const sample = JSON.parse(ev.data) as StatsSample;
        setSamples((cur) => [...cur.slice(-HISTORY), sample]);
      } catch {
        // a malformed frame is skipped, the next one repairs the stream
      }
    };
    return () => es.close();
  }, [orgID, sandboxID, running]);

  const latest = samples[samples.length - 1];

  if (!running) {
    return (
      <EmptyState
        icon={<Cpu aria-hidden className="size-8" />}
        title="No live metrics"
        description="The sandbox is not running. Start it to stream CPU and memory samples."
      />
    );
  }
  if (!latest) {
    return <FullPageSpinner label="Waiting for the first sample…" />;
  }

  return (
    <div className="space-y-4">
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <MetricTile
          label="CPU"
          value={`${latest.stats.cpu_percent.toFixed(1)} %`}
          series={samples.map((s) => s.stats.cpu_percent)}
          max={100}
          ariaLabel="CPU percent history"
        />
        <MetricTile
          label="Memory"
          value={`${formatBytes(latest.stats.mem_usage_bytes)} / ${formatBytes(latest.stats.mem_limit_bytes)}`}
          series={samples.map((s) => s.stats.mem_percent)}
          max={100}
          ariaLabel="Memory percent history"
        />
      </div>
      <p className="text-xs text-muted">
        {samples.length} samples · newest {formatAge(latest.at)} ago · streamed every 2 s
      </p>
    </div>
  );
}

// MetricTile renders the current value plus a pure-SVG sparkline
// (plan §2: charts stay hand-drawn SVG, no chart library).
function MetricTile({
  label,
  value,
  series,
  max,
  ariaLabel,
}: {
  label: string;
  value: string;
  series: number[];
  max: number;
  ariaLabel: string;
}) {
  const points = useMemo(() => {
    const w = 100;
    const h = 28;
    if (series.length < 2) return "";
    const step = w / (series.length - 1);
    return series
      .map((v, i) => `${(i * step).toFixed(2)},${(h - Math.min(v, max) / max * h).toFixed(2)}`)
      .join(" ");
  }, [series, max]);

  return (
    <div className="rounded-[var(--radius)] border border-border bg-surface-2/50 p-3.5">
      <p className="text-xs font-medium text-muted">{label}</p>
      <p className="mt-0.5 text-lg font-semibold text-fg">{value}</p>
      <svg
        viewBox="0 0 100 28"
        preserveAspectRatio="none"
        role="img"
        aria-label={ariaLabel}
        className="mt-2 h-8 w-full text-brand"
      >
        {points && <polyline points={points} fill="none" stroke="currentColor" strokeWidth="1.5" />}
      </svg>
    </div>
  );
}

// --- Network ---------------------------------------------------------

function NetworkPanel({ sandbox: sb }: { sandbox: Sandbox }) {
  const egressEntries = sb.egress ?? [];
  return (
    <div className="space-y-4 text-sm">
      <section aria-label="Egress policy">
        <h3 className="text-sm font-semibold text-fg">Egress</h3>
        <p className="mt-1 text-muted">
          Mode: <Badge variant={sb.egress_mode === "open" ? "warning" : "neutral"}>{sb.egress_mode || "allowlist"}</Badge>
        </p>
        {sb.egress_mode !== "none" && (
          <>
            {egressEntries.length > 0 ? (
              <ul className="mt-2 flex flex-wrap gap-1.5">
                {egressEntries.map((h) => (
                  <li key={h}>
                    <Badge variant="neutral">{h}</Badge>
                  </li>
                ))}
              </ul>
            ) : (
              <p className="mt-2 text-xs text-muted">
                No explicit allow entries — the instance default policy applies.
              </p>
            )}
          </>
        )}
        {sb.egress_mode === "none" && (
          <p className="mt-2 text-xs text-muted">This sandbox has no outbound network access.</p>
        )}
      </section>

      <section aria-label="Ports">
        <h3 className="text-sm font-semibold text-fg">Ports</h3>
        {sb.ports.length > 0 ? (
          <ul className="mt-2 flex flex-wrap gap-1.5">
            {sb.ports.map((p) => (
              <li key={p}>
                <Badge variant="brand">:{p}</Badge>
              </li>
            ))}
          </ul>
        ) : (
          <p className="mt-2 text-xs text-muted">No preview ports declared.</p>
        )}
      </section>

      {sb.preview && (
        <section aria-label="Preview URL">
          <h3 className="text-sm font-semibold text-fg">Preview</h3>
          <a
            href={sb.preview.url}
            target="_blank"
            rel="noreferrer"
            className="mt-1 inline-flex items-center gap-1 text-brand hover:underline"
          >
            {sb.preview.url}
          </a>
          <p className="text-xs text-muted">Preview is {sb.preview.status}.</p>
        </section>
      )}
    </div>
  );
}
