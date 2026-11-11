import { useEffect, useRef, useState } from "react";
import {
  ApiError,
  get,
  post,
  type HostTelemetry,
  type NodeChange,
  type NodeChangeList,
  type NodeInfo,
  type NodeMetrics,
} from "../api";
import { can, Curl, fmtBytes, fmtTime, Link, Loading, Section, StatusBadge, useLoad, useMe, useNotify } from "../ui";
import { Area } from "./Monitoring";

// Node Resources: what each fleet member's machine really has and uses.
// The numbers come from the worker image itself (procfs/statfs, sent with
// every heartbeat), so they work on a VPS, a Docker host or a k3s node.
// Admins can propose a new advertised capacity; that opens a pull request
// on the deploy repo instead of touching the node (GitOps).

const MiB = 1024 * 1024;
const mb = (n: number) => fmtBytes(n * MiB);

function pct(used: number, total: number) {
  return total > 0 ? Math.min(100, (used / total) * 100) : 0;
}

function fmtRate(bps: number) {
  return `${fmtBytes(bps)}/s`;
}

function fmtUptime(sec?: number) {
  if (!sec) return "—";
  const d = Math.floor(sec / 86400);
  const h = Math.floor((sec % 86400) / 3600);
  const m = Math.floor((sec % 3600) / 60);
  return d ? `${d}d ${h}h` : h ? `${h}h ${m}m` : `${m}m`;
}

function Meter({ label, value, detail }: { label: string; value: number; detail: string }) {
  const level = value >= 90 ? "crit" : value >= 75 ? "warn" : "";
  return (
    <div className="meter-row">
      <div className="row small" style={{ justifyContent: "space-between" }}>
        <span>{label}</span>
        <span className="muted">{detail}</span>
      </div>
      <div className={`meter ${level}`} role="meter" aria-valuenow={Math.round(value)} aria-valuemin={0} aria-valuemax={100} aria-label={label}>
        <span style={{ width: `${value}%` }} />
      </div>
    </div>
  );
}

function stale(n: NodeInfo) {
  if (!n.telemetry) return true;
  return Date.now() - new Date(n.telemetry.collected_at).getTime() > 2 * 60_000;
}

function Telemetry({ t }: { t: HostTelemetry }) {
  return (
    <>
      <Meter
        label="CPU"
        value={t.cpu.usage_percent}
        detail={`${t.cpu.usage_percent.toFixed(1)}% of ${t.cpu.cores} cores · load ${t.cpu.load1.toFixed(2)}`}
      />
      <Meter
        label="Memory"
        value={pct(t.memory.used_mb, t.memory.total_mb)}
        detail={`${mb(t.memory.used_mb)} / ${mb(t.memory.total_mb)}`}
      />
      <Meter
        label={`Disk${t.disk.path ? ` (${t.disk.path})` : ""}`}
        value={pct(t.disk.used_mb, t.disk.total_mb)}
        detail={`${mb(t.disk.used_mb)} / ${mb(t.disk.total_mb)}`}
      />
      <div className="row small" style={{ justifyContent: "space-between" }}>
        <span>Network</span>
        <span className="muted">
          ↓ {fmtRate(t.network.rx_bps)} · ↑ {fmtRate(t.network.tx_bps)}
          {!t.network.host_namespace && " (container namespace)"}
        </span>
      </div>
    </>
  );
}

// --- /nodes ----------------------------------------------------------------

