import { useEffect, useRef, useState } from "react";
import { Box, Cpu, MemoryStick, Pause, Play } from "lucide-react";
import { get as apiGet, sse } from "@/console/api";
import { navigate as go } from "@/console/ui";
import { cn } from "@/lib/utils";
import { AreaChart, Sparkline } from "../charts";
import { Mono, Panel, SectionTitle } from "../primitives";

/* Fleet monitoring in the SBX template layout, fed by GET /v1/monitoring
   (one docker-stats sample per running sandbox + a 10s-step fleet history
   the server keeps while this page polls) and the per-sandbox log SSE. */

interface MonResp {
  fleet: { total: number; running: number; avg_cpu: number; mem_used_mb: number; mem_limit_mb: number };
  sandboxes: { id: string; cpu_percent: number; mem_usage_bytes: number; mem_limit_bytes: number; pids: number }[];
  history: { t: string; cpu: number; mem_mb: number }[];
}

const POLL_MS = 10_000;

function fmtBytes(n: number): string {
  if (!n) return "0 B";
  const u = ["B", "KiB", "MiB", "GiB", "TiB"];
  const i = Math.min(u.length - 1, Math.floor(Math.log(n) / Math.log(1024)));
  return `${(n / 1024 ** i).toFixed(i ? 1 : 0)} ${u[i]}`;
}

function MetricHead({ label, vi, icon }: { label: string; vi: string; icon: React.ReactNode }) {
  return (
    <div className="flex items-center justify-between">
      <div>
        <div className="text-[13px] font-medium text-gray-200">{label}</div>
        <div className="text-[11px] text-gray-500">{vi}</div>
      </div>
      <div className="grid size-8 place-items-center rounded-lg border border-line bg-navy/60 text-gray-400">{icon}</div>
    </div>
  );
}

