import { del, get, post, type Sandbox } from "../api";
import { can, Curl, fmtTime, Link, Loading, navigate, Section, StatusBadge, useAction, useLoad, useMe } from "../ui";

export function SandboxListPage() {
  const me = useMe();
  const { data, error, reload } = useLoad(() => get<{ sandboxes: Sandbox[] }>("/v1/sandboxes"));
  const { run, busy } = useAction();
  const write = can(me, "operator");

  const act = async (sb: Sandbox, verb: "start" | "stop" | "delete") => {
    if (verb === "delete" && !confirm(`Delete sandbox ${sb.id}? Its workspace is removed too.`)) return;
    await run(
      () => (verb === "delete" ? del(`/v1/sandboxes/${sb.id}`) : post(`/v1/sandboxes/${sb.id}/${verb}`)),
      verb === "delete" ? "Sandbox deleted" : `Sandbox ${verb === "start" ? "started" : "stopped"}`,
    );
    reload();
  };

  return (
    <Section
      title="Sandboxes"
      actions={
        <>
          <button className="ghost" onClick={reload}>
            Refresh
          </button>
          {write && <button onClick={() => navigate("/sandboxes/new")}>New sandbox</button>}
        </>
      }
    >
      {!data ? (
        <Loading error={error} />
      ) : data.sandboxes.length === 0 ? (
        <p className="muted">No sandboxes yet.</p>
      ) : (
        <table>
          <thead>
            <tr>
              <th>ID</th>
              <th>Status</th>
              <th>Image</th>
              <th>Preview</th>
              <th>Last active</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {data.sandboxes.map((sb) => (
              <tr key={sb.id}>
                <td>
                  <Link to={`/sandboxes/${sb.id}`} className="mono">
                    {sb.id}
                  </Link>
                  {sb.lifecycle?.keep_alive && <span className="badge">keep-alive</span>}
                </td>
                <td>
                  <StatusBadge status={sb.status} />
                </td>
                <td className="mono small">{sb.image}</td>
                <td>
                  {sb.preview ? (
                    <a href={sb.preview.url} target="_blank" rel="noreferrer">
                      :{sb.preview.port}
                    </a>
                  ) : (
                    "—"
                  )}
                </td>
                <td className="small">{fmtTime(sb.last_active_at)}</td>
                <td className="row right">
                  {write && sb.status === "stopped" && (
                    <button className="ghost" disabled={busy} onClick={() => act(sb, "start")}>
                      Start
                    </button>
                  )}
                  {write && sb.status === "running" && (
                    <button className="ghost" disabled={busy} onClick={() => act(sb, "stop")}>
                      Stop
                    </button>
                  )}
                  {write && (
                    <button className="danger ghost" disabled={busy} onClick={() => act(sb, "delete")}>
                      Delete
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <Curl method="GET" path="/v1/sandboxes" />
    </Section>
  );
}
