import { useEffect, useState } from "react";
import { Cpu, ExternalLink, MemoryStick, Network, Pause, Play, RotateCw, SquareTerminal, Timer } from "lucide-react";
import { get as apiGet, sse, type Sandbox as ApiSandbox } from "@/console/api";
import { navigate as go } from "@/console/ui";
import { usePlatform } from "@/lib/platform-store";
import { cn } from "@/lib/utils";
import { AreaChart } from "../charts";
import { KpiCard, Mono, Panel, SectionTitle, StatusPill } from "../primitives";

/* Sandbox detail in the SBX template layout, fed only by sandboxd:
   GET /v1/sandboxes/{id} for the facts and the metrics SSE stream for the
   live numbers + charts. Exec/logs/keep-alive/delete live on the
   Terminal page and egress rules + live traffic on the Network page. */

interface Metrics {
  status: string;
  stats: { cpu_percent: number; mem_usage_bytes: number; mem_limit_bytes: number; pids: number };
}

const HISTORY = 90; // samples kept for the charts (≈3 min at 2s)

function fmtBytes(n: number): string {
  if (!n) return "0 B";
  const u = ["B", "KiB", "MiB", "GiB", "TiB"];
  const i = Math.min(u.length - 1, Math.floor(Math.log(n) / Math.log(1024)));
  return `${(n / 1024 ** i).toFixed(i ? 1 : 0)} ${u[i]}`;
}

function fmtTime(iso?: string): string {
  if (!iso) return "—";
  const d = new Date(iso);
  return isNaN(d.getTime()) ? "—" : d.toLocaleString();
}

function Row({ k, children }: { k: string; children: React.ReactNode }) {
  return (
    <div className="flex items-start justify-between gap-4 border-b border-line-soft py-2.5 last:border-0">
      <Mono className="shrink-0 text-xs text-gray-500">{k}</Mono>
      <div className="min-w-0 text-right text-[13px] text-gray-200 [overflow-wrap:anywhere]">{children}</div>
    </div>
  );
}

const btn =
  "inline-flex items-center gap-2 rounded-lg border px-3.5 py-2 text-xs font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-40";

