import { useEffect, useMemo, useRef, useState } from "react";
import {
  APPROVAL_HEADER,
  ApiError,
  del,
  get,
  post,
  put,
  type EgressRules,
  type NetActivity,
  type NetEvent,
  type NetHost,
  type NetMode,
  type Policy,
} from "../api";
import { can, Curl, fmtBytes, LinesInput, Link, Loading, Section, useAction, useLoad, useMe, useNotify } from "../ui";

interface PortsResp {
  ports: { container: number; preview_url: string }[];
}
interface EgressResp extends EgressRules {
  proxy: string;
}

const MODES: { mode: NetMode; title: string; text: string }[] = [
  { mode: "none", title: "No network", text: "Nothing leaves the sandbox." },
  { mode: "allowlist", title: "Allow list", text: "Only the hosts on the allow list; everything else is blocked." },
  { mode: "open", title: "Open, monitored", text: "Everything except the block list. Every request is shown below." },
];

// --- host matching, mirroring internal/policy.HostAllowed ---------------------

function ipv4(s: string): number | null {
  const m = s.match(/^(\d+)\.(\d+)\.(\d+)\.(\d+)$/);
  if (!m) return null;
  const b = m.slice(1).map(Number);
  if (b.some((x) => x > 255)) return null;
  return ((b[0] << 24) | (b[1] << 16) | (b[2] << 8) | b[3]) >>> 0;
}

function entryMatches(entry: string, host: string): boolean {
  if (entry.includes("/")) {
    const [net, bits] = entry.split("/");
    const a = ipv4(host);
    const n = ipv4(net);
    if (a === null || n === null) return false;
    const mask = Number(bits) === 0 ? 0 : (0xffffffff << (32 - Number(bits))) >>> 0;
    return (a & mask) === (n & mask);
  }
  if (entry.startsWith("*.")) return host.endsWith(entry.slice(1));
  return host === entry || host.endsWith("." + entry);
}

const listed = (list: string[], host: string) => list.some((e) => entryMatches(e, host));

/** Whether host may go out under rules (the proxy's decision, without DNS). */
export function verdict(rules: EgressRules, host: string): { ok: boolean; why: string } {
  if (listed(rules.deny, host)) return { ok: false, why: "block list" };
  if (rules.mode === "none") return { ok: false, why: "no network" };
  if (rules.mode === "open") return { ok: true, why: "open" };
  return listed(rules.allow, host) ? { ok: true, why: "allow list" } : { ok: false, why: "not on allow list" };
}

const uniq = (xs: string[]) => [...new Set(xs)].sort();
const without = (xs: string[], drop: string[]) => xs.filter((x) => !drop.includes(x));

