import { useEffect, useState } from "react";
import {
  APPROVAL_HEADER,
  del,
  get,
  patch,
  post,
  put,
  type ApiKey,
  type Approval,
  type AuditEntry,
  type DomainInfo,
  type NodeInfo,
  type Policy,
  type Role,
  type User,
} from "../api";
import { can, Curl, fmtTime, fmtBytes, LinesInput, Link, Loading, Section, StatusBadge, useAction, useLoad, useMe } from "../ui";

const TABS: { id: string; label: string; admin: boolean }[] = [
  { id: "policy", label: "Policy", admin: false },
  { id: "approvals", label: "Approvals", admin: false },
  { id: "nodes", label: "Nodes", admin: false },
  { id: "users", label: "Users", admin: true },
  { id: "keys", label: "API keys", admin: true },
  { id: "audit", label: "Audit log", admin: true },
  { id: "domain", label: "Domain", admin: true },
];

export function AdminPage({ tab }: { tab: string }) {
  const me = useMe();
  const isAdmin = can(me, "admin");
  const tabs = TABS.filter((t) => isAdmin || !t.admin);
  const cur = tabs.find((t) => t.id === tab) ?? tabs[0];
  return (
    <>
      <nav className="tabs">
        {tabs.map((t) => (
          <Link key={t.id} to={`/admin/${t.id}`} className={t.id === cur.id ? "active" : ""}>
            {t.label}
          </Link>
        ))}
      </nav>
      {cur.id === "policy" && <PolicyTab editable={isAdmin} />}
      {cur.id === "approvals" && <ApprovalsTab canDecide={isAdmin && me?.kind === "user"} />}
      {cur.id === "nodes" && <NodesTab isAdmin={isAdmin} />}
      {cur.id === "users" && <UsersTab />}
      {cur.id === "keys" && <KeysTab />}
      {cur.id === "audit" && <AuditTab />}
      {cur.id === "domain" && <DomainTab />}
    </>
  );
}

// --- policy ----------------------------------------------------------------

const APPROVAL_ACTIONS: [string, string][] = [
  ["sandbox.delete_keep_alive", "Deleting a keep-alive sandbox"],
  ["policy.update", "Changing this policy"],
  ["sandbox.egress_outside_policy", "Egress to hosts outside the policy allowlist"],
  ["sandbox.public", "Creating a sandbox with a public preview"],
];