export function SandboxDetailView() {
  const sandbox = usePlatform((s) => s.sandboxes.find((b) => b.id === s.selectedSandboxId));
  const closeSandbox = usePlatform((s) => s.closeSandbox);
  const setSandboxStatus = usePlatform((s) => s.setSandboxStatus);
  const restartSandbox = usePlatform((s) => s.restartSandbox);
  const me = usePlatform((s) => s.me);
  const canWrite = me?.role === "admin" || me?.role === "operator";

  const id = sandbox?.id;
  const status = sandbox?.status;
  const [info, setInfo] = useState<ApiSandbox | null>(null);
  const [m, setM] = useState<Metrics | null>(null);
  const [cpu, setCpu] = useState<number[]>([]);
  const [mem, setMem] = useState<number[]>([]);

  useEffect(() => {
    if (!id) return;
    let live = true;
    apiGet<ApiSandbox>(`/v1/sandboxes/${id}`)
      .then((d) => live && setInfo(d))
      .catch(() => live && setInfo(null));
    return () => {
      live = false;
    };
  }, [id, status]);

  useEffect(() => {
    setCpu([]);
    setMem([]);
    setM(null);
    if (!id || status !== "Running") return;
    return sse(`/v1/sandboxes/${id}/metrics/stream?interval=2`, (line) => {
      try {
        const f: Metrics = JSON.parse(line);
        setM(f);
        const st = f.stats;
        if (st && st.mem_limit_bytes > 0) {
          setCpu((xs) => [...xs, st.cpu_percent].slice(-HISTORY));
          setMem((xs) => [...xs, (st.mem_usage_bytes / st.mem_limit_bytes) * 100].slice(-HISTORY));
        }
      } catch {
        /* partial frame */
      }
    });
  }, [id, status]);

  if (!sandbox) return null;
  const running = sandbox.status === "Running";
  const st = m?.stats;
  const ports = info?.ports ?? [];

  return (
    <div className="space-y-5">
      {/* ── header ───────────────────────────────────────────────── */}
      <div className="flex flex-col gap-4 lg:flex-row lg:items-center lg:justify-between">
        <div className="flex min-w-0 flex-wrap items-center gap-3">
          <button
            onClick={() => {
              closeSandbox();
              go("/sandboxes");
            }}
            className="grid size-9 shrink-0 place-items-center rounded-lg border border-line text-gray-400 outline-none transition-colors hover:bg-fg/5 hover:text-gray-200 focus-visible:ring-2 focus-visible:ring-ring"
            aria-label="Back to sandbox list"
          >
            <span className="text-sm">←</span>
          </button>
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-2.5">
              <h1 className="truncate text-[24px] font-bold tracking-tight text-ink">{sandbox.name}</h1>
              {sandbox.name !== sandbox.id && (
                <Mono className="rounded-md border border-line bg-surface px-2 py-1 text-xs text-info">{sandbox.id}</Mono>
              )}
              <span className="rounded-md border border-line bg-surface px-2.5 py-1.5">
                <StatusPill status={sandbox.status} />
              </span>
            </div>
            <Mono className="mt-1 block text-[11px] text-gray-500 [overflow-wrap:anywhere]">
              {sandbox.image} · node {sandbox.host} · {sandbox.env}
            </Mono>
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-2.5">
          <button onClick={() => go(`/sandboxes/${sandbox.id}/terminal`)} className={cn(btn, "border-line bg-surface text-gray-300 hover:bg-fg/5")}>
            <SquareTerminal className="size-3.5" /> Terminal
          </button>
          <button onClick={() => go(`/sandboxes/${sandbox.id}/network`)} className={cn(btn, "border-line bg-surface text-gray-300 hover:bg-fg/5")}>
            <Network className="size-3.5" /> Network
          </button>
          {canWrite && (
            <>
              <button
                onClick={() => restartSandbox(sandbox.id)}
                disabled={sandbox.status === "Starting"}
                className={cn(btn, "border-line bg-surface text-gray-300 hover:bg-fg/5")}
              >
                <RotateCw className={cn("size-3.5", sandbox.status === "Starting" && "animate-spin")} /> Restart
              </button>
              {running ? (
                <button
                  onClick={() => setSandboxStatus(sandbox.id, "Stopped")}
                  className={cn(btn, "border-warn/40 bg-warn/10 text-warn hover:bg-warn/20")}
                >
                  <Pause className="size-3.5" /> Stop
                </button>
              ) : (
                <button
                  onClick={() => setSandboxStatus(sandbox.id, "Running")}
                  disabled={sandbox.status === "Starting"}
                  className={cn(btn, "border-neon/40 bg-neon/10 text-neon hover:bg-neon/20")}
                >
                  <Play className="size-3.5" /> Start
                </button>
              )}
            </>
          )}
        </div>
      </div>

      {info?.error_message && (
        <Panel className="border-danger/40 p-4 text-sm text-danger">{info.error_message}</Panel>
      )}

      {/* ── KPIs ─────────────────────────────────────────────────── */}
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-4">
        <KpiCard
          icon={<Cpu className="size-4" />}
          label="CPU"
          vi={`limit ${sandbox.vcpu} vCPU`}
          value={st ? `${st.cpu_percent.toFixed(1)}%` : "—"}
          accent="text-neon"
        />
        <KpiCard
          icon={<MemoryStick className="size-4" />}
          label="Memory"
          vi={st ? `of ${fmtBytes(st.mem_limit_bytes)}` : `limit ${sandbox.memGb} GB`}
          value={st ? fmtBytes(st.mem_usage_bytes) : "—"}
          accent="text-info"
        />
        <KpiCard
          icon={<SquareTerminal className="size-4" />}
          label="Processes"
          vi="pids"
          value={st ? st.pids : "—"}
        />
        <KpiCard icon={<Timer className="size-4" />} label="Uptime" vi="since created" value={sandbox.uptime} />
      </div>

      {/* ── charts ───────────────────────────────────────────────── */}
      <div className="grid grid-cols-1 gap-4 xl:grid-cols-2">
        {(
          [
            ["CPU usage", "Mức sử dụng CPU", cpu, "neon"],
            ["Memory usage", "Mức sử dụng bộ nhớ", mem, "cyan"],
          ] as const
        ).map(([title, vi, data, tone]) => (
          <Panel key={title} className="p-6">
            <SectionTitle title={title} vi={vi} right={<Mono className="text-[10px] text-gray-500">{running ? "live · 2s" : "stopped"}</Mono>} />
            {data.length > 1 ? (
              <AreaChart data={[...data]} tone={tone} height={200} showXLabels={false} />
            ) : (
              <div className="grid h-[200px] place-items-center text-sm text-gray-500">
                {running ? "Collecting samples…" : "Start the sandbox to see live metrics."}
              </div>
            )}
          </Panel>
        ))}
      </div>

      {/* ── facts ────────────────────────────────────────────────── */}
      <div className="grid grid-cols-1 gap-4 xl:grid-cols-[2fr_1fr]">
        <Panel className="p-6">
          <SectionTitle title="Sandbox" vi="Thông tin sandbox" />
          <div className="mt-3">
            <Row k="id">
              <Mono>{sandbox.id}</Mono>
            </Row>
            <Row k="image">
              <Mono>{sandbox.image}</Mono>
            </Row>
            <Row k="command">
              <Mono>{info?.command?.length ? info.command.join(" ") : "image default"}</Mono>
            </Row>
            <Row k="resources">
              {sandbox.vcpu} vCPU · {info?.resources?.memory_mb ?? sandbox.memGb * 1024} MiB
            </Row>
            <Row k="node">{sandbox.host}</Row>
            <Row k="created">{fmtTime(info?.created_at)}</Row>
            <Row k="last active">{fmtTime(info?.last_active_at)}</Row>
            <Row k="keep-alive">
              {info?.lifecycle?.keep_alive ? "on" : "off"}
              {info?.lifecycle ? ` · idle timeout ${info.lifecycle.idle_timeout_secs || "default"}s` : ""}
            </Row>
          </div>
        </Panel>
        <Panel className="p-6">
          <SectionTitle title="Network" vi="Mạng & cổng" />
          <div className="mt-3">
            <Row k="egress">{sandbox.env}</Row>
            <Row k="allow">{info?.egress?.length ? <Mono>{info.egress.join(", ")}</Mono> : "—"}</Row>
            <Row k="block">{info?.egress_deny?.length ? <Mono>{info.egress_deny.join(", ")}</Mono> : "—"}</Row>
            <Row k="ports">{ports.length ? <Mono>{ports.join(", ")}</Mono> : "none"}</Row>
            <Row k="preview">
              {info?.preview?.url ? (
                <a href={info.preview.url} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 text-info hover:underline">
                  {info.preview.url.replace(/^https?:\/\//, "")} <ExternalLink className="size-3" />
                </a>
              ) : (
                "—"
              )}
            </Row>
          </div>
          <button
            onClick={() => go(`/sandboxes/${sandbox.id}/network`)}
            className="mt-4 inline-flex w-full items-center justify-center gap-2 rounded-lg border border-neon/30 bg-neon/10 px-4 py-2 text-sm text-neon transition-colors hover:bg-neon/20"
          >
            <Network className="size-4" /> Rules & live traffic
          </button>
        </Panel>
      </div>
    </div>
  );
}
