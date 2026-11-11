import { getOverview, type Overview } from "../api";
import { fmtTime, Link, Loading, Section, StatusBadge, useLoad } from "../ui";

// Merged from the SBX Console template ("Overview" view): fleet KPIs, recent
// sandboxes and the audit tail — everything real, straight from /v1.

function Kpi({ label, value, tone }: { label: string; value: string | number; tone?: string }) {
  return (
    <div className="kpi">
      <b className={tone}>{value}</b>
      <span>{label}</span>
    </div>
  );
}

export function OverviewPage() {
  const { data, error, reload } = useLoad<Overview>(() => getOverview(), []);
  if (error) return <Loading error={error} />;
  if (!data) return <Loading />;
  const o = data;

  return (
    <>
      <div className="row wrap" style={{ justifyContent: "space-between" }}>
        <h2 style={{ margin: 0 }}>Overview</h2>
        <button className="ghost" onClick={reload}>
          Refresh
        </button>
      </div>

      <div className="kpis">
        <Kpi label="sandboxes running" value={`${o.sandboxes.running} / ${o.sandboxes.total}`} tone="ok" />
        {o.users && <Kpi label="users" value={o.users.total} />}
        <Kpi label="open approvals" value={o.approvals_open} tone={o.approvals_open ? "warn" : undefined} />
        <Kpi label="requests blocked (24h)" value={`${o.net_events_24h.blocked} / ${o.net_events_24h.total}`} />
        <Kpi label="exposed ports" value={o.ports_exposed} />
      </div>

      <div className="cols-2">
        <Section
          title="Recent sandboxes"
          actions={
            <Link to="/sandboxes" className="muted">
              view all
            </Link>
          }
        >
          <div className="scroll-x">
            <table>
              <thead>
                <tr>
                  <th>ID</th>
                  <th>Status</th>
                  <th>Image</th>
                  <th>Network</th>
                  <th>Created</th>
                </tr>
              </thead>
              <tbody>
                {o.recent.length === 0 && (
                  <tr>
                    <td colSpan={5} className="muted">
                      No sandboxes yet.
                    </td>
                  </tr>
                )}
                {o.recent.map((s) => (
                  <tr key={s.id}>
                    <td className="wrap-id">
                      <Link to={`/sandboxes/${s.id}`} className="mono">
                        {s.id}
                      </Link>
                    </td>
                    <td>
                      <StatusBadge status={s.status} />
                    </td>
                    <td className="mono wrap-any">{s.image}</td>
                    <td className="mono">{s.egress_mode ?? "—"}</td>
                    <td className="muted small">{fmtTime(s.created_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Section>

        <Section title="Recent activity">
          {!o.users ? (
            <p className="muted">The audit log is visible to admins only.</p>
          ) : (
            o.audit.length === 0 && <p className="muted">No audit entries yet.</p>
          )}
          <ul className="activity">
            {o.audit.map((a, i) => (
              <li key={i}>
                <span className="mono action">{a.action}</span>
                {a.target && <span className="mono"> {a.target}</span>}
                {a.detail && <span className="muted"> — {a.detail}</span>}
                <div className="muted small">
                  {a.actor} · {fmtTime(a.at)}
                </div>
              </li>
            ))}
          </ul>
        </Section>
      </div>
    </>
  );
}
