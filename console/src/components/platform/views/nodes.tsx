import { useEffect, useState } from "react";
import { ChevronRight, Cpu, ExternalLink, GitPullRequest, HardDrive, MemoryStick, Network, RotateCw, Server } from "lucide-react";
import { toast } from "sonner";
import {
  ApiError,
  get as apiGet,
  post as apiPost,
  type HostTelemetry,
  type NodeChange,
  type NodeChangeList,
  type NodeInfo,
  type NodeMetrics,
} from "@/console/api";
import { navigate as go } from "@/console/ui";
import { usePlatform } from "@/lib/platform-store";
import { cn } from "@/lib/utils";
import { AreaChart } from "../charts";
import { KpiCard, Mono, PageHeader, Panel, SectionTitle, VI } from "../primitives";

/* Node resources in the SBX layout. Host numbers come from the worker image
   itself (procfs/statfs sent with every heartbeat): GET /v1/nodes for the
   fleet, GET /v1/nodes/{id}/metrics for one node's last hour. Admins change
   a node's advertised capacity through a pull request on the deploy repo
   (POST /v1/nodes/{id}/change-requests), never directly. */

const POLL_MS = 10_000;
const MiB = 1024 * 1024;

function fmtBytes(n: number): string {
  if (!n || n < 0) return "0 B";
  const u = ["B", "KiB", "MiB", "GiB", "TiB"];
  const i = Math.min(u.length - 1, Math.floor(Math.log(n) / Math.log(1024)));
  return `${(n / 1024 ** i).toFixed(i ? 1 : 0)} ${u[i]}`;
}
const mb = (n: number) => fmtBytes(n * MiB);
const rate = (bps: number) => `${fmtBytes(bps)}/s`;
const pct = (used: number, total: number) => (total > 0 ? Math.min(100, (used / total) * 100) : 0);

function fmtUptime(sec?: number) {
  if (!sec) return "—";
  const d = Math.floor(sec / 86400);
  const h = Math.floor((sec % 86400) / 3600);
  const m = Math.floor((sec % 3600) / 60);
  return d ? `${d}d ${h}h` : h ? `${h}h ${m}m` : `${m}m`;
}

function fmtTime(iso?: string) {
  if (!iso) return "—";
  const d = new Date(iso);
  return isNaN(d.getTime()) ? "—" : d.toLocaleString();
}

const stale = (n: NodeInfo) => !n.telemetry || Date.now() - new Date(n.telemetry.collected_at).getTime() > 2 * 60_000;

function NodePill({ status }: { status: string }) {
  const on = status === "online";
  return (
    <span className={cn("inline-flex items-center gap-1.5 text-xs font-medium", on ? "text-neon" : status === "offline" ? "text-danger" : "text-gray-400")}>
      <span className={cn("size-1.5 rounded-full", on ? "bg-neon dot-live" : status === "offline" ? "bg-danger" : "bg-gray-500")} />
      {status}
    </span>
  );
}

