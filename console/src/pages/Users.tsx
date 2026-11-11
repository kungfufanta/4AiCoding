import { useMemo, useState } from "react";
import { del, get, patch, post, type Role, type User } from "../api";
import { can, Curl, fmtTime, Loading, Section, useAction, useLoad, useMe } from "../ui";

// Merged from the SBX Console template ("User Management" view): a dedicated
// users page with search, role filter, inline role changes, disable/enable,
// delete and account creation — on top of the existing /v1/users endpoints.

const ROLES: Role[] = ["admin", "operator", "viewer"];

export function UsersPage() {
  const me = useMe();
  const { run, busy } = useAction();
  const { data, error, reload } = useLoad<{ users: User[] }>(() => get("/v1/users"), []);

  const [query, setQuery] = useState("");
  const [roleFilter, setRoleFilter] = useState<string>("all");
  const [form, setForm] = useState<{ username: string; password: string; role: Role }>({
    username: "",
    password: "",
    role: "viewer",
  });

  const users = data?.users ?? [];
  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    return users.filter(
      (u) =>
        (roleFilter === "all" || u.role === roleFilter) &&
        (!q || u.username.toLowerCase().includes(q) || u.role.includes(q)),
    );
  }, [users, query, roleFilter]);

  if (error) return <Loading error={error} />;
  if (!data) return <Loading />;

  const setRole = async (u: User, role: Role) => {
    if (await run(() => patch(`/v1/users/${u.id}`, { role }), `Role of ${u.username} set to ${role}`)) reload();
  };
  const setDisabled = async (u: User, disabled: boolean) => {
    const msg = disabled ? `User ${u.username} disabled` : `User ${u.username} enabled`;
    if (await run(() => patch(`/v1/users/${u.id}`, { disabled }), msg)) reload();
  };
  const remove = async (u: User) => {
    if (!confirm(`Delete user ${u.username}? This cannot be undone.`)) return;
    if (await run(() => del(`/v1/users/${u.id}`), "User deleted")) reload();
  };
  const create = async (e: React.FormEvent) => {
    e.preventDefault();
    if (await run(() => post("/v1/users", form), `User ${form.username} created`)) {
      setForm({ username: "", password: "", role: "viewer" });
      reload();
    }
  };

  const counts = ROLES.map((r) => `${users.filter((u) => u.role === r).length} ${r}`).join(" · ");

  return (
    <>
      <div className="row wrap" style={{ justifyContent: "space-between" }}>
        <h2 style={{ margin: 0 }}>Users</h2>
        <span className="muted">{counts}</span>
      </div>

      <div className="row wrap">
        <input
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder="Search name or role…"
          style={{ minWidth: 220 }}
        />
        <select value={roleFilter} onChange={(e) => setRoleFilter(e.target.value)}>
          <option value="all">All roles</option>
          {ROLES.map((r) => (
            <option key={r} value={r}>
              {r}
            </option>
          ))}
        </select>
      </div>

      <Section title={`Accounts (${filtered.length})`}>
        <div className="scroll-x">
          <table>
            <thead>
              <tr>
                <th>Username</th>
                <th>Role</th>
                <th>Status</th>
                <th>Created</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {filtered.length === 0 && (
                <tr>
                  <td colSpan={5} className="muted">
                    No matching users.
                  </td>
                </tr>
              )}
              {filtered.map((u) => (
                <tr key={u.id}>
                  <td>
                    <b>{u.username}</b>
                    {u.username === me?.user && <span className="muted"> (you)</span>}
                  </td>
                  <td>
                    <select
                      value={u.role}
                      disabled={busy || !can(me, "admin") || u.username === me?.user}
                      onChange={(e) => setRole(u, e.target.value as Role)}
                    >
                      {ROLES.map((r) => (
                        <option key={r} value={r}>
                          {r}
                        </option>
                      ))}
                    </select>
                  </td>
                  <td>
                    <span className={`badge ${u.disabled ? "st-error" : "st-running"}`}>
                      {u.disabled ? "disabled" : "active"}
                    </span>
                  </td>
                  <td className="muted">{fmtTime(u.created_at)}</td>
                  <td>
                    <div className="row right">
                      <button
                        className="ghost"
                        disabled={busy || !can(me, "admin") || u.username === me?.user}
                        onClick={() => setDisabled(u, !u.disabled)}
                      >
                        {u.disabled ? "Enable" : "Disable"}
                      </button>
                      <button
                        className="danger"
                        disabled={busy || !can(me, "admin") || u.username === me?.user}
                        onClick={() => remove(u)}
                      >
                        Delete
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </Section>

      {can(me, "admin") && (
        <Section title="Create account">
          <form onSubmit={create} className="row wrap">
            <input
              required
              value={form.username}
              onChange={(e) => setForm({ ...form, username: e.target.value })}
              placeholder="username"
              style={{ width: 180 }}
            />
            <input
              required
              type="password"
              minLength={8}
              value={form.password}
              onChange={(e) => setForm({ ...form, password: e.target.value })}
              placeholder="password (min 8 chars)"
              style={{ width: 200 }}
            />
            <select value={form.role} onChange={(e) => setForm({ ...form, role: e.target.value as Role })}>
              {ROLES.map((r) => (
                <option key={r} value={r}>
                  {r}
                </option>
              ))}
            </select>
            <button type="submit" disabled={busy}>
              Create user
            </button>
            <Curl method="POST" path="/v1/users" body={{ username: "alice", password: "…", role: "viewer" }} />
          </form>
        </Section>
      )}
    </>
  );
}