export function NodesPage() {
  const { data, error, reload } = useLoad(() => get<{ nodes: NodeInfo[] }>("/v1/nodes"));
  const reloadRef = useRef(reload);
  reloadRef.current = reload;
  useEffect(() => {
    const t = setInterval(() => reloadRef.current(), 10_000);
    return () => clearInterval(t);
  }, []);

  if (!data) return <Loading error={error} />;
  const withTel = data.nodes.filter((n) => n.telemetry);
  const sum = (f: (t: HostTelemetry) => number) => withTel.reduce((a, n) => a + f(n.telemetry!), 0);
  return (
    <>
      <div className="row wrap" style={{ justifyContent: "space-between" }}>
        <h2 style={{ margin: 0 }}>Node resources</h2>
        <button className="ghost" onClick={reload}>
          Refresh
        </button>
      </div>
      <div className="kpis">
        <div className="kpi">
          <b>
            {data.nodes.filter((n) => n.status === "online").length} / {data.nodes.length}
          </b>
          <span>nodes online</span>
        </div>
        <div className="kpi">
          <b>{sum((t) => t.cpu.cores)}</b>
          <span>host cores (reporting nodes)</span>
        </div>
        <div className="kpi">
          <b>
            {mb(sum((t) => t.memory.used_mb))} / {mb(sum((t) => t.memory.total_mb))}
          </b>
          <span>host memory used</span>
        </div>
        <div className="kpi">
          <b>
            {mb(sum((t) => t.disk.used_mb))} / {mb(sum((t) => t.disk.total_mb))}
          </b>
          <span>host disk used</span>
        </div>
      </div>
      {data.nodes.length === 0 && <p className="muted">No nodes registered yet.</p>}
      <div className="cols-2">
        {data.nodes.map((n) => (
          <Section
            key={n.node_id}
            title={n.node_id}
            actions={
              <>
                <StatusBadge status={n.status} />
                {n.drain_state === "drain" && <span className="badge st-error">draining</span>}
                <Link to={`/nodes/${n.node_id}`} className="button ghost">
                  Details
                </Link>
              </>
            }
          >
            <p className="small muted" style={{ marginTop: 0 }}>
              {n.role}
              {n.telemetry?.hostname && <> · host {n.telemetry.hostname}</>}
              {n.telemetry?.in_container && <> · in container</>} · advertises {n.capacity.cpus} CPU / {mb(n.capacity.memory_mb)}
              {n.capacity.disk_mb > 0 && <> / {mb(n.capacity.disk_mb)} disk</>} · {n.sandboxes} sandbox(es)
            </p>
            {n.telemetry ? (
              <>
                <Telemetry t={n.telemetry} />
                {stale(n) && <p className="small error">Last sample {fmtTime(n.telemetry.collected_at)}: the node stopped reporting.</p>}
              </>
            ) : (
              <p className="muted small">
                No host telemetry yet. Workers send it from this release on; upgrade the worker image if this stays empty.
              </p>
            )}
          </Section>
        ))}
      </div>
      <Curl method="GET" path="/v1/nodes" />
    </>
  );
}

// --- /nodes/{id} -------------------------------------------------------------

export function NodeDetailPage({ id }: { id: string }) {
  const me = useMe();
  const { data, error, reload } = useLoad(() => get<NodeMetrics>(`/v1/nodes/${id}/metrics`), [id]);
  const reloadRef = useRef(reload);
  reloadRef.current = reload;
  useEffect(() => {
    const t = setInterval(() => reloadRef.current(), 10_000);
    return () => clearInterval(t);
  }, []);

  if (!data) return <Loading error={error} />;
  const n = data.node;
  const t = n.telemetry;
  const h = data.history;
  const memMax = Math.max(64, ...h.map((p) => p.mem_total_mb));
  const diskMax = Math.max(64, ...h.map((p) => p.disk_total_mb));
  const netMax = Math.max(1024, ...h.map((p) => Math.max(p.rx_bps, p.tx_bps) / 1024));

  return (
    <>
      <p className="small">
        <Link to="/nodes">← Node resources</Link>
      </p>
      <div className="row wrap" style={{ justifyContent: "space-between" }}>
        <h2 style={{ margin: 0 }}>
          {n.node_id} <StatusBadge status={n.status} />
        </h2>
        <button className="ghost" onClick={reload}>
          Refresh
        </button>
      </div>

      {t ? (
        <div className="kpis">
          <div className="kpi">
            <b>{t.cpu.usage_percent.toFixed(1)}%</b>
            <span>
              CPU of {t.cpu.cores} cores · load {t.cpu.load1.toFixed(2)} / {t.cpu.load5.toFixed(2)} / {t.cpu.load15.toFixed(2)}
            </span>
          </div>
          <div className="kpi">
            <b>
              {mb(t.memory.used_mb)} / {mb(t.memory.total_mb)}
            </b>
            <span>
              memory · {mb(t.memory.available_mb)} available
              {t.memory.swap_total_mb ? ` · swap ${mb(t.memory.swap_used_mb ?? 0)} / ${mb(t.memory.swap_total_mb)}` : ""}
            </span>
          </div>
          <div className="kpi">
            <b>
              {mb(t.disk.used_mb)} / {mb(t.disk.total_mb)}
            </b>
            <span>disk {t.disk.path} · {mb(t.disk.free_mb)} free</span>
          </div>
          <div className="kpi">
            <b>
              ↓ {fmtRate(t.network.rx_bps)} ↑ {fmtRate(t.network.tx_bps)}
            </b>
            <span>
              network {t.network.interfaces?.join(", ")}
              {!t.network.host_namespace && " (container namespace)"}
            </span>
          </div>
        </div>
      ) : (
        <p className="muted">This node has not sent host telemetry yet.</p>
      )}

      <div className="cols-2">
        <Section title="CPU % (last hour)">
          <Area points={h.map((p) => p.cpu_percent)} max={100} unit="%" />
        </Section>
        <Section title="Memory used (MiB)">
          <Area points={h.map((p) => p.mem_used_mb)} max={memMax} unit=" MiB" />
        </Section>
        <Section title="Disk used (MiB)">
          <Area points={h.map((p) => p.disk_used_mb)} max={diskMax} unit=" MiB" />
        </Section>
        <Section title="Network in (KiB/s)">
          <Area points={h.map((p) => p.rx_bps / 1024)} max={netMax} unit=" KiB/s" />
        </Section>
      </div>

      <div className="cols-2">
        <Section title="Node">
          <table>
            <tbody>
              <tr><th>Role</th><td>{n.role}</td></tr>
              <tr><th>Drain</th><td>{n.drain_state}</td></tr>
              <tr><th>Version</th><td className="mono small">{n.version || "—"}</td></tr>
              <tr><th>Address</th><td className="mono small">{n.address || "—"}</td></tr>
              <tr><th>Host</th><td>{t ? `${t.hostname ?? "?"} · ${t.os}/${t.arch} · kernel ${t.kernel ?? "?"}` : "—"}</td></tr>
              <tr><th>Uptime</th><td>{fmtUptime(t?.uptime_sec)}</td></tr>
              <tr><th>Runs in</th><td>{t ? (t.in_container ? "a container (Docker / k8s pod)" : "the host") : "—"}</td></tr>
              <tr>
                <th>Advertised</th>
                <td>
                  {n.capacity.cpus} CPU · {mb(n.capacity.memory_mb)} · {n.capacity.disk_mb ? mb(n.capacity.disk_mb) : "—"} disk
                </td>
              </tr>
              <tr><th>Sandboxes</th><td>{n.sandboxes}</td></tr>
              <tr><th>Last sample</th><td>{fmtTime(t?.collected_at)}</td></tr>
            </tbody>
          </table>
        </Section>
        <ChangeRequests node={n} isAdmin={can(me, "admin")} />
      </div>
      <Curl method="GET" path={`/v1/nodes/${id}/metrics`} />
    </>
  );
}