function PolicyTab({ editable }: { editable: boolean }) {
  const { data, error, reload } = useLoad(() => get<Policy>("/v1/policy"));
  const [p, setP] = useState<Policy | null>(null);
  const [approval, setApproval] = useState("");
  const { run, busy } = useAction();
  useEffect(() => {
    if (data) setP(structuredClone(data));
  }, [data]);
  if (!p) return <Loading error={error} />;

  const set = (fn: (q: Policy) => void) =>
    setP((q) => {
      const n = structuredClone(q!);
      fn(n);
      return n;
    });
  const save = async () => {
    const h: Record<string, string> = approval ? { [APPROVAL_HEADER]: approval } : {};
    await run(() => put("/v1/policy", p, h), "Policy saved");
    reload();
  };
  const ro = !editable;

  return (
    <Section title="Instance policy">
      <p className="muted">Every sandbox create, port, egress and lifecycle change is checked against this policy.</p>
      <fieldset disabled={ro} className="form">
        <label>
          Allowed images (besides the instance image)
          <LinesInput value={p.images.allowed} onChange={(v) => set((q) => (q.images.allowed = v))} placeholder="alpine:3.20" rows={3} />
        </label>
        <label>
          Port ranges
          <LinesInput
            value={p.ports.ranges.map((r) => `${r.min}-${r.max}`)}
            onChange={(v) =>
              set(
                (q) =>
                  (q.ports.ranges = v.map((s) => {
                    const [a, b] = s.split("-").map((x) => Number(x.trim()));
                    return { min: a, max: b ?? a };
                  })),
              )
            }
            placeholder="1024-65535"
            rows={2}
          />
        </label>
        <div className="cols">
          <label>
            Max CPUs (0 = no cap)
            <input type="number" step="0.1" min="0" value={p.resources.max_cpus} onChange={(e) => set((q) => (q.resources.max_cpus = Number(e.target.value)))} />
          </label>
          <label>
            Max memory (MiB, 0 = no cap)
            <input type="number" min="0" value={p.resources.max_memory_mb} onChange={(e) => set((q) => (q.resources.max_memory_mb = Number(e.target.value)))} />
          </label>
          <label>
            Max ports per sandbox
            <input type="number" min="1" value={p.resources.max_ports_per_sandbox} onChange={(e) => set((q) => (q.resources.max_ports_per_sandbox = Number(e.target.value)))} />
          </label>
        </div>
        <div className="cols">
          <label>
            Egress mode
            <select value={p.egress.mode} onChange={(e) => set((q) => (q.egress.mode = e.target.value as Policy["egress"]["mode"]))}>
              <option value="strict">strict — sandboxes may only allow hosts listed here</option>
              <option value="permissive">permissive — sandboxes may allow any host</option>
            </select>
          </label>
          <label>
            Egress allowlist
            <LinesInput value={p.egress.allow} onChange={(v) => set((q) => (q.egress.allow = v))} placeholder={"github.com\n*.npmjs.org"} rows={3} />
          </label>
        </div>
        <div className="cols">
          <label className="check">
            <input type="checkbox" checked={p.lifecycle.allow_keep_alive} onChange={(e) => set((q) => (q.lifecycle.allow_keep_alive = e.target.checked))} />
            Allow keep-alive sandboxes
          </label>
          <label>
            Max idle timeout (seconds, 0 = no cap)
            <input type="number" min="0" value={p.lifecycle.max_idle_timeout_secs} onChange={(e) => set((q) => (q.lifecycle.max_idle_timeout_secs = Number(e.target.value)))} />
          </label>
        </div>
        <div>
          <b>Require an admin's approval for</b>
          {APPROVAL_ACTIONS.map(([id, label]) => (
            <label key={id} className="check">
              <input
                type="checkbox"
                checked={p.approvals.includes(id)}
                onChange={(e) => set((q) => (q.approvals = e.target.checked ? [...q.approvals, id] : q.approvals.filter((x) => x !== id)))}
              />
              {label}
            </label>
          ))}
        </div>
      </fieldset>
      {editable && (
        <div className="row">
          <input placeholder="Approval ID (optional)" value={approval} onChange={(e) => setApproval(e.target.value.trim())} />
          <button disabled={busy} onClick={save}>
            Save policy
          </button>
          <button className="ghost" onClick={() => setP(structuredClone(data!))}>
            Reset
          </button>
        </div>
      )}
      <Curl method="PUT" path="/v1/policy" body={p} />
    </Section>
  );
}

// --- nodes (fleet, M6) ------------------------------------------------------