export function MonitoringView() {
  const [data, setData] = useState<MonResp | null>(null);
  const [error, setError] = useState("");
  const [pick, setPick] = useState("");
  const [lines, setLines] = useState<string[]>([]);
  const [paused, setPaused] = useState(false);
  const logRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    let live = true;
    const load = () =>
      apiGet<MonResp>("/v1/monitoring")
        .then((d) => {
          if (!live) return;
          setData(d);
          setError("");
        })
        .catch((e) => live && setError(e instanceof Error ? e.message : String(e)));
    void load();
    const t = setInterval(load, POLL_MS);
    return () => {
      live = false;
      clearInterval(t);
    };
  }, []);

  const running = data?.sandboxes.map((s) => s.id) ?? [];
  const streamId = pick && running.includes(pick) ? pick : running[0] ?? "";

  useEffect(() => {
    setLines([]);
    if (!streamId || paused) return;
    return sse(`/v1/sandboxes/${streamId}/logs?tail=100&follow=true`, (l) =>
      setLines((xs) => (xs.length > 1000 ? [...xs.slice(-800), l] : [...xs, l])),
    );
  }, [streamId, paused]);

  useEffect(() => {
    if (logRef.current) logRef.current.scrollTop = logRef.current.scrollHeight;
  }, [lines]);

  if (error && !data) {
    return <Panel className="p-6 text-sm text-danger">Could not load monitoring: {error}</Panel>;
  }
  if (!data) return <div className="p-6 font-mono text-xs text-gray-500">loading…</div>;

  const f = data.fleet;
  const cpuHist = data.history.map((h) => h.cpu);
  const memHist = data.history.map((h) => h.mem_mb);
  const memPct = f.mem_limit_mb ? (f.mem_used_mb / f.mem_limit_mb) * 100 : 0;

  return (
    <div className="space-y-6">
      {/* ── header ─────────────────────────────────────────────── */}
      <div className="flex flex-col gap-1">
        <h1 className="text-[28px] font-bold tracking-tight text-ink">Monitoring</h1>
        <p className="text-sm text-gray-400">
          Fleet resources, sampled every 10s while this page is open · <span className="text-gray-500">Giám sát tài nguyên</span>
        </p>
      </div>

      {/* ── KPI strip ──────────────────────────────────────────── */}
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
        <Panel className="p-5">
          <MetricHead label="Running" vi="Đang chạy" icon={<Box className="size-4" />} />
          <Mono className="mt-2 block text-[26px] font-semibold text-neon">
            {f.running}
            <span className="text-sm text-gray-500"> / {f.total}</span>
          </Mono>
        </Panel>
        <Panel className="p-5">
          <MetricHead label="Average CPU" vi="Vi xử lý" icon={<Cpu className="size-4" />} />
          <Mono className="mt-2 block text-[26px] font-semibold text-neon">{f.avg_cpu.toFixed(1)}%</Mono>
          {cpuHist.length > 1 && <Sparkline data={cpuHist.slice(-24)} tone="neon" className="mt-3" />}
        </Panel>
        <Panel className="p-5">
          <MetricHead label="Memory" vi="Bộ nhớ" icon={<MemoryStick className="size-4" />} />
          <Mono className="mt-2 block text-[26px] font-semibold text-info">
            {f.mem_used_mb}
            <span className="text-sm text-gray-500"> / {Math.round(f.mem_limit_mb)} MiB</span>
          </Mono>
          <div className="mt-3 h-1.5 w-full overflow-hidden rounded-full bg-line">
            <div className="h-full rounded-full bg-gradient-to-r from-info/50 to-info transition-all" style={{ width: `${Math.min(100, memPct)}%` }} />
          </div>
        </Panel>
      </div>

      {/* ── chart row ──────────────────────────────────────────── */}
      <div className="grid gap-4 xl:grid-cols-2">
        {(
          [
            ["Fleet CPU", "CPU trung bình", cpuHist, "neon", 100, "%"],
            ["Fleet memory", "Bộ nhớ đã dùng", memHist, "cyan", Math.max(64, f.mem_limit_mb), " MiB"],
          ] as const
        ).map(([title, vi, series, tone, yMax, unit]) => (
          <Panel key={title} className="p-6">
            <SectionTitle title={title} vi={vi} right={<Mono className="text-[10px] text-gray-600">last 8 min</Mono>} />
            {series.length > 1 ? (
              <AreaChart data={[...series]} tone={tone} height={210} yMax={yMax} yUnit={unit} showXLabels={false} className="mt-4" />
            ) : (
              <div className="mt-4 grid h-[210px] place-items-center text-sm text-gray-500">Collecting samples…</div>
            )}
          </Panel>
        ))}
      </div>

      {/* ── per sandbox + logs ─────────────────────────────────── */}
      <div className="grid gap-4 xl:grid-cols-2">
        <Panel className="overflow-hidden">
          <div className="p-6 pb-3">
            <SectionTitle title="Per-sandbox" vi="Từng sandbox" />
          </div>
          {data.sandboxes.length === 0 ? (
            <p className="px-6 pb-6 text-sm text-gray-500">Nothing is running.</p>
          ) : (
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-y border-line text-left font-mono text-[10.5px] uppercase tracking-wider text-gray-500">
                    <th className="px-6 py-2.5 font-medium">Sandbox</th>
                    <th className="px-3 py-2.5 font-medium">CPU</th>
                    <th className="px-3 py-2.5 font-medium">Memory</th>
                    <th className="px-6 py-2.5 font-medium">PIDs</th>
                  </tr>
                </thead>
                <tbody>
                  {data.sandboxes.map((s) => (
                    <tr key={s.id} className="border-b border-line-soft last:border-0 hover:bg-fg/[0.03]">
                      <td className="px-6 py-2.5">
                        <button onClick={() => go(`/sandboxes/${s.id}`)} className="font-mono text-xs text-info hover:underline">
                          {s.id}
                        </button>
                      </td>
                      <td className="px-3 py-2.5 font-mono text-xs text-gray-200">{s.cpu_percent.toFixed(1)}%</td>
                      <td className="px-3 py-2.5 font-mono text-xs text-gray-200">
                        {fmtBytes(s.mem_usage_bytes)} <span className="text-gray-500">/ {fmtBytes(s.mem_limit_bytes)}</span>
                      </td>
                      <td className="px-6 py-2.5 font-mono text-xs text-gray-200">{s.pids}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Panel>

        <Panel className="flex flex-col p-6">
          <SectionTitle
            title="Live logs"
            vi="Nhật ký trực tiếp"
            right={
              running.length > 0 && (
                <div className="flex items-center gap-2">
                  <select
                    value={streamId}
                    onChange={(e) => setPick(e.target.value)}
                    className="h-8 rounded-lg border border-line bg-navy/60 px-2 font-mono text-xs text-gray-200 outline-none"
                  >
                    {running.map((id) => (
                      <option key={id} value={id}>
                        {id}
                      </option>
                    ))}
                  </select>
                  <button
                    onClick={() => setPaused((p) => !p)}
                    className="grid size-8 place-items-center rounded-lg border border-line text-gray-400 hover:bg-fg/5"
                    aria-label={paused ? "Resume log stream" : "Pause log stream"}
                  >
                    {paused ? <Play className="size-3.5" /> : <Pause className="size-3.5" />}
                  </button>
                </div>
              )
            }
          />
          <div
            ref={logRef}
            className={cn(
              "mt-4 h-[300px] overflow-auto rounded-lg border border-line bg-sunken p-3 font-mono text-[11.5px] leading-relaxed text-gray-300",
              "whitespace-pre-wrap [overflow-wrap:anywhere]",
            )}
          >
            {!streamId ? "No running sandbox to attach to." : lines.length ? lines.join("\n") : paused ? "paused" : "waiting for output…"}
          </div>
        </Panel>
      </div>
    </div>
  );
}
