import { useEffect, useRef, useState } from "react";
import { APPROVAL_HEADER, del, get, post, sse, type Sandbox } from "../api";
import { Sparkline } from "../sparkline";
import {
  can,
  Curl,
  fmtBytes,
  fmtTime,
  Link,
  Loading,
  navigate,
  Section,
  StatusBadge,
  useAction,
  useLoad,
  useMe,
} from "../ui";

interface Metrics {
  status: string;
  stats?: { cpu_percent: number; mem_usage_bytes: number; mem_limit_bytes: number; pids: number };
}

export function SandboxDetailPage({ id }: { id: string }) {
  const me = useMe();
  const write = can(me, "operator");
  const { data: sb, error, reload } = useLoad(() => get<Sandbox>(`/v1/sandboxes/${id}`), [id]);
  const { run, busy } = useAction();
  const [approval, setApproval] = useState("");

  if (!sb) return <Loading error={error} />;

  const lifecycle = async (verb: "start" | "stop" | "restart") => {
    await run(() => post(`/v1/sandboxes/${id}/${verb}`), `Sandbox ${verb === "stop" ? "stopped" : verb + "ed"}`);
    reload();
  };
  const keepAlive = async (on: boolean) => {
    await run(() => (on ? post(`/v1/sandboxes/${id}/keep-alive`) : del(`/v1/sandboxes/${id}/keep-alive`)), on ? "Keep-alive on" : "Keep-alive off");
    reload();
  };
  const remove = async () => {
    if (!confirm(`Delete sandbox ${id}? Its workspace is removed too.`)) return;
    const h: Record<string, string> = approval ? { [APPROVAL_HEADER]: approval } : {};
    const r = await run(() => del(`/v1/sandboxes/${id}`, h));
    if (r === undefined) return; // error already shown
    if (typeof r === "object" && r !== null && "approval_id" in r) return; // parked for approval
    navigate("/");
  };

  return (
    <>
      <Section
        title={`Sandbox ${sb.id}`}
        actions={
          <>
            <Link to={`/sandboxes/${id}/network`} className="button ghost">
              Network
            </Link>
            <button className="ghost" onClick={reload}>
              Refresh
            </button>
            {write && sb.status !== "running" && (
              <button disabled={busy} onClick={() => lifecycle("start")}>
                Start
              </button>
            )}
            {write && sb.status === "running" && (
              <>
                <button className="ghost" disabled={busy} onClick={() => lifecycle("restart")}>
                  Restart
                </button>
                <button className="ghost" disabled={busy} onClick={() => lifecycle("stop")}>
                  Stop
                </button>
              </>
            )}
          </>
        }
      >
        <dl className="grid">
          <dt>Status</dt>
          <dd>
            <StatusBadge status={sb.status} /> {sb.error_message && <span className="error">{sb.error_message}</span>}
          </dd>
          <dt>Image</dt>
          <dd className="mono">{sb.image}</dd>
          <dt>Command</dt>
          <dd className="mono">{sb.command?.length ? sb.command.join(" ") : "image default"}</dd>
          <dt>Resources</dt>
          <dd>
            {sb.resources.cpus ? `${sb.resources.cpus} CPU` : "no CPU cap"} · {sb.resources.memory_mb} MiB
          </dd>
          <dt>Preview</dt>
          <dd>
            {sb.preview ? (
              <a href={sb.preview.url} target="_blank" rel="noreferrer">
                {sb.preview.url}
              </a>
            ) : (
              "no ports exposed"
            )}{" "}
            <span className="muted">({sb.visibility})</span>
          </dd>
          <dt>Network</dt>
          <dd>
            <Link to={`/sandboxes/${id}/network`}>{sb.egress_mode ?? "allowlist"}</Link>
            <span className="muted">
              {" "}
              {sb.egress_mode === "open"
                ? `(everything except ${sb.egress_deny?.length ?? 0} blocked)`
                : sb.egress_mode === "none"
                  ? "(no outbound traffic)"
                  : `(${sb.egress.length} allowed host${sb.egress.length === 1 ? "" : "s"})`}
            </span>
          </dd>
          <dt>Env</dt>
          <dd className="mono small">
            {sb.env && Object.keys(sb.env).length ? Object.entries(sb.env).map(([k, v]) => <div key={k}>{`${k}=${v}`}</div>) : "—"}
          </dd>
          <dt>Created</dt>
          <dd>{fmtTime(sb.created_at)}</dd>
          <dt>Last active</dt>
          <dd>{fmtTime(sb.last_active_at)}</dd>
          <dt>Keep-alive</dt>
          <dd className="row">
            {sb.lifecycle.keep_alive ? "on" : "off"}
            <span className="muted">
              (idle timeout {sb.lifecycle.idle_timeout_secs ? `${sb.lifecycle.idle_timeout_secs}s` : "instance default"})
            </span>
            {write && (
              <button className="ghost" disabled={busy} onClick={() => keepAlive(!sb.lifecycle.keep_alive)}>
                Turn {sb.lifecycle.keep_alive ? "off" : "on"}
              </button>
            )}
          </dd>
        </dl>
        <Curl method="GET" path={`/v1/sandboxes/${id}`} />
      </Section>

      {sb.status === "running" && <MetricsCard id={id} />}
      {sb.status === "running" && write && <ExecCard id={id} />}
      <LogsCard id={id} running={sb.status === "running"} />

      {write && (
        <Section title="Danger zone">
          <div className="row">
            <input placeholder="Approval ID (optional)" value={approval} onChange={(e) => setApproval(e.target.value.trim())} />
            <button className="danger" disabled={busy} onClick={remove}>
              Delete sandbox
            </button>
          </div>
          <Curl method="DELETE" path={`/v1/sandboxes/${id}`} />
        </Section>
      )}
    </>
  );
}

