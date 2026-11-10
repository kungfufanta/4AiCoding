import { useEffect, useState } from "react";
import { APPROVAL_HEADER, del, get, post, put, type Policy } from "../api";
import { can, Curl, LinesInput, Link, Loading, Section, useAction, useLoad, useMe } from "../ui";

interface PortsResp {
  ports: { container: number; preview_url: string }[];
}
interface EgressResp {
  allow: string[];
  proxy: string;
}

export function NetworkPage({ id }: { id: string }) {
  const me = useMe();
  const write = can(me, "operator");
  const ports = useLoad(() => get<PortsResp>(`/v1/sandboxes/${id}/network/ports`), [id]);
  const egress = useLoad(() => get<EgressResp>(`/v1/sandboxes/${id}/network/egress`), [id]);
  const { data: policy } = useLoad(() => get<Policy>("/v1/policy"));
  const { run, busy } = useAction();
  const [newPort, setNewPort] = useState("");
  const [allow, setAllow] = useState<string[]>([]);
  const [approval, setApproval] = useState("");

  useEffect(() => {
    if (egress.data) setAllow(egress.data.allow);
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
  const saveEgress = async () => {
    const h: Record<string, string> = approval ? { [APPROVAL_HEADER]: approval } : {};
    await run(() => put(`/v1/sandboxes/${id}/network/egress`, { allow }, h), "Egress allowlist saved");
    egress.reload();
  };

  return (
    <>
      <p>
        <Link to={`/sandboxes/${id}`}>← Sandbox {id}</Link>
      </p>
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

      <Section title="Egress allowlist">
        {!egress.data ? (
          <Loading error={egress.error} />
        ) : (
          <>
            <p className="muted">
              Outbound traffic goes only through the egress proxy{egress.data.proxy ? ` (${egress.data.proxy})` : ""}. Only these hosts are
              reachable; an empty list means no internet. <code>example.com</code> also allows its subdomains, <code>*.example.com</code> only
              subdomains. Cloud metadata and loopback are always blocked.
            </p>
            {policy && (
              <p className="small">
                Policy: <b>{policy.egress.mode}</b>
                {policy.egress.mode === "strict" && ` — allowed: ${policy.egress.allow.length ? policy.egress.allow.join(", ") : "nothing"}`}
                {policy.approvals.includes("sandbox.egress_outside_policy") && "; hosts outside it need an admin's approval"}
              </p>
            )}
            <LinesInput value={allow} onChange={setAllow} placeholder={"github.com\n*.npmjs.org"} rows={6} />
            {write && (
              <div className="row">
                <input placeholder="Approval ID (optional)" value={approval} onChange={(e) => setApproval(e.target.value.trim())} />
                <button disabled={busy} onClick={saveEgress}>
                  Save allowlist
                </button>
              </div>
            )}
            <Curl method="PUT" path={`/v1/sandboxes/${id}/network/egress`} body={{ allow }} />
          </>
        )}
      </Section>
    </>
  );
}
