import { useMemo, useState } from "react";
import { getPermissions, putPermissions, type PermGrant, type PermMatrix } from "../api";
import { can, Loading, Section, StatusBadge, useAction, useLoad, useMe, useNotify } from "../ui";

// Merged from the SBX Console template ("Sandbox Permissions" view): a
// user × sandbox matrix of R/W/X grants with live cascade (W/X imply R),
// unsaved-changes tracking, Reset/Save, and a per-user access summary.

const KEYS = ["r", "w", "x"] as const;
type Key = (typeof KEYS)[number];
const LABEL: Record<Key, string> = { r: "R", w: "W", x: "X" };

function labelOf(g?: PermGrant): string {
  if (!g) return "—";
  const on = KEYS.filter((k) => g[k]);
  return on.length ? on.map((k) => LABEL[k]).join("·") : "—";
}

export function PermissionsPage() {
  const me = useMe();
  const notify = useNotify();
  const { run, busy } = useAction();
  const { data, error, reload } = useLoad<PermMatrix>(() => getPermissions(), []);

  const [saved, setSaved] = useState<PermGrant[] | null>(null); // last loaded/saved snapshot
  const [draft, setDraft] = useState<PermGrant[] | null>(null); // working copy
  const [selected, setSelected] = useState<string | null>(null);
  const [query, setQuery] = useState("");

  const grants = draft ?? saved ?? data?.grants ?? [];
  const snapshot = saved ?? data?.grants ?? [];
  const dirty = useMemo(() => JSON.stringify(grants) !== JSON.stringify(snapshot), [grants, snapshot]);

  if (error) return <Loading error={error} />;
  if (!data) return <Loading />;

  const users = data.users.filter(
    (u) =>
      !query.trim() ||
      u.username.toLowerCase().includes(query.trim().toLowerCase()) ||
      u.role.includes(query.trim().toLowerCase()),
  );
  const selectedId = selected ?? users[0]?.id ?? null;
  const selectedUser = data.users.find((u) => u.id === selectedId);

  const cellOf = (uid: string, sid: string): PermGrant =>
    grants.find((g) => g.user_id === uid && g.sandbox_id === sid) ?? {
      user_id: uid,
      sandbox_id: sid,
      r: false,
      w: false,
      x: false,
    };

  const toggle = (uid: string, sid: string, k: Key) => {
    const next = grants.map((g) => ({ ...g }));
    const row = next.find((g) => g.user_id === uid && g.sandbox_id === sid);
    const cur = row ?? { user_id: uid, sandbox_id: sid, r: false, w: false, x: false };
    cur[k] = !cur[k];
    if (cur[k] && (k === "w" || k === "x")) cur.r = true; // W/X imply R
    if (!cur[k] && k === "r") {
      cur.w = false; // taking R away takes W/X with it
      cur.x = false;
    }
    const idx = next.findIndex((g) => g.user_id === uid && g.sandbox_id === sid);
    if (idx >= 0) next[idx] = cur;
    else next.push(cur);
    setDraft(next);
  };

  const save = async () => {
    const ok = await run(() => putPermissions(grants), "Permissions saved");
    if (ok) {
      setSaved(grants);
      setDraft(null);
      reload();
    }
  };

  const reset = () => {
    setDraft(null);
    notify({ kind: "info", text: "Changes reset to the last saved matrix." });
  };

  const totalGrants = selectedId
    ? data.sandboxes.reduce((acc, s) => {
        const g = cellOf(selectedId, s.id);
        return acc + KEYS.filter((k) => g[k]).length;
      }, 0)
    : 0;
  const canSave = can(me, "admin");

  return (
    <>
      <div className="row wrap" style={{ justifyContent: "space-between" }}>
        <h2 style={{ margin: 0 }}>Permissions</h2>
        <div className="row">
          <span className={dirty ? "dirty" : "synced"}>{dirty ? "● unsaved changes" : "● synced"}</span>
          <button className="ghost" onClick={reset} disabled={!dirty}>
            Reset
          </button>
          <button onClick={save} disabled={!dirty || busy || !canSave} title={canSave ? "" : "Requires the admin role"}>
            Save changes
          </button>
        </div>
      </div>
      <p className="muted">Grant Read / Write / Execute per sandbox. W and X imply R — taking R away clears W/X.</p>

      <div className="perm-search">
        <input value={query} onChange={(e) => setQuery(e.target.value)} placeholder="Search users or roles…" />
      </div>

      <div className="perm-layout">
        <Section title="Matrix">
          <div className="scroll-x">
            <table className="perm-table">
              <thead>
                <tr>
                  <th>User</th>
                  {data.sandboxes.map((s) => (
                    <th key={s.id}>
                      <span className="mono">{s.id}</span>
                      <br />
                      <StatusBadge status={s.status} />
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {users.length === 0 && (
                  <tr>
                    <td colSpan={data.sandboxes.length + 1} className="muted">
                      No matching users.
                    </td>
                  </tr>
                )}
                {users.map((u) => (
                  <tr
                    key={u.id}
                    className={u.id === selectedId ? "active" : ""}
                    onClick={() => setSelected(u.id)}
                  >
                    <td>
                      <b>{u.username}</b>
                      <div className="muted small">{u.role}</div>
                    </td>
                    {data.sandboxes.map((s) => {
                      const g = cellOf(u.id, s.id);
                      return (
                        <td key={s.id} onClick={(e) => e.stopPropagation()}>
                          <div className="row" style={{ gap: 4 }}>
                            {KEYS.map((k) => (
                              <button
                                key={k}
                                type="button"
                                className={`perm-chip${g[k] ? " on" : ""}`}
                                aria-pressed={g[k]}
                                aria-label={`${LABEL[k]} for ${u.username} on ${s.id}`}
                                onClick={() => toggle(u.id, s.id, k)}
                              >
                                {LABEL[k]}
                              </button>
                            ))}
                          </div>
                        </td>
                      );
                    })}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <p className="muted small">
            <b className="ok-text">R</b> = Read · <b className="ok-text">W</b> = Write ·{" "}
            <b className="ok-text">X</b> = Execute — W/X tự động cấp R
          </p>
        </Section>

        <Section title="Access summary">
          {selectedUser ? (
            <>
              <p>
                <b>{selectedUser.username}</b> <span className="muted small">({selectedUser.role})</span>
              </p>
              <ul className="perm-summary">
                {data.sandboxes.map((s) => {
                  const label = labelOf(cellOf(selectedUser.id, s.id));
                  return (
                    <li key={s.id}>
                      <span className="mono">{s.id}</span>
                      <b className={label === "—" ? "muted" : "ok-text"}>{label}</b>
                    </li>
                  );
                })}
              </ul>
              <div className="row" style={{ justifyContent: "space-between", borderTop: "1px solid var(--line)", paddingTop: 8 }}>
                <span className="muted">Total grants</span>
                <span className="mono">
                  {totalGrants} / {data.sandboxes.length * 3}
                </span>
              </div>
            </>
          ) : (
            <p className="muted">No users yet.</p>
          )}
        </Section>
      </div>
    </>
  );
}
