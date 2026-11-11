import { useEffect, useMemo, useState } from "react";
import { ChevronRight, Container, Copy, Cpu, MemoryStick, Plus, Rocket, Trash2, X } from "lucide-react";
import { toast } from "sonner";
import { Slider } from "@/components/ui/slider";
import { Switch } from "@/components/ui/switch";
import {
  get as apiGet,
  post as apiPost,
  isApprovalPending,
  type NetMode,
  type Policy,
  type Sandbox as ApiSandbox,
} from "@/console/api";
import { type ViewKey } from "@/lib/platform-data";
import { navigate as go } from "@/console/ui";
import { usePlatform } from "@/lib/platform-store";
import { cn } from "@/lib/utils";
import { Mono, PageHeader, Panel, SectionTitle, Segmented, VI } from "../primitives";

/* Create sandbox in the SBX template layout. Every field maps 1:1 onto
   POST /v1/sandboxes; the right column shows the exact request body and,
   after submit, the server's answer. Limits come from GET /v1/policy when
   the caller may read it, else from the conservative defaults below. */

type Phase = "ready" | "sending" | "done" | "failed";

const DEFAULT_LIMITS = { cpus: 4, memMb: 8192, ports: 5 };

const splitList = (s: string) =>
  s
    .split(/[\s,]+/)
    .map((x) => x.trim())
    .filter(Boolean);