function NodesTab({ isAdmin }: { isAdmin: boolean }) {
  const { data, error, reload } = useLoad(() => get<{ nodes: NodeInfo[] }>("/v1/nodes"));
  const { run, busy } = useAction();

  // The fleet is a live view: workers appear and go offline as they
  // heartbeat, so refresh on a short interval.
  useEffect(() => {
    const t = setInterval(reload, 10_000);
    return () => clearInterval(t);
  }, [reload]);

  const drain = async (n: NodeInfo, to: "drain" | "undrain") => {
    if (to === "drain" && !confirm(`Drain node ${n.node_id}? New sandboxes are refused on a draining node.`)) return;
    await run(() => post(`/v1/nodes/${n.node_id}/${to}`), to === "drain" ? `Drained ${n.node_id}` : `Undrained ${n.node_id}`);
    reload();
  };

  return (
    <Section title="Nodes">
      <p className="muted">
        The control plane itself is the <code>local</code> node; workers register with the worker token and stay online
        by heartbeating. A draining node refuses new sandboxes; its running sandboxes are untouched.
      </p>
      {!data ? (
        <Loading error={error} />
      ) : data.nodes.length === 0 ? (
        <p className="muted">No nodes registered yet.</p>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Node</th>
              <th>Role</th>
              <th>Status</th>
              <th>Drain</th>
              <th>Capacity</th>
              <th>Used</th>
              <th>Sandboxes</th>
              <th>Heartbeat</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {data.nodes.map((n) => (
              <tr key={n.node_id}>
                <td>
                  <b>{n.node_id}</b>
                  {n.version && <div className="small muted mono">{n.version}</div>}
                  {n.address && <div className="small muted mono">{n.address}</div>}
                </td>
                <td>{n.role}</td>
                <td>
                  <StatusBadge status={n.status} />
                </td>
                <td>
                  {n.drain_state === "drain" ? (
                    <span className="badge st-error">draining</span>
                  ) : (
                    <span className="badge st-running">active</span>
                  )}
                </td>
                <td className="small">
                  {n.capacity.cpus} CPUs · {fmtBytes(n.capacity.memory_mb * 1024 * 1024)}
                  {n.capacity.disk_mb > 0 && <> · {fmtBytes(n.capacity.disk_mb * 1024 * 1024)} disk</>}
                </td>
                <td className="small">
                  {n.used.cpus > 0 && <>{n.used.cpus} CPUs · </>}
                  {n.used.memory_mb > 0 && <>{fmtBytes(n.used.memory_mb * 1024 * 1024)}</>}
                  {!n.used.cpus && !n.used.memory_mb && <span className="muted">—</span>}
                </td>
                <td>{n.sandboxes}</td>
                <td className="small" title={n.heartbeat_at}>
                  {n.role === "local" ? "self" : fmtTime(n.heartbeat_at)}
                </td>
                <td className="row right">
                  {isAdmin &&
                    (n.drain_state === "drain" ? (
                      <button disabled={busy} onClick={() => drain(n, "undrain")}>
                        Undrain
                      </button>
                    ) : (
                      <button className="ghost danger" disabled={busy} onClick={() => drain(n, "drain")}>
                        Drain
                      </button>
                    ))}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <Curl method="GET" path="/v1/nodes" />
    </Section>
  );
}

// --- approvals -------------------------------------------------------------

function ApprovalsTab({ canDecide }: { canDecide: boolean }) {
  const [status, setStatus] = useState("pending");
  const { data, error, reload } = useLoad(() => get<{ approvals: Approval[] }>(`/v1/approvals${status ? `?status=${status}` : ""}`), [status]);
  const { run, busy } = useAction();
  const decide = async (a: Approval, verb: "approve" | "deny") => {
    const reason = prompt(`Reason for ${verb === "approve" ? "approving" : "denying"} ${a.action}?`) ?? "";
    await run(() => post(`/v1/approvals/${a.id}/${verb}`, { reason }), verb === "approve" ? "Approved" : "Denied");
    reload();
  };
  return (
    <Section
      title="Approvals"
      actions={
        <select value={status} onChange={(e) => setStatus(e.target.value)}>
          {["pending", "approved", "used", "denied", "expired", ""].map((s) => (
            <option key={s} value={s}>
              {s || "all"}
            </option>
          ))}
        </select>
      }
    >
      <p className="muted">
        An approval is used once, for exactly the request that was parked, within 15 minutes. The requester repeats the request with header{" "}
        <code>{APPROVAL_HEADER}</code>. API keys can never approve.
      </p>
      {!data ? (
        <Loading error={error} />
      ) : data.approvals.length === 0 ? (
        <p className="muted">Nothing here.</p>
      ) : (
        <table>
          <thead>
            <tr>
              <th>ID</th>
              <th>Action</th>
              <th>Target</th>
              <th>Requested by</th>
              <th>Status</th>
              <th>Created</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {data.approvals.map((a) => (
              <tr key={a.id}>
                <td className="mono small">{a.id}</td>
                <td>{a.action}</td>
                <td className="mono small">{a.target}</td>
                <td>{a.requested_by}</td>
                <td>
                  <StatusBadge status={a.status} />
                  {a.decided_by && <div className="small muted">by {a.decided_by}{a.reason ? `: ${a.reason}` : ""}</div>}
                </td>
                <td className="small">{fmtTime(a.created_at)}</td>
                <td className="row right">
                  <details>
                    <summary className="small">payload</summary>
                    <pre className="term small">{prettyJSON(a.payload)}</pre>
                  </details>
                  {canDecide && a.status === "pending" && (
                    <>
                      <button disabled={busy} onClick={() => decide(a, "approve")}>
                        Approve
                      </button>
                      <button className="ghost danger" disabled={busy} onClick={() => decide(a, "deny")}>
                        Deny
                      </button>
                    </>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </Section>
  );
}

function prettyJSON(s: string) {
  try {
    return JSON.stringify(JSON.parse(s), null, 2);
  } catch {
    return s;
  }
}

// --- users -----------------------------------------------------------------

const ROLES: Role[] = ["admin", "operator", "viewer"];

function UsersTab() {
  const { data, error, reload } = useLoad(() => get<{ users: User[] }>("/v1/users"));
  const { run, busy } = useAction();
  const [form, setForm] = useState({ username: "", password: "", role: "operator" as Role });

  const create = async (e: React.FormEvent) => {
    e.preventDefault();
    const ok = await run(() => post("/v1/users", form), `User ${form.username} created`);
    if (ok) setForm({ username: "", password: "", role: "operator" });
    reload();
  };
  const update = async (u: User, body: Partial<{ role: Role; disabled: boolean; password: string }>, msg: string) => {
    await run(() => patch(`/v1/users/${u.id}`, body), msg);
    reload();
  };
  const remove = async (u: User) => {
    if (!confirm(`Delete user ${u.username}?`)) return;
    await run(() => del(`/v1/users/${u.id}`), "User deleted");
    reload();
  };

  return (
    <Section title="Users">
      {!data ? (
        <Loading error={error} />
      ) : (
        <table>
          <thead>
            <tr>
              <th>Username</th>
              <th>Role</th>
              <th>Status</th>
              <th>Created</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {data.users.map((u) => (
              <tr key={u.id}>
                <td>{u.username}</td>
                <td>
                  <select value={u.role} disabled={busy} onChange={(e) => update(u, { role: e.target.value as Role }, "Role changed")}>
                    {ROLES.map((r) => (
                      <option key={r}>{r}</option>
                    ))}
                  </select>
                </td>
                <td>{u.disabled ? <span className="badge st-error">disabled</span> : <span className="badge st-running">active</span>}</td>
                <td className="small">{fmtTime(u.created_at)}</td>
                <td className="row right">
                  <button className="ghost" disabled={busy} onClick={() => update(u, { disabled: !u.disabled }, u.disabled ? "User enabled" : "User disabled")}>
                    {u.disabled ? "Enable" : "Disable"}
                  </button>
                  <button
                    className="ghost"
                    disabled={busy}
                    onClick={() => {
                      const pw = prompt(`New password for ${u.username} (min 8 chars)`);
                      if (pw) void update(u, { password: pw }, "Password changed");
                    }}
                  >
                    Set password
                  </button>
                  <button className="ghost danger" disabled={busy} onClick={() => remove(u)}>
                    Delete
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <form className="row" onSubmit={create}>
        <input placeholder="username" value={form.username} onChange={(e) => setForm({ ...form, username: e.target.value })} required />
        <input type="password" placeholder="password (min 8)" minLength={8} value={form.password} onChange={(e) => setForm({ ...form, password: e.target.value })} required />
        <select value={form.role} onChange={(e) => setForm({ ...form, role: e.target.value as Role })}>
          {ROLES.map((r) => (
            <option key={r}>{r}</option>
          ))}
        </select>
        <button disabled={busy}>Add user</button>
      </form>
      <Curl method="POST" path="/v1/users" body={{ username: form.username || "alice", password: "…", role: form.role }} />
    </Section>
  );
}

// --- API keys --------------------------------------------------------------

const SCOPES = ["health", "sandboxes:read", "sandboxes:write", "sandboxes:exec", "snapshots", "sandboxes:egress"];

function KeysTab() {
  const { data, error, reload } = useLoad(() => get<{ keys: ApiKey[] }>("/v1/api-keys"));
  const { run, busy } = useAction();
  const [name, setName] = useState("");
  const [role, setRole] = useState<Role>("operator");
  const [scopes, setScopes] = useState<string[]>([]);
  const [ttl, setTtl] = useState("");
  const [sandbox, setSandbox] = useState("");
  const [minted, setMinted] = useState("");

  const body = {
    name,
    role,
    ...(scopes.length ? { scopes } : {}),
    ...(ttl ? { ttl_seconds: Number(ttl) } : {}),
    ...(sandbox ? { sandbox_id: sandbox } : {}),
  };
  const create = async (e: React.FormEvent) => {
    e.preventDefault();
    const r = await run(() => post<{ key: string }>("/v1/api-keys", body), "API key created");
    if (r?.key) {
      setMinted(r.key);
      setName("");
    }
    reload();
  };
  const revoke = async (k: ApiKey) => {
    if (!confirm(`Revoke key ${k.name}?`)) return;
    await run(() => del(`/v1/api-keys/${k.id}`), "Key revoked");
    reload();
  };

  return (
    <Section title="API keys">
      {minted && (
        <div className="notice">
          Copy this key now; it is not shown again: <code className="mono">{minted}</code>{" "}
          <button className="ghost" onClick={() => navigator.clipboard?.writeText(minted)}>
            Copy
          </button>
        </div>
      )}
      {!data ? (
        <Loading error={error} />
      ) : (
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Prefix</th>
              <th>Role</th>
              <th>Scopes</th>
              <th>Sandbox</th>
              <th>Expires</th>
              <th>Last used</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {data.keys.map((k) => (
              <tr key={k.id}>
                <td>{k.name}</td>
                <td className="mono small">{k.prefix}</td>
                <td>{k.role ?? "admin"}</td>
                <td className="small">{k.scopes?.length ? k.scopes.join(", ") : "all (per role)"}</td>
                <td className="mono small">{k.sandbox_id || "—"}</td>
                <td className="small">{fmtTime(k.expires_at)}</td>
                <td className="small">{fmtTime(k.last_used_at)}</td>
                <td className="right">
                  <button className="ghost danger" disabled={busy} onClick={() => revoke(k)}>
                    Revoke
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <form className="form" onSubmit={create}>
        <div className="cols">
          <label>
            Name
            <input value={name} onChange={(e) => setName(e.target.value)} required />
          </label>
          <label>
            Role
            <select value={role} onChange={(e) => setRole(e.target.value as Role)}>
              {ROLES.map((r) => (
                <option key={r}>{r}</option>
              ))}
            </select>
          </label>
          <label>
            Lifetime (seconds)
            <input type="number" min="0" placeholder="no expiry / instance max" value={ttl} onChange={(e) => setTtl(e.target.value)} />
          </label>
          <label>
            Bind to sandbox
            <input className="mono" placeholder="sandbox id (optional)" value={sandbox} onChange={(e) => setSandbox(e.target.value.trim())} />
          </label>
        </div>
        <div className="row wrap">
          <span className="small">Scopes (none = everything the role allows):</span>
          {SCOPES.map((s) => (
            <label key={s} className="check small">
              <input type="checkbox" checked={scopes.includes(s)} onChange={(e) => setScopes(e.target.checked ? [...scopes, s] : scopes.filter((x) => x !== s))} />
              {s}
            </label>
          ))}
        </div>
        <div>
          <button disabled={busy || !name}>Create key</button>
        </div>
      </form>
      <Curl method="POST" path="/v1/api-keys" body={body} />
    </Section>
  );
}

// --- audit -----------------------------------------------------------------

function AuditTab() {
  const [action, setAction] = useState("");
  const [before, setBefore] = useState<number | undefined>();
  const q = new URLSearchParams({ limit: "100" });
  if (action) q.set("action", action);
  if (before) q.set("before", String(before));
  const { data, error } = useLoad(() => get<{ entries: AuditEntry[] }>(`/v1/audit?${q}`), [action, before]);
  const verify = useLoad(() => get<{ ok: boolean; checked: number; unchained: number; broken_at?: number; reason?: string }>("/v1/audit/verify"));
  const last = data?.entries[data.entries.length - 1];
  return (
    <Section
      title="Audit log"
      actions={
        <>
          <input placeholder="filter action, e.g. sandbox.create" value={action} onChange={(e) => (setAction(e.target.value.trim()), setBefore(undefined))} />
          <button className="ghost" onClick={verify.reload}>
            Verify chain
          </button>
        </>
      }
    >
      {verify.data && (
        <p className={verify.data.ok ? "ok" : "error"}>
          {verify.data.ok
            ? `Hash chain intact (${verify.data.checked} rows checked${verify.data.unchained ? `, ${verify.data.unchained} older unchained rows` : ""}).`
            : `Hash chain BROKEN at row ${verify.data.broken_at}: ${verify.data.reason}`}
        </p>
      )}
      {!data ? (
        <Loading error={error} />
      ) : (
        <table>
          <thead>
            <tr>
              <th>#</th>
              <th>Time</th>
              <th>Actor</th>
              <th>Action</th>
              <th>Target</th>
              <th>Detail</th>
            </tr>
          </thead>
          <tbody>
            {data.entries.map((e) => (
              <tr key={e.id}>
                <td className="small">{e.id}</td>
                <td className="small">{fmtTime(e.at)}</td>
                <td className="small">
                  {e.actor_kind}
                  {e.actor_name ? `:${e.actor_name}` : ""}
                </td>
                <td>{e.action}</td>
                <td className="mono small">{e.target}</td>
                <td className="mono small detail">{e.detail}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <div className="row">
        {before && (
          <button className="ghost" onClick={() => setBefore(undefined)}>
            Newest
          </button>
        )}
        {data && data.entries.length === 100 && last && (
          <button className="ghost" onClick={() => setBefore(last.id)}>
            Older →
          </button>
        )}
      </div>
    </Section>
  );
}

// --- domain ----------------------------------------------------------------

interface Verification {
  ok: boolean;
  hint?: string;
  checks: { host: string; addrs: string[]; ok: boolean; error?: string }[];
}
interface Cert {
  id: string;
  subject: string;
  dns_names: string[];
  not_after: string;
  default: boolean;
}

function DomainTab() {
  const { data, error, reload } = useLoad(() => get<DomainInfo>("/v1/domain"));
  const certs = useLoad(() => get<{ enabled: boolean; certificates: Cert[] }>("/v1/domain/certificates"));
  const { run, busy } = useAction();
  const [form, setForm] = useState({ domain: "", tls_mode: "off", acme_email: "" });
  const [ver, setVer] = useState<Verification | null>(null);
  const [expect, setExpect] = useState("");
  const [cert, setCert] = useState({ cert_pem: "", key_pem: "" });

  useEffect(() => {
    if (data) setForm({ domain: data.domain, tls_mode: data.tls_mode, acme_email: data.acme_email ?? "" });
  }, [data]);
  if (!data) return <Loading error={error} />;

  const save = async () => {
    await run(() => put("/v1/domain", { domain: form.domain, tls_mode: form.tls_mode, ...(form.acme_email ? { acme_email: form.acme_email } : {}) }), "Domain saved");
    reload();
  };
  const verify = async () => {
    const ips = expect.split(/[\s,]+/).filter(Boolean);
    const r = await run(() => post<Verification>("/v1/domain/verify", ips.length ? { expect_ips: ips } : {}));
    if (r) setVer(r);
  };
  const upload = async () => {
    const r = await run(() => post("/v1/domain/certificates", cert), "Certificate installed");
    if (r) setCert({ cert_pem: "", key_pem: "" });
    certs.reload();
  };

  return (
    <>
      <Section title="Domain">
        {data.restart_required && (
          <p className="notice">
            Saved settings differ from what is running ({data.running.domain}, TLS {data.running.tls_mode}). Restart sandboxd to apply them.
          </p>
        )}
        <div className="form">
          <div className="cols">
            <label>
              Domain
              <input value={form.domain} onChange={(e) => setForm({ ...form, domain: e.target.value })} />
            </label>
            <label>
              TLS
              <select value={form.tls_mode} onChange={(e) => setForm({ ...form, tls_mode: e.target.value })}>
                <option value="off">off (HTTP)</option>
                <option value="custom">custom certificate</option>
                <option value="letsencrypt">Let's Encrypt (resolver in traefik.yml)</option>
              </select>
            </label>
            {form.tls_mode === "letsencrypt" && (
              <label>
                ACME email
                <input type="email" value={form.acme_email} onChange={(e) => setForm({ ...form, acme_email: e.target.value })} />
              </label>
            )}
          </div>
          <p className="small muted">
            Console: <code>{data.console_host}</code> · previews: <code>{data.preview_example}</code>
          </p>
          <div>
            <button disabled={busy} onClick={save}>
              Save domain
            </button>
          </div>
        </div>
        <Curl method="PUT" path="/v1/domain" body={{ domain: form.domain, tls_mode: form.tls_mode }} />
      </Section>

      <Section title="DNS check">
        <div className="row">
          <input placeholder="expected server IPs (optional)" value={expect} onChange={(e) => setExpect(e.target.value)} />
          <button className="ghost" disabled={busy} onClick={verify}>
            Check DNS
          </button>
        </div>
        {ver && (
          <>
            <p className={ver.ok ? "ok" : "error"}>{ver.ok ? "DNS looks right." : ver.hint}</p>
            <ul>
              {ver.checks.map((c) => (
                <li key={c.host}>
                  <code>{c.host}</code> → {c.addrs.join(", ") || "—"} {c.ok ? "✓" : <span className="error">{c.error}</span>}
                </li>
              ))}
            </ul>
          </>
        )}
      </Section>

      <Section title="Certificates">
        {!certs.data ? (
          <Loading error={certs.error} />
        ) : !certs.data.enabled ? (
          <p className="muted">Certificate storage is not configured (SANDBOXD_TRAEFIK_DYNAMIC_DIR).</p>
        ) : (
          <>
            {certs.data.certificates.length === 0 ? (
              <p className="muted">No custom certificates.</p>
            ) : (
              <table>
                <tbody>
                  {certs.data.certificates.map((c) => (
                    <tr key={c.id}>
                      <td className="mono small">{c.id}</td>
                      <td>{c.dns_names.join(", ")}</td>
                      <td className="small">expires {fmtTime(c.not_after)}</td>
                      <td>{c.default && <span className="badge">default</span>}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
            <div className="form">
              <div className="cols">
                <label>
                  Certificate (PEM, full chain)
                  <textarea className="mono" rows={5} value={cert.cert_pem} onChange={(e) => setCert({ ...cert, cert_pem: e.target.value })} />
                </label>
                <label>
                  Private key (PEM)
                  <textarea className="mono" rows={5} value={cert.key_pem} onChange={(e) => setCert({ ...cert, key_pem: e.target.value })} />
                </label>
              </div>
              <div>
                <button disabled={busy || !cert.cert_pem || !cert.key_pem} onClick={upload}>
                  Install certificate
                </button>
              </div>
            </div>
          </>
        )}
      </Section>
    </>
  );
}