export function NetworkPage({ id }: { id: string }) {
  const me = useMe();
  const write = can(me, "operator");
  const ports = useLoad(() => get<PortsResp>(`/v1/sandboxes/${id}/network/ports`), [id]);
  const egress = useLoad(() => get<EgressResp>(`/v1/sandboxes/${id}/network/egress`), [id]);
  const policy = useLoad(() => get<Policy>("/v1/policy"));
  const { run, busy } = useAction();
  const notify = useNotify();
  const [newPort, setNewPort] = useState("");
  const [mode, setMode] = useState<NetMode>("allowlist");
  const [allow, setAllow] = useState<string[]>([]);
  const [deny, setDeny] = useState<string[]>([]);
  const [approval, setApproval] = useState("");

  useEffect(() => {
    if (egress.data) {
      setMode(egress.data.mode);
      setAllow(egress.data.allow);
      setDeny(egress.data.deny);
    }
  }, [egress.data]);

  const addPort = async (e: React.FormEvent) => {
    e.preventDefault();
    const n = Number(newPort);
    if (!Number.isInteger(n) || n < 1 || n > 65535) return;
    await run(() => post(`/v1/sandboxes/${id}/network/ports`, { container: n }), `Port ${n} exposed`);
    setNewPort("");
    ports.reload();
  };
  const removePort = async (p: number) => {
    await run(() => del(`/v1/sandboxes/${id}/network/ports/${p}`), `Port ${p} removed`);
    ports.reload();
  };

  /** Saves rules; returns true when they were applied. */
  const applyRules = async (next: EgressRules, ok: string): Promise<boolean> => {
    const h: Record<string, string> = approval ? { [APPROVAL_HEADER]: approval } : {};
    const r = await run(() => put<EgressResp>(`/v1/sandboxes/${id}/network/egress`, next, h), ok);
    egress.reload();
    return !!r && "mode" in r;
  };
  const saveForm = () => applyRules({ mode, allow, deny }, "Network rules saved");

  const pol = policy.data;
  const strict = pol?.egress.mode === "strict";
  const makePermissive = async () => {
    if (!pol) return;
    await run(() => put(`/v1/policy/egress`, { ...pol.egress, mode: "permissive" }), "Egress policy is now permissive");
    policy.reload();
  };

  const current: EgressRules | undefined = egress.data && { mode: egress.data.mode, allow: egress.data.allow, deny: egress.data.deny };

  return (
    <>
      <p>
        <Link to={`/sandboxes/${id}`}>← Sandbox {id}</Link>
      </p>

      <Section title="Network mode">
        {!egress.data ? (
          <Loading error={egress.error} />
        ) : (
          <>
            <div className="modes">
              {MODES.map((m) => (
                <label key={m.mode} className={`mode ${mode === m.mode ? "on" : ""}`}>
                  <input type="radio" name="mode" value={m.mode} checked={mode === m.mode} disabled={!write} onChange={() => setMode(m.mode)} />
                  <b>{m.title}</b>
                  <span className="muted small">{m.text}</span>
                </label>
              ))}
            </div>
            {pol && (
              <p className="small">
                Policy: <b>{pol.egress.mode}</b>
                {strict && ` (allowed hosts: ${pol.egress.allow.length ? pol.egress.allow.join(", ") : "none"})`}
                {pol.approvals.includes("sandbox.egress_outside_policy") && "; anything outside it needs an admin's approval"}
              </p>
            )}
            {strict && mode === "open" && (
              <p className="notice">
                Open mode needs the egress policy set to <b>permissive</b>.{" "}
                {can(me, "admin") ? (
                  <button className="ghost" disabled={busy} onClick={makePermissive}>
                    Switch policy to permissive
                  </button>
                ) : (
                  "Ask an admin to switch it."
                )}
              </p>
            )}
            <div className="cols">
              <label className="stack">
                <b>Allow list</b>
                <LinesInput value={allow} onChange={setAllow} placeholder={"github.com\n*.npmjs.org\n140.82.112.0/20"} rows={6} />
                <small className="muted">Used in allow-list mode. example.com includes its subdomains.</small>
              </label>
              <label className="stack">
                <b>Block list</b>
                <LinesInput value={deny} onChange={setDeny} placeholder={"tracker.example\n203.0.113.7\n10.0.0.0/8"} rows={6} />
                <small className="muted">Always blocked, in every mode. Hosts, IPs or CIDR ranges.</small>
              </label>
            </div>
            <p className="muted small">
              Traffic leaves only through the egress proxy{egress.data.proxy ? ` (${egress.data.proxy})` : ""}; changes apply to the next connection
              without a restart. Cloud metadata, loopback and private networks stay blocked in every mode.
            </p>
            {write && (
              <div className="row wrap">
                <input placeholder="Approval ID (optional)" value={approval} onChange={(e) => setApproval(e.target.value.trim())} />
                <button disabled={busy} onClick={saveForm}>
                  Save network rules
                </button>
              </div>
            )}
            <Curl method="PUT" path={`/v1/sandboxes/${id}/network/egress`} body={{ mode, allow, deny }} />
          </>
        )}
      </Section>

      {current && (
        <ActivityCard
          id={id}
          rules={current}
          write={write}
          busy={busy}
          apply={async (next, msg) => {
            const ok = await applyRules(next, msg);
            if (!ok && next.mode === "open" && strict) notify({ kind: "info", text: "Open mode needs a permissive egress policy." });
          }}
        />
      )}

      <Section title="Preview ports">
        {!ports.data ? (
          <Loading error={ports.error} />
        ) : (
          <table>
            <tbody>
              {ports.data.ports.length === 0 && (
                <tr>
                  <td className="muted">No ports exposed: the sandbox has no preview.</td>
                </tr>
              )}
              {ports.data.ports.map((p) => (
                <tr key={p.container}>
                  <td className="mono">{p.container}</td>
                  <td>
                    <a href={p.preview_url} target="_blank" rel="noreferrer">
                      {p.preview_url}
                    </a>
                  </td>
                  <td className="right">
                    {write && (
                      <button className="ghost danger" disabled={busy} onClick={() => removePort(p.container)}>
                        Remove
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {write && (
          <form className="row" onSubmit={addPort}>
            <input type="number" min="1" max="65535" placeholder="container port" value={newPort} onChange={(e) => setNewPort(e.target.value)} />
            <button disabled={busy || !newPort}>Expose port</button>
          </form>
        )}
        <small className="muted">Changing ports rebuilds the container; the workspace is kept.</small>
        <Curl method="POST" path={`/v1/sandboxes/${id}/network/ports`} body={{ container: Number(newPort) || 8080 }} />
      </Section>
    </>
  );
}

// --- live activity ------------------------------------------------------------

const POLL_MS = 2000;
const MAX_FEED = 300;

function ActivityCard({
  id,
  rules,
  write,
  busy,
  apply,
}: {
  id: string;
  rules: EgressRules;
  write: boolean;
  busy: boolean;
  apply: (next: EgressRules, msg: string) => Promise<void>;
}) {
  const [hosts, setHosts] = useState<NetHost[]>([]);
  const [feed, setFeed] = useState<NetEvent[]>([]);
  const [recording, setRecording] = useState(true);
  const [paused, setPaused] = useState(false);
  const [error, setError] = useState("");
  const [filter, setFilter] = useState("");
  const [only, setOnly] = useState<"all" | "allowed" | "blocked">("all");
  const [sel, setSel] = useState<Set<string>>(new Set());
  const cursor = useRef("");

  useEffect(() => {
    if (paused) return;
    let live = true;
    let timer: ReturnType<typeof setTimeout>;
    const tick = async () => {
      try {
        const q = cursor.current ? `?after=${cursor.current}` : "";
        const a = await get<NetActivity>(`/v1/sandboxes/${id}/network/activity${q}`);
        if (!live) return;
        cursor.current = a.cursor;
        setRecording(a.recording);
        setHosts(a.hosts);
        if (a.events.length) setFeed((f) => [...a.events.slice().reverse(), ...f].slice(0, MAX_FEED));
        setError("");
      } catch (e) {
        if (live) setError(e instanceof ApiError ? e.message : String(e));
      }
      if (live) timer = setTimeout(tick, POLL_MS);
    };
    void tick();
    return () => {
      live = false;
      clearTimeout(timer);
    };
  }, [id, paused]);

  const shown = useMemo(() => {
    const f = filter.trim().toLowerCase();
    return hosts.filter((h) => {
      if (f && !h.host.includes(f) && !h.ips.some((ip) => ip.includes(f))) return false;
      const ok = verdict(rules, h.host).ok;
      return only === "all" || (only === "allowed" ? ok : !ok);
    });
  }, [hosts, filter, only, rules]);

  const toggle = (h: string) =>
    setSel((s) => {
      const n = new Set(s);
      if (n.has(h)) n.delete(h);
      else n.add(h);
      return n;
    });
  const allShown = shown.length > 0 && shown.every((h) => sel.has(h.host));
  const toggleAll = () => setSel(allShown ? new Set() : new Set(shown.map((h) => h.host)));
  const picked = [...sel];

  const done = () => setSel(new Set());
  const block = (items: string[]) =>
    apply({ mode: rules.mode, allow: without(rules.allow, items), deny: uniq([...rules.deny, ...items]) }, `Blocked ${items.join(", ")}`).then(done);
  const allow = (items: string[]) =>
    apply(
      {
        // Allowing something from "no network" means: start an allow list.
        mode: rules.mode === "none" ? "allowlist" : rules.mode,
        allow: rules.mode === "open" ? rules.allow : uniq([...rules.allow, ...items]),
        deny: without(rules.deny, items),
      },
      `Allowed ${items.join(", ")}`,
    ).then(done);
  const allowOnly = (items: string[]) =>
    apply({ mode: "allowlist", allow: uniq(items), deny: without(rules.deny, items) }, `Only ${items.length} host(s) allowed, the rest is blocked`).then(done);

  return (
    <Section
      title="Live network activity"
      actions={
        <>
          <span className={`live ${paused ? "" : "on"}`}>{paused ? "paused" : "live"}</span>
          <button className="ghost" onClick={() => setPaused((p) => !p)}>
            {paused ? "Resume" : "Pause"}
          </button>
          <button
            className="ghost"
            onClick={() => {
              setFeed([]);
            }}
          >
            Clear feed
          </button>
        </>
      }
    >
      {!recording && <p className="notice">The egress proxy is not configured on this node, so no requests are recorded.</p>}
      {error && <p className="error">{error}</p>}
      <p className="muted small">
        Mode <b>{rules.mode}</b>. Every request the sandbox makes through the proxy appears here within {POLL_MS / 1000}s. Pick hosts and
        block or allow them; the status column shows what the current rules do to each host.
      </p>

      <div className="row wrap">
        <input className="grow" placeholder="Filter host or IP" value={filter} onChange={(e) => setFilter(e.target.value)} />
        <select value={only} onChange={(e) => setOnly(e.target.value as typeof only)}>
          <option value="all">All hosts</option>
          <option value="allowed">Allowed now</option>
          <option value="blocked">Blocked now</option>
        </select>
      </div>

      {write && (
        <div className="row wrap toolbar">
          <span className="muted small">{picked.length} selected</span>
          <button className="ghost danger" disabled={busy || !picked.length} onClick={() => block(picked)}>
            Block selected
          </button>
          <button className="ghost" disabled={busy || !picked.length} onClick={() => allow(picked)}>
            Allow selected
          </button>
          <button disabled={busy || !picked.length} onClick={() => allowOnly(picked)}>
            Allow only selected, block the rest
          </button>
          {rules.mode !== "open" && (
            <button className="ghost" disabled={busy} onClick={() => apply({ ...rules, mode: "open" }, "Network is open (monitored)")}>
              Open everything
            </button>
          )}
          {rules.mode !== "none" && (
            <button className="ghost danger" disabled={busy} onClick={() => apply({ ...rules, mode: "none" }, "Network cut off")}>
              Cut off network
            </button>
          )}
        </div>
      )}

      <table className="hosts">
        <thead>
          <tr>
            {write && (
              <th>
                <input type="checkbox" aria-label="Select all" checked={allShown} onChange={toggleAll} />
              </th>
            )}
            <th>Host</th>
            <th>IPs</th>
            <th>Ports</th>
            <th>Requests</th>
            <th>Traffic</th>
            <th>Last seen</th>
            <th>Now</th>
            {write && <th />}
          </tr>
        </thead>
        <tbody>
          {shown.length === 0 && (
            <tr>
              <td colSpan={9} className="muted">
                {hosts.length ? "No host matches the filter." : "No requests yet. Start something in the sandbox that uses the network."}
              </td>
            </tr>
          )}
          {shown.map((h) => {
            const v = verdict(rules, h.host);
            return (
              <tr key={h.host} className={sel.has(h.host) ? "picked" : ""}>
                {write && (
                  <td>
                    <input type="checkbox" aria-label={`Select ${h.host}`} checked={sel.has(h.host)} onChange={() => toggle(h.host)} />
                  </td>
                )}
                <td className="mono">{h.host}</td>
                <td className="mono small">
                  {h.ips.length
                    ? h.ips.map((ip) =>
                        write ? (
                          <button key={ip} className="chip" title={`Block ${ip}`} disabled={busy} onClick={() => block([ip])}>
                            {ip} ✕
                          </button>
                        ) : (
                          <div key={ip}>{ip}</div>
                        ),
                      )
                    : "—"}
                </td>
                <td className="mono small">{h.ports.join(", ")}</td>
                <td className="small">
                  <span className="ok">{h.allowed} ok</span>
                  {h.denied > 0 && <span className="error"> · {h.denied} blocked</span>}
                </td>
                <td className="small">
                  ↑{fmtBytes(h.bytes_up)} ↓{fmtBytes(h.bytes_down)}
                </td>
                <td className="small">{new Date(h.last_seen).toLocaleTimeString()}</td>
                <td>
                  <span className={`badge ${v.ok ? "st-running" : "st-error"}`} title={v.why}>
                    {v.ok ? "allowed" : "blocked"}
                  </span>
                </td>
                {write && (
                  <td className="row right">
                    {v.ok ? (
                      <button className="ghost danger" disabled={busy} onClick={() => block([h.host])}>
                        Block
                      </button>
                    ) : (
                      <button className="ghost" disabled={busy} onClick={() => allow([h.host])}>
                        Allow
                      </button>
                    )}
                  </td>
                )}
              </tr>
            );
          })}
        </tbody>
      </table>

      <h3>Requests</h3>
      <div className="feed">
        {feed.length === 0 ? (
          <p className="muted small">Waiting for requests…</p>
        ) : (
          feed.map((e) => (
            <div key={e.seq} className={`feed-row ${e.allowed ? "" : "denied"}`}>
              <span className="muted">{new Date(e.time).toLocaleTimeString()}</span>
              <span className={e.allowed ? "ok" : "error"}>{e.allowed ? "ALLOW" : "BLOCK"}</span>
              <span>{e.method}</span>
              <span>
                {e.host}:{e.port}
              </span>
              <span className="muted">{e.ip ?? ""}</span>
              <span className="muted">{e.reason ?? ""}</span>
            </div>
          ))
        )}
      </div>
      <Curl method="GET" path={`/v1/sandboxes/${id}/network/activity?after=${cursor.current || 0}`} />
    </Section>
  );
}