function MetricsCard({ id }: { id: string }) {
  const [m, setM] = useState<Metrics | null>(null);
  /* rolling history for the gradient sparklines (≈2 min at a 2s interval) */
  const [hist, setHist] = useState<{ cpu: number[]; mem: number[] }>({ cpu: [], mem: [] });
  useEffect(() => {
    return sse(`/v1/sandboxes/${id}/metrics/stream?interval=2`, (line) => {
      try {
        const frame: Metrics = JSON.parse(line);
        setM(frame);
        const s = frame.stats;
        if (s && s.mem_limit_bytes > 0) {
          setHist((h) => ({
            cpu: [...h.cpu, s.cpu_percent].slice(-60),
            mem: [...h.mem, (s.mem_usage_bytes / s.mem_limit_bytes) * 100].slice(-60),
          }));
        }
      } catch {
        /* ignore partial frames */
      }
    });
  }, [id]);
  const s = m?.stats;
  return (
    <Section title="Metrics">
      {!s ? (
        <p className="muted">Waiting for the first sample…</p>
      ) : (
        <>
          <div className="stats">
            <div>
              <b>{s.cpu_percent.toFixed(1)}%</b>
              <span>CPU</span>
            </div>
            <div>
              <b>{fmtBytes(s.mem_usage_bytes)}</b>
              <span>of {fmtBytes(s.mem_limit_bytes)}</span>
            </div>
            <div>
              <b>{s.pids}</b>
              <span>processes</span>
            </div>
          </div>
          {(hist.cpu.length > 1 || hist.mem.length > 1) && (
            <div className="spark-row">
              <figure className="spark-cpu">
                <figcaption>
                  <span>CPU %</span>
                  <b>last {hist.cpu.length} samples</b>
                </figcaption>
                <Sparkline data={hist.cpu} />
              </figure>
              <figure className="spark-mem">
                <figcaption>
                  <span>Memory %</span>
                  <b>last {hist.mem.length} samples</b>
                </figcaption>
                <Sparkline data={hist.mem} />
              </figure>
            </div>
          )}
        </>
      )}
    </Section>
  );
}

function ExecCard({ id }: { id: string }) {
  const [cmd, setCmd] = useState("");
  const [out, setOut] = useState<{ stdout: string; stderr: string; exit_code: number } | null>(null);
  const { run, busy } = useAction();
  const exec = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!cmd.trim()) return;
    const r = await run(() => post<{ stdout: string; stderr: string; exit_code: number }>(`/v1/sandboxes/${id}/exec`, { cmd: ["sh", "-c", cmd] }));
    if (r) setOut(r);
  };
  return (
    <Section title="Exec">
      <form className="row" onSubmit={exec}>
        <input className="mono grow" placeholder="ls -la /home/sandbox" value={cmd} onChange={(e) => setCmd(e.target.value)} />
        <button disabled={busy}>Run</button>
      </form>
      {out && (
        <pre className="term">
          {out.stdout}
          {out.stderr && <span className="error">{out.stderr}</span>}
          <span className="muted">{`\n[exit ${out.exit_code}]`}</span>
        </pre>
      )}
      <Curl method="POST" path={`/v1/sandboxes/${id}/exec`} body={{ cmd: ["sh", "-c", cmd || "..."] }} />
    </Section>
  );
}

function LogsCard({ id, running }: { id: string; running: boolean }) {
  const [lines, setLines] = useState<string[]>([]);
  const [follow, setFollow] = useState(false);
  const box = useRef<HTMLPreElement>(null);
  useEffect(() => {
    setLines([]);
    if (follow && running) {
      // SSE: one `data:` frame per line until the container stops.
      return sse(`/v1/sandboxes/${id}/logs?tail=200&follow=true`, (l) =>
        setLines((xs) => (xs.length > 2000 ? [...xs.slice(-1500), l] : [...xs, l])),
      );
    }
    let live = true;
    fetch(`/v1/sandboxes/${id}/logs?tail=200`, { credentials: "same-origin" })
      .then((r) => (r.ok ? r.text() : ""))
      .then((t) => live && setLines(t ? t.replace(/\n$/, "").split("\n") : []));
    return () => {
      live = false;
    };
  }, [id, follow, running]);
  useEffect(() => {
    if (box.current) box.current.scrollTop = box.current.scrollHeight;
  }, [lines]);
  return (
    <Section
      title="Logs"
      actions={
        <label className="row small">
          <input type="checkbox" checked={follow} disabled={!running} onChange={(e) => setFollow(e.target.checked)} /> Follow
        </label>
      }
    >
      <pre className="term" ref={box}>
        {lines.length ? lines.join("\n") : <span className="muted">No output.</span>}
      </pre>
    </Section>
  );
}
