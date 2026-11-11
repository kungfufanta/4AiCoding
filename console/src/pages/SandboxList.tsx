import { useState } from "react";
import { ApiError, DEFAULT_SANDBOX_TAG, del, get, post, put, type Policy, type Sandbox } from "../api";
import { can, Curl, fmtTime, Link, Loading, navigate, Section, StatusBadge, useAction, useLoad, useMe, useNotify } from "../ui";

/** The one-click default sandbox: open network (monitored), preview on 3000. */
export const DEFAULT_SANDBOX_BODY = {
  network: { ports: [{ container: 3000 }], egress_mode: "open" },
  external: { project_id: DEFAULT_SANDBOX_TAG },
};

export function SandboxListPage() {
  const me = useMe();
  const { data, error, reload } = useLoad(() => get<{ sandboxes: Sandbox[] }>("/v1/sandboxes"));
  const { run, busy } = useAction();
  const write = can(me, "operator");
  const notify = useNotify();
  const [opening, setOpening] = useState(false);

  // Opens the default sandbox, creating it first if there is none. When the
  // egress policy is strict, an admin is offered to make it permissive.
  const openDefault = async () => {
    setOpening(true);
    try {
      const existing = data?.sandboxes.find((s) => s.external?.project_id === DEFAULT_SANDBOX_TAG);
      if (existing) {
        if (existing.status === "stopped") await post(`/v1/sandboxes/${existing.id}/start`);
        navigate(`/sandboxes/${existing.id}/network`);
        return;
      }
      let sb: Sandbox | undefined;
      try {
        sb = await post<Sandbox>("/v1/sandboxes", DEFAULT_SANDBOX_BODY);
      } catch (e) {
        const policyBlocks = e instanceof ApiError && e.message.includes("permissive");
        if (!policyBlocks || !can(me, "admin")) throw e;
        if (!confirm("The default sandbox has open (monitored) network access, which the strict egress policy forbids. Switch the egress policy to permissive?")) return;
        const pol = await get<Policy>("/v1/policy");
        await put("/v1/policy/egress", { ...pol.egress, mode: "permissive" });
        sb = await post<Sandbox>("/v1/sandboxes", DEFAULT_SANDBOX_BODY);
      }
      if (sb && "id" in sb) {
        notify({ kind: "ok", text: "Default sandbox created" });
        navigate(`/sandboxes/${sb.id}/network`);
      } else {
        notify({ kind: "info", text: "The request needs an admin's approval first." });
      }
    } catch (e) {
      notify({ kind: "err", text: e instanceof ApiError ? `${e.message} (${e.code})` : String(e) });
    } finally {
      setOpening(false);
    }
  };

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
          {write && (
            <button className="ghost" disabled={opening || !data} onClick={openDefault} title="Open network, monitored; preview on port 3000">
              Default sandbox
            </button>
          )}
          {write && <button onClick={() => navigate("/sandboxes/new")}>New sandbox</button>}
        </>
      }
    >
      {!data ? (
        <Loading error={error} />
      ) : data.sandboxes.length === 0 ? (
        <p className="muted">
          No sandboxes yet. {write && "Default sandbox creates one with open, monitored network access in one click."}
        </p>
      ) : (
        <table>
          <thead>
            <tr>
              <th>ID</th>
              <th>Status</th>
              <th>Image</th>
              <th>Network</th>
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
                  {sb.external?.project_id === DEFAULT_SANDBOX_TAG && <span className="badge">default</span>}
                  {sb.lifecycle?.keep_alive && <span className="badge">keep-alive</span>}
                </td>
                <td>
                  <StatusBadge status={sb.status} />
                </td>
                <td className="mono small">{sb.image}</td>
                <td className="small">
                  <Link to={`/sandboxes/${sb.id}/network`}>{sb.egress_mode ?? "allowlist"}</Link>
                </td>
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
                <td className="actions">
                  <div className="row right">
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
                  </div>
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
