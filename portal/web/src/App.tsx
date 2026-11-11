import { useCallback, useEffect, useState } from "react";
import { ApiError, api, type AuditEvent, type AuthMethods, type Member, type Org, type Project, type User } from "./api";

// S1 dashboard (PORTAL_PLAN 6/S1): login via SSO, org switcher,
// projects, members (invite + role change). No router yet — state
// tabs keep the bundle tiny.

type Me = { user: User; orgs: Org[] };

export function App() {
  const [me, setMe] = useState<Me | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [methods, setMethods] = useState<AuthMethods | null>(null);
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [signingIn, setSigningIn] = useState(false);

  // The server decides which sign-in surfaces to render: SSO when an
  // OIDC provider is configured, password for the local admin.
  useEffect(() => {
    api.authMethods().then(setMethods).catch(() => setMethods({ sso: true, password: false }));
  }, []);

  const refresh = useCallback(async () => {
    try {
      setMe(await api.me());
      setError(null);
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) setMe(null);
      else setError(String(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh]);

  if (loading) return <main className="shell"><p className="muted">Loading…</p></main>;

  if (!me) {
    return (
      <main className="shell">
        <header className="topbar">
          <span className="brand">Portal</span>
          <span className="sub">sandboxes for agents</span>
        </header>
        <section className="card login-card">
          <h2>Sign in</h2>
          {methods?.sso && (
            <>
              <p className="muted">Single sign-on via your organisation's identity provider.</p>
              <a className="btn" href="/auth/login">Sign in with SSO</a>
            </>
          )}
          {methods?.sso && methods?.password && <div className="divider">or</div>}
          {methods?.password && (
            <form
              className="row-form login-form"
              onSubmit={async (e) => {
                e.preventDefault();
                setSigningIn(true);
                try {
                  await api.passwordLogin(username, password);
                  setUsername("");
                  setPassword("");
                  await refresh();
                } catch (err) {
                  setError(err instanceof Error ? err.message : String(err));
                } finally {
                  setSigningIn(false);
                }
              }}
            >
              <input
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                placeholder="Username"
                autoComplete="username"
                required
              />
              <input
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                placeholder="Password"
                autoComplete="current-password"
                required
              />
              <button className="btn" type="submit" disabled={signingIn}>
                {signingIn ? "Signing in…" : "Sign in"}
              </button>
            </form>
          )}
          {!methods && <p className="muted">Loading…</p>}
          {error && <p className="bad">{error}</p>}
        </section>
      </main>
    );
  }

  return <Dashboard me={me} onChanged={refresh} />;
}

function Dashboard({ me, onChanged }: { me: Me; onChanged: () => Promise<void> }) {
  const [orgID, setOrgID] = useState<string>(me.orgs[0]?.id ?? "");
  const [tab, setTab] = useState<"projects" | "members" | "audit">("projects");
  const org = me.orgs.find((o) => o.id === orgID);

  async function createOrg() {
    const name = prompt("Organisation name");
    if (!name) return;
    try {
      const org = await api.createOrg(name);
      await onChanged();
      setOrgID(org.id);
    } catch (e) {
      alert(e instanceof Error ? e.message : String(e));
    }
  }

  return (
    <main className="shell">
      <header className="topbar">
        <span className="brand">Portal</span>
        <select value={orgID} onChange={(e) => setOrgID(e.target.value)} className="org-select">
          {me.orgs.map((o) => (
            <option key={o.id} value={o.id}>{o.name}</option>
          ))}
        </select>
        <button className="btn small" onClick={createOrg}>+ New org</button>
        <span className="spacer" />
        <span className="muted">{me.user.email}</span>
        <button
          className="btn small ghost"
          onClick={async () => {
            await api.logout();
            location.reload();
          }}
        >
          Sign out
        </button>
      </header>

      {org ? (
        <>
          <nav className="tabs">
            {(["projects", "members", "audit"] as const).map((t) => (
              <button key={t} className={"tab" + (tab === t ? " active" : "")} onClick={() => setTab(t)}>
                {t[0].toUpperCase() + t.slice(1)}
              </button>
            ))}
          </nav>
          {tab === "projects" && <Projects orgID={org.id} />}
          {tab === "members" && <Members orgID={org.id} viewerEmail={me.user.email} />}
          {tab === "audit" && <Audit orgID={org.id} />}
        </>
      ) : (
        <section className="card">
          <h2>No organisation yet</h2>
          <p className="muted">Create one to start inviting your team.</p>
          <button className="btn" onClick={createOrg}>Create an organisation</button>
        </section>
      )}
    </main>
  );
}

function Projects({ orgID }: { orgID: string }) {
  const [projects, setProjects] = useState<Project[]>([]);
  const [name, setName] = useState("");

  const load = useCallback(async () => {
    setProjects(await api.listProjects(orgID));
  }, [orgID]);

  useEffect(() => {
    load().catch((e) => alert(e instanceof Error ? e.message : String(e)));
  }, [load]);

  return (
    <section className="card">
      <h2>Projects</h2>
      <ul className="rows">
        {projects.map((p) => (
          <li key={p.id}>
            <strong>{p.name}</strong> <span className="muted">/{p.slug}</span>
          </li>
        ))}
        {projects.length === 0 && <li className="muted">No projects yet.</li>}
      </ul>
      <form
        className="row-form"
        onSubmit={async (e) => {
          e.preventDefault();
          try {
            await api.createProject(orgID, name);
            setName("");
            await load();
          } catch (err) {
            alert(err instanceof Error ? err.message : String(err));
          }
        }}
      >
        <input value={name} onChange={(e) => setName(e.target.value)} placeholder="New project name" required />
        <button className="btn" type="submit">Create project</button>
      </form>
    </section>
  );
}

const ROLES = ["admin", "member", "billing"] as const;

function Members({ orgID, viewerEmail }: { orgID: string; viewerEmail: string }) {
  const [members, setMembers] = useState<Member[]>([]);
  const [email, setEmail] = useState("");
  const [role, setRole] = useState<string>("member");
  const [inviteURL, setInviteURL] = useState<string | null>(null);

  const load = useCallback(async () => {
    setMembers(await api.listMembers(orgID));
  }, [orgID]);

  useEffect(() => {
    load().catch((e) => alert(e instanceof Error ? e.message : String(e)));
  }, [load]);

  // The server enforces the admin gate (non-admins get a 404); the
  // UI only mirrors it.
  const viewerRole = members.find((m) => m.email === viewerEmail)?.role;
  const isAdmin = viewerRole === "owner" || viewerRole === "admin";

  return (
    <section className="card">
      <h2>Members</h2>
      <ul className="rows">
        {members.map((m) => (
          <li key={m.user_id} className="member-row">
            <span>
              <strong>{m.name || m.email}</strong> <span className="muted">{m.email}</span>
            </span>
            {m.role === "owner" || !isAdmin ? (
              <span className="pill">{m.role}</span>
            ) : (
              <select
                defaultValue={m.role}
                onChange={async (e) => {
                  try {
                    await api.setMemberRole(orgID, m.user_id, e.target.value);
                    await load();
                  } catch (err) {
                    alert(err instanceof Error ? err.message : String(err));
                  }
                }}
              >
                {ROLES.map((r) => (
                  <option key={r} value={r}>{r}</option>
                ))}
              </select>
            )}
          </li>
        ))}
      </ul>

      {isAdmin && (
        <>
          <h3>Invite a member</h3>
          <form
            className="row-form"
            onSubmit={async (e) => {
              e.preventDefault();
              try {
                const { url } = await api.createInvite(orgID, email, role);
                setInviteURL(url);
                setEmail("");
                await load();
              } catch (err) {
                alert(err instanceof Error ? err.message : String(err));
              }
            }}
          >
            <input type="email" value={email} onChange={(e) => setEmail(e.target.value)} placeholder="teammate@company.com" required />
            <select value={role} onChange={(e) => setRole(e.target.value)}>
              {ROLES.map((r) => (
                <option key={r} value={r}>{r}</option>
              ))}
            </select>
            <button className="btn" type="submit">Invite</button>
          </form>
          {inviteURL && (
            <p className="invite-once">
              Share this invite link <strong>now — it is shown only once</strong>:
              <code>{inviteURL}</code>
            </p>
          )}
        </>
      )}
    </section>
  );
}

function Audit({ orgID }: { orgID: string }) {
  const [events, setEvents] = useState<AuditEvent[]>([]);
  useEffect(() => {
    api.audit(orgID).then(setEvents).catch((e) => alert(e instanceof Error ? e.message : String(e)));
  }, [orgID]);
  return (
    <section className="card">
      <h2>Audit trail</h2>
      <ul className="rows mono">
        {events.map((e) => (
          <li key={e.id}>
            <span className="pill">{e.action}</span> <span className="muted">by {e.actor_type} {e.actor_id.slice(0, 8)}…</span>{" "}
            {new Date(e.occurred_at).toLocaleString()}
          </li>
        ))}
        {events.length === 0 && <li className="muted">Nothing recorded yet.</li>}
      </ul>
    </section>
  );
}
