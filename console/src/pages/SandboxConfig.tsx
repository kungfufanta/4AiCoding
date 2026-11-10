import { useState } from "react";
import { get, post, type NetMode, type Policy, type Sandbox } from "../api";
import { Curl, LinesInput, navigate, Section, useAction, useLoad } from "../ui";

// One form field per sandboxspec field (PLAN §6): what you fill in here is
// exactly the POST /v1/sandboxes body shown in the curl preview.
export function SandboxConfigPage() {
  const { data: policy } = useLoad(() => get<Policy>("/v1/policy"));
  const [image, setImage] = useState("");
  const [command, setCommand] = useState("");
  const [env, setEnv] = useState<string[]>([]);
  const [cpus, setCpus] = useState("");
  const [memory, setMemory] = useState("");
  const [ports, setPorts] = useState("3000");
  const [visibility, setVisibility] = useState("private");
  const [repo, setRepo] = useState("");
  const [branch, setBranch] = useState("");
  const [netMode, setNetMode] = useState<NetMode>("allowlist");
  const [egress, setEgress] = useState<string[]>([]);
  const [deny, setDeny] = useState<string[]>([]);
  const [idle, setIdle] = useState("");
  const [keepAlive, setKeepAlive] = useState(false);
  const { run, busy } = useAction();

  const body: Record<string, unknown> = {};
  if (image) body.image = image;
  if (command.trim()) body.command = ["sh", "-c", command.trim()];
  if (env.length) body.env = Object.fromEntries(env.map((l) => [l.split("=")[0], l.slice(l.indexOf("=") + 1)]));
  if (cpus || memory) body.resources = { cpus: Number(cpus) || 0, memory_mb: Number(memory) || 0 };
  const portList = ports
    .split(/[\s,]+/)
    .filter(Boolean)
    .map(Number);
  if (portList.length || egress.length || deny.length || netMode !== "allowlist")
    body.network = {
      ...(portList.length ? { ports: portList.map((container) => ({ container })) } : {}),
      ...(netMode !== "allowlist" ? { egress_mode: netMode } : {}),
      ...(egress.length ? { egress } : {}),
      ...(deny.length ? { egress_deny: deny } : {}),
    };
  if (visibility !== "private") body.visibility = visibility;
  if (repo) body.workspace = { repo_url: repo, ...(branch ? { branch } : {}) };
  if (idle || keepAlive) body.lifecycle = { idle_timeout_secs: Number(idle) || 0, keep_alive: keepAlive };

  const envBad = env.find((l) => !/^[A-Za-z_][A-Za-z0-9_]*=/.test(l));
  const portsBad = portList.some((p) => !Number.isInteger(p) || p < 1 || p > 65535);

  const create = async (e: React.FormEvent) => {
    e.preventDefault();
    const sb = await run(() => post<Sandbox>("/v1/sandboxes", body), "Sandbox created");
    if (sb && "id" in sb) navigate(`/sandboxes/${sb.id}`);
  };

  return (
    <Section title="New sandbox">
      <form className="form" onSubmit={create}>
        <label>
          Image
          <select value={image} onChange={(e) => setImage(e.target.value)}>
            <option value="">Instance default image</option>
            {policy?.images.allowed.map((i) => (
              <option key={i} value={i}>
                {i}
              </option>
            ))}
          </select>
          <small>Only the instance image and images on the policy allowlist can be used.</small>
        </label>
        <label>
          Command
          <input className="mono" placeholder="image default (e.g. npm run dev)" value={command} onChange={(e) => setCommand(e.target.value)} />
          <small>Run through <code>sh -c</code>. Leave empty to keep the image's CMD.</small>
        </label>
        <label>
          Environment
          <LinesInput value={env} onChange={setEnv} placeholder="KEY=value, one per line" />
          {envBad && <small className="error">Invalid line: {envBad}</small>}
          <small>Never put secrets here: everything in a sandbox is treated as untrusted.</small>
        </label>
        <div className="cols">
          <label>
            CPUs
            <input type="number" step="0.1" min="0" placeholder={policy?.resources.max_cpus ? `max ${policy.resources.max_cpus}` : "no cap"} value={cpus} onChange={(e) => setCpus(e.target.value)} />
          </label>
          <label>
            Memory (MiB)
            <input type="number" min="0" placeholder={policy ? `max ${policy.resources.max_memory_mb}` : ""} value={memory} onChange={(e) => setMemory(e.target.value)} />
          </label>
        </div>
        <small>Values above the policy maximum are clamped, not rejected.</small>
        <div className="cols">
          <label>
            Preview ports
            <input className="mono" placeholder="3000, 8080" value={ports} onChange={(e) => setPorts(e.target.value)} />
            {portsBad && <small className="error">Ports must be 1–65535.</small>}
          </label>
          <label>
            Preview visibility
            <select value={visibility} onChange={(e) => setVisibility(e.target.value)}>
              <option value="private">private (signed-in users)</option>
              <option value="public">public (may need approval)</option>
            </select>
          </label>
        </div>
        <div className="cols">
          <label>
            Git repository
            <input placeholder="https://github.com/org/repo.git" value={repo} onChange={(e) => setRepo(e.target.value)} />
          </label>
          <label>
            Branch
            <input placeholder="default branch" value={branch} onChange={(e) => setBranch(e.target.value)} />
          </label>
        </div>
        <label>
          Network
          <select value={netMode} onChange={(e) => setNetMode(e.target.value as NetMode)}>
            <option value="none">No network: nothing leaves the sandbox</option>
            <option value="allowlist">Allow list: only the hosts below</option>
            <option value="open">Open, monitored: everything except the block list</option>
          </select>
          <small>
            Policy mode: {policy?.egress.mode ?? "…"}
            {policy?.egress.mode === "strict" && policy.egress.allow.length > 0 && ` (allowed: ${policy.egress.allow.join(", ")})`}
            {policy?.egress.mode === "strict" && netMode === "open" && ". Open needs a permissive policy."}
            {" "}Every request is visible live on the sandbox's Network page.
          </small>
        </label>
        <div className="cols">
          <label>
            Allow list
            <LinesInput value={egress} onChange={setEgress} placeholder={"github.com\n*.npmjs.org"} rows={3} />
            <small>Used in allow-list mode. example.com includes its subdomains.</small>
          </label>
          <label>
            Block list
            <LinesInput value={deny} onChange={setDeny} placeholder={"tracker.example\n10.0.0.0/8"} rows={3} />
            <small>Always blocked: hosts, IPs or CIDR ranges.</small>
          </label>
        </div>
        <div className="cols">
          <label>
            Idle timeout (seconds)
            <input type="number" min="0" placeholder="instance default" value={idle} onChange={(e) => setIdle(e.target.value)} />
          </label>
          <label className="check">
            <input type="checkbox" checked={keepAlive} disabled={policy ? !policy.lifecycle.allow_keep_alive : false} onChange={(e) => setKeepAlive(e.target.checked)} />
            Keep alive (never idle-stopped){policy && !policy.lifecycle.allow_keep_alive && " — disabled by policy"}
          </label>
        </div>
        <div className="row">
          <button type="submit" disabled={busy || !!envBad || portsBad}>
            Create sandbox
          </button>
          <button type="button" className="ghost" onClick={() => navigate("/")}>
            Cancel
          </button>
        </div>
        <Curl method="POST" path="/v1/sandboxes" body={body} />
      </form>
    </Section>
  );
}