export function CreateSandboxView({ onNavigate }: { onNavigate: (v: ViewKey) => void }) {
  const refreshAll = usePlatform((s) => s.refreshAll);

  const [policy, setPolicy] = useState<Policy | null>(null);
  useEffect(() => {
    apiGet<Policy>("/v1/policy")
      .then(setPolicy)
      .catch(() => setPolicy(null));
  }, []);
  const maxCpus = policy?.resources.max_cpus || DEFAULT_LIMITS.cpus;
  const maxMem = policy?.resources.max_memory_mb || DEFAULT_LIMITS.memMb;
  const maxPorts = policy?.resources.max_ports_per_sandbox || DEFAULT_LIMITS.ports;
  const imageHints = (policy?.images.allowed ?? []).filter((i) => !i.includes("*"));

  // basics
  const [project, setProject] = useState("");
  const [image, setImage] = useState("");
  const [command, setCommand] = useState("");
  // resources
  const [cpu, setCpu] = useState(1);
  const [mem, setMem] = useState(1024);
  // network
  const [ports, setPorts] = useState<string[]>([]);
  const [mode, setMode] = useState<NetMode>("allowlist");
  const [allow, setAllow] = useState("");
  const [deny, setDeny] = useState("");
  // env + lifecycle
  const [envVars, setEnvVars] = useState<Array<{ key: string; value: string }>>([]);
  const [idle, setIdle] = useState("");
  const [keepAlive, setKeepAlive] = useState(false);

  const [phase, setPhase] = useState<Phase>("ready");
  const [result, setResult] = useState<string>("");
  const [created, setCreated] = useState<ApiSandbox | null>(null);

  const body = useMemo(() => {
    const portList = ports.map(Number).filter((n) => Number.isInteger(n) && n > 0 && n < 65536);
    const env = Object.fromEntries(envVars.filter((v) => v.key.trim()).map((v) => [v.key.trim(), v.value]));
    const b: Record<string, unknown> = {
      resources: { cpus: Math.min(cpu, maxCpus), memory_mb: Math.min(mem, maxMem) },
      network: {
        egress_mode: mode,
        ...(portList.length ? { ports: portList.map((container) => ({ container })) } : {}),
        ...(mode === "allowlist" && splitList(allow).length ? { egress: splitList(allow) } : {}),
        ...(mode !== "none" && splitList(deny).length ? { egress_deny: splitList(deny) } : {}),
      },
    };
    if (image.trim()) b.image = image.trim();
    if (command.trim()) b.command = command.trim().split(/\s+/);
    if (project.trim()) b.external = { project_id: project.trim() };
    if (Object.keys(env).length) b.env = env;
    if (idle.trim() || keepAlive)
      b.lifecycle = { ...(idle.trim() ? { idle_timeout_secs: Number(idle) } : {}), ...(keepAlive ? { keep_alive: true } : {}) };
    return b;
  }, [project, image, command, cpu, mem, ports, mode, allow, deny, envVars, idle, keepAlive, maxCpus, maxMem]);

  const bodyText = JSON.stringify(body, null, 2);
  const badPort = ports.some((p) => p !== "" && !(Number(p) > 0 && Number(p) < 65536));
  const badIdle = idle.trim() !== "" && !(Number(idle) >= 0);
  const valid = !badPort && !badIdle && ports.length <= maxPorts;

  const submit = async () => {
    if (phase === "sending" || !valid) return;
    setPhase("sending");
    setResult("POST /v1/sandboxes …");
    try {
      const res = await apiPost<ApiSandbox>("/v1/sandboxes", body);
      if (isApprovalPending(res)) {
        setPhase("done");
        setResult(JSON.stringify(res, null, 2));
        toast.info("Waiting for approval", { description: "An admin must approve this request." });
        return;
      }
      setCreated(res);
      setPhase("done");
      setResult(JSON.stringify(res, null, 2));
      toast.success(`${res.id} created`, { description: res.image });
      void refreshAll();
    } catch (e) {
      setPhase("failed");
      setResult(e instanceof Error ? e.message : String(e));
    }
  };

  const phaseMeta: Record<Phase, { label: string; cls: string; dot: string }> = {
    ready: { label: "Not sent", cls: "text-gray-400 border-line bg-navy/40", dot: "bg-gray-500" },
    sending: { label: "Sending…", cls: "text-info border-info/30 bg-info/10", dot: "bg-info" },
    done: { label: created ? created.status : "Accepted", cls: "text-neon border-neon/30 bg-neon/10", dot: "bg-neon" },
    failed: { label: "Refused", cls: "text-danger border-danger/30 bg-danger/10", dot: "bg-danger" },
  };

  const field =
    "h-10 w-full rounded-lg border border-line bg-navy/60 px-3 font-mono text-sm text-gray-100 placeholder:text-gray-600 outline-none focus-visible:ring-2 focus-visible:ring-ring";

  return (
    <div className="space-y-6">
      <PageHeader
        title="Create New Sandbox"
        subtitle="Provision an isolated container with its own limits and egress rules"
        vi="Tạo môi trường sandbox mới"
        breadcrumb={
          <nav className="mb-1.5 flex items-center gap-1.5 text-xs" aria-label="Breadcrumb">
            <button
              onClick={() => onNavigate("sandboxes")}
              className="font-mono text-gray-500 outline-none hover:text-neon focus-visible:ring-2 focus-visible:ring-ring"
            >
              Sandboxes
            </button>
            <ChevronRight className="size-3 text-gray-600" />
            <Mono className="text-gray-300">Create</Mono>
          </nav>
        }
      />

      <div className="grid gap-4 xl:grid-cols-5">
        {/* ══ LEFT — form ═══════════════════════════════════════ */}
        <div className="min-w-0 space-y-4 xl:col-span-3">
          <Panel className="p-6">
            <SectionTitle title="Basic Info" vi="Thông tin cơ bản" right={<Mono className="text-[10px] text-gray-600">01 / 04</Mono>} />
            <div className="mt-5 space-y-4">
              <div className="grid gap-4 md:grid-cols-2">
                <div className="space-y-1.5">
                  <label className="text-xs text-gray-400" htmlFor="cs-project">
                    Project tag <VI className="ml-1">external.project_id</VI>
                  </label>
                  <input id="cs-project" value={project} onChange={(e) => setProject(e.target.value)} placeholder="optional" className={field} />
                </div>
                <div className="space-y-1.5">
                  <label className="text-xs text-gray-400" htmlFor="cs-image">
                    Image
                  </label>
                  <input
                    id="cs-image"
                    list="cs-images"
                    value={image}
                    onChange={(e) => setImage(e.target.value)}
                    placeholder="instance default"
                    className={field}
                  />
                  <datalist id="cs-images">
                    {imageHints.map((i) => (
                      <option key={i} value={i} />
                    ))}
                  </datalist>
                </div>
              </div>
              <div className="space-y-1.5">
                <label className="text-xs text-gray-400" htmlFor="cs-cmd">
                  Command
                </label>
                <input id="cs-cmd" value={command} onChange={(e) => setCommand(e.target.value)} placeholder="image default (e.g. sleep infinity)" className={field} />
              </div>
              {policy && (
                <p className="text-[11px] text-gray-500">
                  Allowed images: <Mono className="text-gray-400">{policy.images.allowed.join(", ") || "instance image only"}</Mono>
                </p>
              )}
            </div>

            <div className="my-6 border-t border-line-soft" />

            <SectionTitle title="Resource Limits" vi="Giới hạn tài nguyên" right={<Mono className="text-[10px] text-gray-600">02 / 04</Mono>} />
            <div className="mt-5 space-y-6">
              <SliderRow icon={<Cpu className="size-4" />} label="CPU" value={Math.min(cpu, maxCpus)} min={1} max={maxCpus} step={1} onChange={setCpu} display={`${Math.min(cpu, maxCpus)} vCPU`} />
              <SliderRow
                icon={<MemoryStick className="size-4" />}
                label="Memory"
                value={Math.min(mem, maxMem)}
                min={256}
                max={maxMem}
                step={256}
                onChange={setMem}
                display={`${Math.min(mem, maxMem)} MiB`}
              />
            </div>

            <div className="my-6 border-t border-line-soft" />

            <SectionTitle title="Network" vi="Mạng & cổng" right={<Mono className="text-[10px] text-gray-600">03 / 04</Mono>} />
            <div className="mt-5 space-y-4">
              <div className="space-y-2">
                <div className="text-xs text-gray-400">Preview ports (container)</div>
                {ports.map((p, i) => (
                  <div key={i} className="flex items-center gap-2">
                    <input
                      value={p}
                      onChange={(e) => setPorts((arr) => arr.map((x, j) => (j === i ? e.target.value.replace(/[^0-9]/g, "") : x)))}
                      placeholder="3000"
                      inputMode="numeric"
                      aria-label={`Port ${i + 1}`}
                      className="h-9 w-full min-w-0 rounded-lg border border-line bg-navy/60 px-3 font-mono text-xs text-gray-200 placeholder:text-gray-600 outline-none focus-visible:ring-2 focus-visible:ring-ring"
                    />
                    <button
                      onClick={() => setPorts((arr) => arr.filter((_, j) => j !== i))}
                      className="grid size-9 shrink-0 place-items-center rounded-lg border border-line text-gray-500 hover:border-danger/40 hover:text-danger"
                      aria-label="Remove port"
                    >
                      <X className="size-4" />
                    </button>
                  </div>
                ))}
                <button
                  onClick={() => setPorts((arr) => [...arr, ""])}
                  disabled={ports.length >= maxPorts}
                  className="inline-flex items-center gap-1.5 rounded-lg border border-dashed border-line px-3 py-2 text-xs text-gray-400 transition-colors hover:border-neon/40 hover:text-neon disabled:opacity-40"
                >
                  <Plus className="size-3.5" /> Add port <span className="text-gray-600">(max {maxPorts})</span>
                </button>
              </div>

              <div className="space-y-2">
                <div className="text-xs text-gray-400">Egress</div>
                <Segmented
                  options={[
                    { value: "none", label: "No internet" },
                    { value: "allowlist", label: "Allow-list" },
                    { value: "open", label: "Open (monitored)" },
                  ]}
                  value={mode}
                  onChange={setMode}
                />
              </div>
              {mode === "allowlist" && (
                <div className="space-y-1.5">
                  <label className="text-xs text-gray-400" htmlFor="cs-allow">
                    Allow hosts <VI className="ml-1">checked against the instance policy</VI>
                  </label>
                  <textarea
                    id="cs-allow"
                    value={allow}
                    onChange={(e) => setAllow(e.target.value)}
                    rows={2}
                    placeholder="pypi.org, github.com"
                    className="w-full resize-none rounded-lg border border-line bg-navy/60 px-3 py-2 font-mono text-xs text-gray-100 placeholder:text-gray-600 outline-none focus-visible:ring-2 focus-visible:ring-ring"
                  />
                </div>
              )}
              {mode !== "none" && (
                <div className="space-y-1.5">
                  <label className="text-xs text-gray-400" htmlFor="cs-deny">
                    Block hosts <VI className="ml-1">always refused</VI>
                  </label>
                  <textarea
                    id="cs-deny"
                    value={deny}
                    onChange={(e) => setDeny(e.target.value)}
                    rows={2}
                    placeholder="optional"
                    className="w-full resize-none rounded-lg border border-line bg-navy/60 px-3 py-2 font-mono text-xs text-gray-100 placeholder:text-gray-600 outline-none focus-visible:ring-2 focus-visible:ring-ring"
                  />
                </div>
              )}
            </div>

            <div className="my-6 border-t border-line-soft" />

            <SectionTitle title="Environment & Lifecycle" vi="Biến môi trường & vòng đời" right={<Mono className="text-[10px] text-gray-600">04 / 04</Mono>} />
            <div className="mt-5 space-y-3">
              {envVars.map((v, i) => (
                <div key={i} className="flex items-center gap-2">
                  <input
                    value={v.key}
                    onChange={(e) => setEnvVars((arr) => arr.map((x, j) => (j === i ? { ...x, key: e.target.value } : x)))}
                    placeholder="KEY"
                    aria-label="Variable name"
                    className="h-9 w-[38%] min-w-0 rounded-lg border border-line bg-navy/60 px-3 font-mono text-xs text-info placeholder:text-gray-600 outline-none focus-visible:ring-2 focus-visible:ring-ring"
                  />
                  <Mono className="text-gray-600">=</Mono>
                  <input
                    value={v.value}
                    onChange={(e) => setEnvVars((arr) => arr.map((x, j) => (j === i ? { ...x, value: e.target.value } : x)))}
                    placeholder="value"
                    aria-label="Variable value"
                    className="h-9 min-w-0 flex-1 rounded-lg border border-line bg-navy/60 px-3 font-mono text-xs text-gray-200 placeholder:text-gray-600 outline-none focus-visible:ring-2 focus-visible:ring-ring"
                  />
                  <button
                    onClick={() => setEnvVars((arr) => arr.filter((_, j) => j !== i))}
                    className="grid size-9 shrink-0 place-items-center rounded-lg border border-line text-gray-500 hover:border-danger/40 hover:text-danger"
                    aria-label="Remove variable"
                  >
                    <Trash2 className="size-4" />
                  </button>
                </div>
              ))}
              <button
                onClick={() => setEnvVars((arr) => [...arr, { key: "", value: "" }])}
                className="inline-flex items-center gap-1.5 rounded-lg border border-dashed border-line px-3 py-2 text-xs text-gray-400 transition-colors hover:border-neon/40 hover:text-neon"
              >
                <Plus className="size-3.5" /> Add variable
              </button>
              <div className="grid gap-4 pt-2 md:grid-cols-2">
                <div className="space-y-1.5">
                  <label className="text-xs text-gray-400" htmlFor="cs-idle">
                    Idle timeout (seconds)
                  </label>
                  <input
                    id="cs-idle"
                    value={idle}
                    onChange={(e) => setIdle(e.target.value.replace(/[^0-9]/g, ""))}
                    inputMode="numeric"
                    placeholder="instance default"
                    className={field}
                  />
                </div>
                <label className="flex cursor-pointer items-center justify-between gap-3 h-10 rounded-lg border border-line bg-navy/40 px-3 md:mt-[22px]">
                  <span className="text-xs text-gray-400">Keep alive</span>
                  <Switch
                    checked={keepAlive}
                    onCheckedChange={setKeepAlive}
                    disabled={policy ? !policy.lifecycle.allow_keep_alive : false}
                    className="data-[state=checked]:bg-neon data-[state=unchecked]:bg-line"
                  />
                </label>
              </div>
            </div>
          </Panel>

          <Panel className="flex flex-col gap-3 p-4 sm:flex-row sm:items-center sm:justify-between">
            <div className="flex items-center gap-2.5 text-xs text-gray-500">
              <span className={cn("size-1.5 rounded-full", phaseMeta[phase].dot)} />
              <Mono>{!valid ? "check the highlighted fields" : phase === "done" && created ? `created ${created.id}` : phase === "failed" ? "refused — see the response" : "ready"}</Mono>
            </div>
            <div className="flex flex-wrap items-center gap-2.5">
              {created && (
                <button
                  onClick={() => {
                    go(`/sandboxes/${created.id}`);
                  }}
                  className="inline-flex items-center gap-2 rounded-lg border border-neon/40 bg-neon/10 px-4 py-2.5 text-sm font-medium text-neon transition-colors hover:bg-neon/20 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                >
                  <Container className="size-4" /> Open {created.id}
                </button>
              )}
              <button
                onClick={submit}
                disabled={phase === "sending" || !valid}
                className="inline-flex items-center gap-2 rounded-lg bg-neon px-6 py-3 text-sm font-bold text-navy transition-all hover:bg-neon/90 hover:shadow-[0_0_28px_var(--glow-mid)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:pointer-events-none disabled:opacity-50"
              >
                <Rocket className="size-4" />
                {phase === "sending" ? "Creating…" : "Create sandbox"}
              </button>
            </div>
          </Panel>
        </div>

        {/* ══ RIGHT — request / response ════════════════════════ */}
        <div className="min-w-0 space-y-4 xl:col-span-2">
          <Panel className="overflow-hidden">
            <div className="flex items-center justify-between border-b border-line px-5 py-4">
              <SectionTitle title="Request" vi="Nội dung gửi lên API" />
              <span className={cn("inline-flex items-center gap-1.5 rounded-md border px-2 py-1 text-[11px] font-medium", phaseMeta[phase].cls)}>
                <span className={cn("size-1.5 rounded-full", phaseMeta[phase].dot)} />
                {phaseMeta[phase].label}
              </span>
            </div>
            <div className="bg-sunken px-5 py-4">
              <div className="mb-2.5 flex items-center gap-2">
                <Mono className="text-[10px] text-gray-500">POST /v1/sandboxes</Mono>
                <button
                  onClick={() =>
                    navigator.clipboard?.writeText(bodyText).then(
                      () => toast.success("Request body copied"),
                      () => toast.error("Clipboard unavailable"),
                    )
                  }
                  className="ml-auto grid size-6 place-items-center rounded text-gray-600 hover:text-gray-300"
                  aria-label="Copy request body"
                >
                  <Copy className="size-3.5" />
                </button>
              </div>
              <pre className="max-h-72 overflow-auto whitespace-pre-wrap rounded-lg border border-line/60 bg-navy/70 p-3 font-mono text-[11.5px] leading-relaxed text-gray-300 [overflow-wrap:anywhere]">
                {bodyText}
              </pre>
            </div>
          </Panel>

          {result && (
            <Panel className="overflow-hidden">
              <div className="border-b border-line px-5 py-4">
                <SectionTitle title="Response" vi="Phản hồi" />
              </div>
              <pre
                className={cn(
                  "max-h-80 overflow-auto whitespace-pre-wrap bg-sunken p-5 font-mono text-[11.5px] leading-relaxed [overflow-wrap:anywhere]",
                  phase === "failed" ? "text-danger" : "text-gray-300",
                )}
              >
                {result}
              </pre>
            </Panel>
          )}
        </div>
      </div>
    </div>
  );
}

/* ── subcomponents ────────────────────────────────────────────── */
function SliderRow({
  icon,
  label,
  value,
  min,
  max,
  step,
  onChange,
  display,
}: {
  icon: React.ReactNode;
  label: string;
  value: number;
  min: number;
  max: number;
  step: number;
  onChange: (v: number) => void;
  display: string;
}) {
  return (
    <div className="grid items-center gap-3 md:grid-cols-[140px_1fr_110px]">
      <div className="flex items-center gap-2.5 text-xs text-gray-400">
        <span className="grid size-7 place-items-center rounded-md border border-line bg-navy/60 text-gray-500">{icon}</span>
        {label}
      </div>
      <Slider value={[value]} min={min} max={max} step={step} onValueChange={(v) => onChange(v[0])} aria-label={label} />
      <div className="text-right">
        <Mono className="text-sm font-semibold text-neon">{display}</Mono>
        <div className="font-mono text-[9.5px] text-gray-600">
          {min}–{max}
        </div>
      </div>
    </div>
  );
}