// --- capacity change request (GitOps) -----------------------------------------

function ChangeRequests({ node, isAdmin }: { node: NodeInfo; isAdmin: boolean }) {
  const notify = useNotify();
  const list = useLoad(() => get<NodeChangeList>(`/v1/nodes/${node.node_id}/change-requests`), [node.node_id]);
  const [cpus, setCpus] = useState(String(node.capacity.cpus || ""));
  const [mem, setMem] = useState(String(node.capacity.memory_mb || ""));
  const [disk, setDisk] = useState(String(node.capacity.disk_mb || 0));
  // Prefill from the values file once it is read (what the PR will edit),
  // not from the live advertised numbers.
  const cur = list.data?.current;
  const [touched, setTouched] = useState(false);
  useEffect(() => {
    if (!cur || touched) return;
    setCpus(String(cur.cpus));
    setMem(String(cur.memory_mb));
    setDisk(String(cur.disk_mb));
  }, [cur?.cpus, cur?.memory_mb, cur?.disk_mb]); // eslint-disable-line react-hooks/exhaustive-deps
  // Any edit invalidates the preview: the PR button needs a fresh one.
  const edit = (set: (v: string) => void) => (e: { target: { value: string } }) => {
    set(e.target.value);
    setTouched(true);
    setPreview((p) => (p?.pr ? p : null));
  };
  const [reason, setReason] = useState("");
  const [preview, setPreview] = useState<NodeChange | null>(null);
  const [busy, setBusy] = useState(false);
  const t = node.telemetry;

  if (!node.change_file) {
    return (
      <Section title="Change capacity">
        <p className="muted small">
          No values file is mapped to this node, so capacity changes are not managed here. Map it on the control plane with{" "}
          <code>SANDBOXD_GITOPS_NODE_FILES={node.node_id}=deploy/…-values.yaml</code> (see docs/NODE_RESOURCES.md). A node outside
          Helm (VPS / Docker) takes its capacity from <code>SANDBOXD_CAPACITY_*</code> at start.
        </p>
      </Section>
    );
  }

  const body = (dry: boolean) => ({
    cpus: Number(cpus),
    memory_mb: Number(mem),
    disk_mb: Number(disk),
    reason: reason.trim() || undefined,
    dry_run: dry,
  });

  const submit = async (dry: boolean) => {
    setBusy(true);
    try {
      const r = await post<NodeChange>(`/v1/nodes/${node.node_id}/change-requests`, body(dry));
      setPreview(r);
      if (!dry && r.pr) {
        notify({ kind: "ok", text: `Opened PR #${r.pr.number}. Merge it to apply.` });
        list.reload();
      }
    } catch (e) {
      notify({ kind: "err", text: e instanceof ApiError ? `${e.message} (${e.code})` : String(e) });
    } finally {
      setBusy(false);
    }
  };

  return (
    <Section title="Change capacity">
      <p className="small muted" style={{ marginTop: 0 }}>
        Edits <code>capacity:</code> in <code>{node.change_file}</code> through a pull request
        {list.data?.repo && (
          <>
            {" "}on <code>{list.data.repo}</code>
          </>
        )}
        {cur && (
          <>
            {" "}(now {cur.cpus} CPU / {cur.memory_mb} MiB / {cur.disk_mb} MiB disk)
          </>
        )}
        . Nothing changes until someone merges it; the deploy workflow then runs helm upgrade, and reverting the PR rolls back.
        This only changes what the node advertises to the scheduler: grow the VM (vCPU / RAM / disk) first.
      </p>
      {isAdmin ? (
        <div className="form">
          <div className="cols">
            <label>
              CPUs
              <input type="number" min={0.25} step={0.25} value={cpus} onChange={edit(setCpus)} />
              {t && <small>host has {t.cpu.cores} cores</small>}
            </label>
            <label>
              Memory (MiB)
              <input type="number" min={256} step={256} value={mem} onChange={edit(setMem)} />
              {t && <small>host has {t.memory.total_mb} MiB</small>}
            </label>
            <label>
              Disk (MiB)
              <input type="number" min={0} step={1024} value={disk} onChange={edit(setDisk)} />
              {t && <small>host disk {t.disk.total_mb} MiB</small>}
            </label>
          </div>
          <label>
            Reason <small>shown in the PR</small>
            <input value={reason} maxLength={1000} placeholder="VM grown to 4 vCPU / 8 GiB in Proxmox" onChange={(e) => setReason(e.target.value)} />
          </label>
          <div className="row">
            <button className="ghost" disabled={busy} onClick={() => submit(true)}>
              Preview
            </button>
            <button disabled={busy || !preview || !preview.can_open_pr} onClick={() => submit(false)}>
              Open pull request
            </button>
          </div>
          {preview && (
            <div>
              <p className="small">
                {preview.pr ? (
                  <>
                    PR{" "}
                    <a href={preview.pr.url} target="_blank" rel="noreferrer">
                      #{preview.pr.number}
                    </a>{" "}
                    opened on <code>{preview.pr.branch}</code>.
                  </>
                ) : preview.can_open_pr ? (
                  <>Preview of the change to <code>{preview.file}</code> on {preview.base}:</>
                ) : (
                  <>
                    No GitOps token is configured, so the console cannot open the PR. Commit this change to{" "}
                    <code>{preview.file}</code> yourself:
                  </>
                )}
              </p>
              <pre className="term small">{preview.diff.join("\n")}</pre>
            </div>
          )}
        </div>
      ) : (
        <p className="muted small">Only admins can request a capacity change.</p>
      )}
      <h3 className="small" style={{ marginBottom: 4 }}>
        Recent requests
      </h3>
      {!list.data ? (
        <Loading error={list.error} />
      ) : list.data.error ? (
        <p className="error small">GitHub: {list.data.error}</p>
      ) : list.data.requests.length === 0 ? (
        <p className="muted small">None yet.</p>
      ) : (
        <table>
          <tbody>
            {list.data.requests.map((p) => (
              <tr key={p.number}>
                <td>
                  <a href={p.url} target="_blank" rel="noreferrer">
                    #{p.number}
                  </a>
                </td>
                <td className="small">{p.title}</td>
                <td>
                  <span className={`badge ${p.state === "merged" ? "st-running" : p.state === "open" ? "st-pending" : "st-error"}`}>
                    {p.state}
                  </span>
                </td>
                <td className="small muted">{fmtTime(p.merged_at || p.created_at)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <Curl method="POST" path={`/v1/nodes/${node.node_id}/change-requests`} body={body(true)} />
    </Section>
  );
}