/** poll a loader every 10s while mounted */
function usePoll<T>(load: () => Promise<T>, deps: unknown[]) {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState("");
  const [tick, setTick] = useState(0);
  useEffect(() => {
    let live = true;
    const run = () =>
      load()
        .then((d) => {
          if (!live) return;
          setData(d);
          setError("");
        })
        .catch((e) => live && setError(e instanceof Error ? e.message : String(e)));
    void run();
    const t = setInterval(run, POLL_MS);
    return () => {
      live = false;
      clearInterval(t);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, tick]);
  return { data, error, reload: () => setTick((x) => x + 1) };
}

function Meter({ label, value, detail }: { label: string; value: number; detail: string }) {
  const tone = value >= 90 ? "from-danger/60 to-danger" : value >= 75 ? "from-warn/60 to-warn" : "from-neon/50 to-neon";
  return (
    <div className="space-y-1.5">
      <div className="flex items-baseline justify-between gap-3 text-xs">
        <span className="text-gray-400">{label}</span>
        <Mono className="truncate text-[11px] text-gray-500">{detail}</Mono>
      </div>
      <div
        className="h-1.5 w-full overflow-hidden rounded-full bg-line"
        role="meter"
        aria-label={label}
        aria-valuenow={Math.round(value)}
        aria-valuemin={0}
        aria-valuemax={100}
      >
        <div className={cn("h-full rounded-full bg-gradient-to-r transition-all", tone)} style={{ width: `${value}%` }} />
      </div>
    </div>
  );
}

function TelemetryMeters({ t }: { t: HostTelemetry }) {
  return (
    <div className="space-y-3.5">
      <Meter label="CPU" value={t.cpu.usage_percent} detail={`${t.cpu.usage_percent.toFixed(1)}% of ${t.cpu.cores} cores · load ${t.cpu.load1.toFixed(2)}`} />
      <Meter label="Memory" value={pct(t.memory.used_mb, t.memory.total_mb)} detail={`${mb(t.memory.used_mb)} / ${mb(t.memory.total_mb)}`} />
      <Meter
        label={`Disk${t.disk.path ? ` ${t.disk.path}` : ""}`}
        value={pct(t.disk.used_mb, t.disk.total_mb)}
        detail={`${mb(t.disk.used_mb)} / ${mb(t.disk.total_mb)}`}
      />
      <div className="flex items-baseline justify-between gap-3 text-xs">
        <span className="text-gray-400">Network</span>
        <Mono className="truncate text-[11px] text-gray-500">
          ↓ {rate(t.network.rx_bps)} · ↑ {rate(t.network.tx_bps)}
          {!t.network.host_namespace && " (container ns)"}
        </Mono>
      </div>
    </div>
  );
}

/* ══ /nodes ═══════════════════════════════════════════════════════ */
export function NodesView() {
  const { data, error, reload } = usePoll(() => apiGet<{ nodes: NodeInfo[] }>("/v1/nodes"), []);

  if (error && !data) return <Panel className="p-6 text-sm text-danger">Could not load nodes: {error}</Panel>;
  if (!data) return <div className="p-6 font-mono text-xs text-gray-500">loading…</div>;

  const nodes = data.nodes;
  const tel = nodes.filter((n) => n.telemetry).map((n) => n.telemetry!);
  const sum = (f: (t: HostTelemetry) => number) => tel.reduce((a, t) => a + f(t), 0);
  const online = nodes.filter((n) => n.status === "online").length;

  return (
    <div className="space-y-6">
      <PageHeader
        title="Node resources"
        subtitle="Host CPU, memory, disk and network reported by every node"
        vi="Tài nguyên máy chủ"
        actions={
          <button
            onClick={reload}
            className="inline-flex items-center gap-2 rounded-lg border border-line bg-surface px-3.5 py-2 text-xs font-medium text-gray-300 hover:bg-fg/5"
          >
            <RotateCw className="size-3.5" /> Refresh
          </button>
        }
      />

      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-4">
        <KpiCard icon={<Server className="size-4" />} label="Nodes online" vi="Đang hoạt động" value={`${online} / ${nodes.length}`} accent="text-neon" />
        <KpiCard icon={<Cpu className="size-4" />} label="Host cores" vi="reporting nodes" value={sum((t) => t.cpu.cores)} accent="text-info" />
        <KpiCard
          icon={<MemoryStick className="size-4" />}
          label="Host memory used"
          vi={`of ${mb(sum((t) => t.memory.total_mb))}`}
          value={mb(sum((t) => t.memory.used_mb))}
        />
        <KpiCard
          icon={<HardDrive className="size-4" />}
          label="Host disk used"
          vi={`of ${mb(sum((t) => t.disk.total_mb))}`}
          value={mb(sum((t) => t.disk.used_mb))}
        />
      </div>

      {nodes.length === 0 && <Panel className="p-6 text-sm text-gray-500">No nodes registered yet.</Panel>}

      <div className="grid gap-4 xl:grid-cols-2">
        {nodes.map((n) => (
          <Panel key={n.node_id} className="flex flex-col p-6">
            <div className="flex items-start justify-between gap-3">
              <div className="min-w-0">
                <div className="flex flex-wrap items-center gap-2.5">
                  <h2 className="truncate font-mono text-sm font-semibold text-ink">{n.node_id}</h2>
                  <NodePill status={n.status} />
                  {n.drain_state === "drain" && (
                    <span className="rounded-md border border-warn/30 bg-warn/10 px-1.5 py-0.5 font-mono text-[10px] text-warn">draining</span>
                  )}
                </div>
                <Mono className="mt-1 block text-[11px] text-gray-500 [overflow-wrap:anywhere]">
                  {n.role}
                  {n.telemetry?.hostname && ` · host ${n.telemetry.hostname}`}
                  {n.telemetry?.in_container && " · in container"} · advertises {n.capacity.cpus} CPU / {mb(n.capacity.memory_mb)}
                  {n.capacity.disk_mb > 0 && ` / ${mb(n.capacity.disk_mb)} disk`} · {n.sandboxes} sandbox(es)
                </Mono>
              </div>
              <button
                onClick={() => go(`/nodes/${n.node_id}`)}
                className="inline-flex shrink-0 items-center gap-1 rounded-lg border border-line px-3 py-1.5 text-xs text-gray-300 hover:bg-fg/5 hover:text-neon"
              >
                Details <ChevronRight className="size-3.5" />
              </button>
            </div>
            <div className="mt-5 flex-1">
              {n.telemetry ? (
                <>
                  <TelemetryMeters t={n.telemetry} />
                  {stale(n) && (
                    <p className="mt-3 text-xs text-danger">Last sample {fmtTime(n.telemetry.collected_at)}: the node stopped reporting.</p>
                  )}
                </>
              ) : (
                <p className="text-xs text-gray-500">
                  No host telemetry yet. Workers send it from this release on; upgrade the worker image if this stays empty.
                </p>
              )}
            </div>
          </Panel>
        ))}
      </div>
    </div>
  );
}

/* ══ /nodes/{id} ══════════════════════════════════════════════════ */
function Row({ k, children }: { k: string; children: React.ReactNode }) {
  return (
    <div className="flex items-start justify-between gap-4 border-b border-line-soft py-2.5 last:border-0">
      <Mono className="shrink-0 text-xs text-gray-500">{k}</Mono>
      <div className="min-w-0 text-right text-[13px] text-gray-200 [overflow-wrap:anywhere]">{children}</div>
    </div>
  );
}

export function NodeDetailView({ id }: { id: string }) {
  const me = usePlatform((s) => s.me);
  const { data, error, reload } = usePoll(() => apiGet<NodeMetrics>(`/v1/nodes/${id}/metrics`), [id]);

  if (error && !data) return <Panel className="p-6 text-sm text-danger">Could not load node {id}: {error}</Panel>;
  if (!data) return <div className="p-6 font-mono text-xs text-gray-500">loading…</div>;

  const n = data.node;
  const t = n.telemetry;
  const h = data.history;
  const memMax = Math.max(64, ...h.map((p) => p.mem_total_mb));
  const diskMax = Math.max(64, ...h.map((p) => p.disk_total_mb));
  const netMax = Math.max(16, ...h.map((p) => Math.max(p.rx_bps, p.tx_bps) / 1024));

  const charts = [
    ["CPU", "% of host cores", h.map((p) => p.cpu_percent), "neon", 100, "%"],
    ["Memory used", "MiB", h.map((p) => p.mem_used_mb), "cyan", memMax, ""],
    ["Disk used", "MiB", h.map((p) => p.disk_used_mb), "amber", diskMax, ""],
    ["Network in", "KiB/s", h.map((p) => p.rx_bps / 1024), "cyan", netMax, ""],
  ] as const;

  return (
    <div className="space-y-5">
      <div className="flex flex-col gap-4 lg:flex-row lg:items-center lg:justify-between">
        <div className="flex min-w-0 items-center gap-3">
          <button
            onClick={() => go("/nodes")}
            className="grid size-9 shrink-0 place-items-center rounded-lg border border-line text-gray-400 hover:bg-fg/5 hover:text-gray-200"
            aria-label="Back to node resources"
          >
            <span className="text-sm">←</span>
          </button>
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-2.5">
              <h1 className="truncate font-mono text-[22px] font-bold tracking-tight text-ink">{n.node_id}</h1>
              <span className="rounded-md border border-line bg-surface px-2.5 py-1.5">
                <NodePill status={n.status} />
              </span>
            </div>
            <Mono className="mt-1 block text-[11px] text-gray-500 [overflow-wrap:anywhere]">
              {n.role} · {t ? `${t.hostname ?? "?"} · ${t.os}/${t.arch}` : "no telemetry"} · last sample {fmtTime(t?.collected_at)}
            </Mono>
          </div>
        </div>
        <button
          onClick={reload}
          className="inline-flex items-center gap-2 self-start rounded-lg border border-line bg-surface px-3.5 py-2 text-xs font-medium text-gray-300 hover:bg-fg/5"
        >
          <RotateCw className="size-3.5" /> Refresh
        </button>
      </div>

      {t ? (
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-4">
          <KpiCard
            icon={<Cpu className="size-4" />}
            label={`CPU · ${t.cpu.cores} cores`}
            vi={`load ${t.cpu.load1.toFixed(2)} / ${t.cpu.load5.toFixed(2)} / ${t.cpu.load15.toFixed(2)}`}
            value={`${t.cpu.usage_percent.toFixed(1)}%`}
            accent="text-neon"
          />
          <KpiCard
            icon={<MemoryStick className="size-4" />}
            label={`Memory of ${mb(t.memory.total_mb)}`}
            vi={`${mb(t.memory.available_mb)} available${t.memory.swap_total_mb ? ` · swap ${mb(t.memory.swap_used_mb ?? 0)}` : ""}`}
            value={mb(t.memory.used_mb)}
            accent="text-info"
          />
          <KpiCard
            icon={<HardDrive className="size-4" />}
            label={`Disk ${t.disk.path ?? ""} of ${mb(t.disk.total_mb)}`}
            vi={`${mb(t.disk.free_mb)} free`}
            value={mb(t.disk.used_mb)}
          />
          <KpiCard
            icon={<Network className="size-4" />}
            label={`Network ${t.network.interfaces?.join(", ") ?? ""}`}
            vi={t.network.host_namespace ? `↑ ${rate(t.network.tx_bps)}` : `↑ ${rate(t.network.tx_bps)} · container ns`}
            value={`↓ ${rate(t.network.rx_bps)}`}
          />
        </div>
      ) : (
        <Panel className="p-6 text-sm text-gray-500">This node has not sent host telemetry yet.</Panel>
      )}

      <div className="grid grid-cols-1 gap-4 xl:grid-cols-2">
        {charts.map(([title, unit, series, tone, yMax, suffix]) => (
          <Panel key={title} className="p-6">
            <SectionTitle title={title} vi={unit} right={<Mono className="text-[10px] text-gray-500">last hour</Mono>} />
            {series.length > 1 ? (
              <AreaChart data={[...series]} tone={tone} height={180} yMax={yMax} yUnit={suffix} showXLabels={false} className="mt-3" />
            ) : (
              <div className="grid h-[180px] place-items-center text-sm text-gray-500">Collecting samples…</div>
            )}
          </Panel>
        ))}
      </div>

      <div className="grid grid-cols-1 gap-4 xl:grid-cols-[1fr_1.4fr]">
        <Panel className="p-6">
          <SectionTitle title="Node" vi="Thông tin node" />
          <div className="mt-3">
            <Row k="role">{n.role}</Row>
            <Row k="drain">{n.drain_state}</Row>
            <Row k="version">
              <Mono>{n.version || "—"}</Mono>
            </Row>
            <Row k="address">
              <Mono>{n.address || "—"}</Mono>
            </Row>
            <Row k="host">{t ? `${t.hostname ?? "?"} · kernel ${t.kernel ?? "?"}` : "—"}</Row>
            <Row k="uptime">{fmtUptime(t?.uptime_sec)}</Row>
            <Row k="runs in">{t ? (t.in_container ? "a container (Docker / k8s pod)" : "the host") : "—"}</Row>
            <Row k="advertised">
              {n.capacity.cpus} CPU · {mb(n.capacity.memory_mb)} · {n.capacity.disk_mb ? mb(n.capacity.disk_mb) : "—"} disk
            </Row>
            <Row k="sandboxes">{n.sandboxes}</Row>
          </div>
        </Panel>
        <ChangeCapacity node={n} isAdmin={me?.role === "admin"} />
      </div>
    </div>
  );
}

/* ── capacity change request (GitOps) ────────────────────────────── */
const field =
  "h-10 w-full rounded-lg border border-line bg-navy/60 px-3 font-mono text-sm text-gray-100 placeholder:text-gray-600 outline-none focus-visible:ring-2 focus-visible:ring-ring";

function ChangeCapacity({ node, isAdmin }: { node: NodeInfo; isAdmin: boolean }) {
  const [list, setList] = useState<NodeChangeList | null>(null);
  const [listErr, setListErr] = useState("");
  const [cpus, setCpus] = useState(String(node.capacity.cpus || ""));
  const [mem, setMem] = useState(String(node.capacity.memory_mb || ""));
  const [disk, setDisk] = useState(String(node.capacity.disk_mb || 0));
  const [reason, setReason] = useState("");
  const [touched, setTouched] = useState(false);
  const [preview, setPreview] = useState<NodeChange | null>(null);
  const [busy, setBusy] = useState(false);
  const t = node.telemetry;

  const loadList = () =>
    apiGet<NodeChangeList>(`/v1/nodes/${node.node_id}/change-requests`)
      .then((d) => {
        setList(d);
        setListErr("");
      })
      .catch((e) => setListErr(e instanceof Error ? e.message : String(e)));
  useEffect(() => {
    if (node.change_file) void loadList();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [node.node_id, node.change_file]);

  // prefill from the values file (what the PR edits), not the live numbers
  const cur = list?.current;
  useEffect(() => {
    if (!cur || touched) return;
    setCpus(String(cur.cpus));
    setMem(String(cur.memory_mb));
    setDisk(String(cur.disk_mb));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [cur?.cpus, cur?.memory_mb, cur?.disk_mb]);

  // any edit invalidates the preview: Open PR needs a fresh one
  const edit = (set: (v: string) => void) => (e: { target: { value: string } }) => {
    set(e.target.value);
    setTouched(true);
    setPreview((p) => (p?.pr ? p : null));
  };

  if (!node.change_file) {
    return (
      <Panel className="p-6">
        <SectionTitle title="Change capacity" vi="Đổi tài nguyên qua pull request" />
        <p className="mt-3 text-xs leading-relaxed text-gray-500">
          No values file is mapped to this node, so capacity changes are not managed here. Map it on the control plane with{" "}
          <Mono className="text-gray-300">SANDBOXD_GITOPS_NODE_FILES={node.node_id}=deploy/…-values.yaml</Mono> (see docs/NODE_RESOURCES.md). A node outside
          Helm (VPS / Docker) takes its capacity from <Mono className="text-gray-300">SANDBOXD_CAPACITY_*</Mono> at start.
        </p>
      </Panel>
    );
  }

  const submit = async (dry: boolean) => {
    setBusy(true);
    try {
      const r = await apiPost<NodeChange>(`/v1/nodes/${node.node_id}/change-requests`, {
        cpus: Number(cpus),
        memory_mb: Number(mem),
        disk_mb: Number(disk),
        reason: reason.trim() || undefined,
        dry_run: dry,
      });
      setPreview(r);
      if (!dry && r.pr) {
        toast.success(`Opened PR #${r.pr.number}`, { description: "Merge it to apply the new capacity." });
        void loadList();
      }
    } catch (e) {
      toast.error(e instanceof ApiError ? `${e.message} (${e.code})` : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Panel className="p-6">
      <SectionTitle title="Change capacity" vi="Đổi tài nguyên qua pull request" />
      <p className="mt-3 text-xs leading-relaxed text-gray-500">
        Edits <Mono className="text-gray-300">capacity:</Mono> in <Mono className="text-gray-300">{node.change_file}</Mono> through a pull request
        {list?.repo && (
          <>
            {" "}on <Mono className="text-gray-300">{list.repo}</Mono>
          </>
        )}
        {cur && ` (now ${cur.cpus} CPU / ${cur.memory_mb} MiB / ${cur.disk_mb} MiB disk)`}. Nothing changes until someone merges it; the deploy workflow
        then runs helm upgrade, and reverting the PR rolls back. This only changes what the node advertises: grow the VM first.
      </p>

      {isAdmin ? (
        <div className="mt-5 space-y-4">
          <div className="grid gap-3 sm:grid-cols-3">
            {(
              [
                ["CPUs", cpus, setCpus, 0.25, t && `host has ${t.cpu.cores} cores`],
                ["Memory (MiB)", mem, setMem, 256, t && `host has ${t.memory.total_mb} MiB`],
                ["Disk (MiB)", disk, setDisk, 1024, t && `host disk ${t.disk.total_mb} MiB`],
              ] as const
            ).map(([label, value, set, step, hint]) => (
              <label key={label} className="space-y-1.5 text-xs text-gray-400">
                {label}
                <input type="number" min={0} step={step} value={value} onChange={edit(set)} className={field} />
                {hint && <VI className="block">{hint}</VI>}
              </label>
            ))}
          </div>
          <label className="block space-y-1.5 text-xs text-gray-400">
            Reason <VI>shown in the PR</VI>
            <input value={reason} maxLength={1000} placeholder="VM grown to 4 vCPU / 8 GiB in Proxmox" onChange={(e) => setReason(e.target.value)} className={field} />
          </label>
          <div className="flex flex-wrap gap-2.5">
            <button
              disabled={busy}
              onClick={() => submit(true)}
              className="rounded-lg border border-line bg-surface px-4 py-2 text-sm text-gray-300 hover:bg-fg/5 disabled:opacity-40"
            >
              Preview
            </button>
            <button
              disabled={busy || !preview || !preview.can_open_pr || !!preview.pr}
              onClick={() => submit(false)}
              className="inline-flex items-center gap-2 rounded-lg bg-neon px-4 py-2 text-sm font-semibold text-navy hover:bg-neon/90 disabled:pointer-events-none disabled:opacity-40"
            >
              <GitPullRequest className="size-4" /> Open pull request
            </button>
          </div>
          {preview && (
            <div className="space-y-2">
              <p className="text-xs text-gray-400">
                {preview.pr ? (
                  <>
                    PR{" "}
                    <a href={preview.pr.url} target="_blank" rel="noreferrer" className="text-info hover:underline">
                      #{preview.pr.number}
                    </a>{" "}
                    opened on <Mono>{preview.pr.branch}</Mono>.
                  </>
                ) : preview.can_open_pr ? (
                  <>
                    Preview of the change to <Mono>{preview.file}</Mono> on {preview.base}:
                  </>
                ) : (
                  <>
                    No GitOps token is configured, so the console cannot open the PR. Commit this change to <Mono>{preview.file}</Mono> yourself:
                  </>
                )}
              </p>
              <pre className="max-h-64 overflow-auto rounded-lg border border-line bg-sunken p-3 font-mono text-[11.5px] leading-relaxed">
                {preview.diff.map((l, i) => (
                  <div key={i} className={l.startsWith("+") ? "text-neon" : l.startsWith("-") ? "text-danger" : "text-gray-400"}>
                    {l || " "}
                  </div>
                ))}
              </pre>
            </div>
          )}
        </div>
      ) : (
        <p className="mt-4 text-xs text-gray-500">Only admins can request a capacity change.</p>
      )}

      <div className="mt-6">
        <SectionTitle title="Recent requests" vi="Yêu cầu gần đây" />
        <div className="mt-3">
          {listErr ? (
            <p className="text-xs text-danger">{listErr}</p>
          ) : !list ? (
            <p className="font-mono text-xs text-gray-500">loading…</p>
          ) : list.error ? (
            <p className="text-xs text-danger">GitHub: {list.error}</p>
          ) : list.requests.length === 0 ? (
            <p className="text-xs text-gray-500">None yet.</p>
          ) : (
            list.requests.map((p) => (
              <div key={p.number} className="flex items-center gap-3 border-b border-line-soft py-2 last:border-0">
                <a href={p.url} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 font-mono text-xs text-info hover:underline">
                  #{p.number} <ExternalLink className="size-3" />
                </a>
                <span className="min-w-0 flex-1 truncate text-xs text-gray-300">{p.title}</span>
                <span
                  className={cn(
                    "rounded-md border px-1.5 py-0.5 font-mono text-[10px]",
                    p.state === "merged"
                      ? "border-neon/30 bg-neon/10 text-neon"
                      : p.state === "open"
                        ? "border-warn/30 bg-warn/10 text-warn"
                        : "border-line text-gray-500",
                  )}
                >
                  {p.state}
                </span>
                <Mono className="hidden text-[10px] text-gray-600 sm:inline">{fmtTime(p.merged_at || p.created_at)}</Mono>
              </div>
            ))
          )}
        </div>
      </div>
    </Panel>
  );
}
