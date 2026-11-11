// Thin client for the sandboxd /v1 API (docs/openapi.yaml). The console is
// served from the same origin as the API, so the HttpOnly session cookie
// travels automatically; every unsafe request also carries the CSRF header
// the middleware requires for cookie-authenticated calls.

export const CSRF_HEADER = "X-Sandboxd-CSRF";
export const APPROVAL_HEADER = "X-Sandboxd-Approval";

export type Role = "admin" | "operator" | "viewer";

export class ApiError extends Error {
  status: number;
  code: string;
  constructor(status: number, code: string, message: string) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

/** A gated request was parked for an admin's approval (HTTP 202). */
export interface ApprovalPending {
  approval_id: string;
  status: "pending";
  action: string;
  target: string;
  message: string;
}

export function isApprovalPending(v: unknown): v is ApprovalPending {
  return typeof v === "object" && v !== null && "approval_id" in v && (v as ApprovalPending).status === "pending";
}

const SAFE = new Set(["GET", "HEAD", "OPTIONS"]);

export async function api<T = unknown>(
  method: string,
  path: string,
  body?: unknown,
  headers: Record<string, string> = {},
): Promise<T> {
  const h: Record<string, string> = { ...headers };
  if (!SAFE.has(method)) h[CSRF_HEADER] = "1";
  if (body !== undefined) h["Content-Type"] = "application/json";
  const res = await fetch(path, {
    method,
    headers: h,
    credentials: "same-origin",
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (res.status === 204) return undefined as T;
  const text = await res.text();
  let data: unknown = undefined;
  try {
    data = text ? JSON.parse(text) : undefined;
  } catch {
    data = text;
  }
  if (!res.ok) {
    const err = (data as { error?: { code?: string; message?: string } | string })?.error;
    const code = typeof err === "object" ? err?.code ?? "error" : "error";
    const msg = typeof err === "object" ? err?.message ?? res.statusText : typeof err === "string" ? err : res.statusText;
    throw new ApiError(res.status, code, msg);
  }
  return data as T;
}

export const get = <T>(p: string) => api<T>("GET", p);
export const post = <T>(p: string, b?: unknown, h?: Record<string, string>) => api<T>("POST", p, b, h);
export const put = <T>(p: string, b?: unknown, h?: Record<string, string>) => api<T>("PUT", p, b, h);
export const patch = <T>(p: string, b?: unknown) => api<T>("PATCH", p, b);
export const del = <T>(p: string, h?: Record<string, string>) => api<T>("DELETE", p, undefined, h);

// --- types (subset of docs/openapi.yaml) ---------------------------------

export interface Sandbox {
  id: string;
  status: "creating" | "running" | "stopped" | "error" | string;
  image: string;
  command?: string[];
  env?: Record<string, string>;
  resources: { cpus: number; memory_mb: number };
  lifecycle: { idle_timeout_secs: number; keep_alive: boolean };
  egress: string[];
  egress_mode?: NetMode;
  egress_deny?: string[];
  ports: number[];
  preview?: { url: string; port?: number; status: string };
  visibility: string;
  idle_policy: string;
  node?: string;
  external?: { user_id?: string; project_id?: string };
  error_message?: string;
  last_active_at?: string;
  created_at: string;
}

/** Per-sandbox network mode: nothing out, only the allow list, or everything but the block list. */
export type NetMode = "none" | "allowlist" | "open";

export interface EgressRules {
  mode: NetMode;
  allow: string[];
  deny: string[];
}

export interface NetEvent {
  seq: number;
  time: string;
  method: string;
  host: string;
  port: string;
  ip?: string;
  allowed: boolean;
  reason?: string;
}

export interface NetHost {
  host: string;
  ips: string[];
  ports: string[];
  allowed: number;
  denied: number;
  bytes_up: number;
  bytes_down: number;
  first_seen: string;
  last_seen: string;
  last_allowed: boolean;
  last_reason?: string;
}

export interface NetActivity {
  sandbox_id: string;
  recording: boolean;
  rules: EgressRules;
  cursor: string;
  events: NetEvent[];
  hosts: NetHost[];
}

/** project_id that marks the one-click default sandbox. */
export const DEFAULT_SANDBOX_TAG = "default";

export interface AuthStatus {
  enabled: boolean;
  authenticated: boolean;
  setup_needed: boolean;
}

export interface Me {
  kind: "user" | "service" | string;
  user: string;
  role: Role;
}

export interface Policy {
  images: { allowed: string[] };
  ports: { ranges: { min: number; max: number }[] };
  resources: { max_cpus: number; max_memory_mb: number; max_ports_per_sandbox: number };
  egress: { mode: "strict" | "permissive"; allow: string[] };
  lifecycle: { allow_keep_alive: boolean; max_idle_timeout_secs: number };
  approvals: string[];
}

export interface Approval {
  id: string;
  action: string;
  target: string;
  payload: string;
  status: string;
  requested_by: string;
  decided_by?: string;
  reason?: string;
  created_at: number;
  expires_at: number;
}

export interface User {
  id: string;
  username: string;
  role: Role;
  disabled: boolean;
  created_at: string;
}

export interface ApiKey {
  id: string;
  name: string;
  prefix: string;
  role?: Role;
  scopes?: string[];
  expires_at?: string;
  sandbox_id?: string;
  created_at: string;
  last_used_at?: string;
}

/** Fleet membership (M6). role 'local' = the control plane itself. */
export interface NodeAmount {
  cpus: number;
  memory_mb: number;
  disk_mb: number;
}

export interface NodeUsage extends NodeAmount {
  sandboxes: number;
}

export interface NodeInfo {
  node_id: string;
  role: "local" | "worker" | string;
  status: "online" | "offline" | string;
  drain_state: "active" | "drain" | string;
  address?: string;
  version?: string;
  capacity: NodeAmount;
  used: NodeUsage;
  sandboxes: number;
  registered_at: string;
  heartbeat_at: string;
  /** Latest host sample the node sent with its heartbeat (absent = never reported). */
  telemetry?: HostTelemetry;
  /** Values file a capacity change request edits (absent = no GitOps mapping). */
  change_file?: string;
}

/** Host telemetry: the machine a node runs on (VPS, Docker host or k3s node). */
export interface HostTelemetry {
  collected_at: string;
  hostname?: string;
  os?: string;
  arch?: string;
  kernel?: string;
  uptime_sec?: number;
  in_container?: boolean;
  cpu: { cores: number; usage_percent: number; load1: number; load5: number; load15: number };
  memory: { total_mb: number; used_mb: number; available_mb: number; swap_total_mb?: number; swap_used_mb?: number };
  disk: { path?: string; total_mb: number; used_mb: number; free_mb: number };
  network: { interfaces?: string[]; rx_bytes: number; tx_bytes: number; rx_bps: number; tx_bps: number; host_namespace: boolean };
}

export interface NodeMetricPoint {
  t: string;
  cpu_percent: number;
  load1: number;
  mem_used_mb: number;
  mem_total_mb: number;
  disk_used_mb: number;
  disk_total_mb: number;
  rx_bps: number;
  tx_bps: number;
}

export interface NodeMetrics {
  node: NodeInfo;
  history: NodeMetricPoint[];
  step: string;
  window: string;
  now: string;
}

export interface NodeChangePR {
  number: number;
  url: string;
  title: string;
  state: "open" | "closed" | "merged" | string;
  branch: string;
  created_at?: string;
  merged_at?: string;
}

export interface NodeChange {
  node_id: string;
  dry_run: boolean;
  repo?: string;
  base: string;
  file: string;
  current: NodeAmount;
  proposed: NodeAmount;
  diff: string[];
  content?: string;
  can_open_pr: boolean;
  pr?: NodeChangePR;
}

export interface NodeChangeList {
  node_id: string;
  repo?: string;
  base?: string;
  file: string;
  can_open_pr: boolean;
  /** Capacity in the values file on the base branch. */
  current?: NodeAmount;
  error?: string;
  requests: NodeChangePR[];
}

export interface AuditEntry {
  id: number;
  at: number;
  actor_kind: string;
  actor_name?: string;
  action: string;
  target?: string;
  detail?: string;
}

export interface DomainInfo {
  domain: string;
  tls_mode: string;
  acme_email?: string;
  running: { domain: string; tls_mode: string };
  restart_required: boolean;
  console_host: string;
  preview_example: string;
}

/** Subscribes to a Server-Sent Events endpoint; returns a stop function. */
export function sse(path: string, onData: (line: string) => void, onEnd?: () => void): () => void {
  const es = new EventSource(path, { withCredentials: true });
  es.onmessage = (e) => onData(e.data);
  es.addEventListener("end", () => {
    es.close();
    onEnd?.();
  });
  es.onerror = () => {
    es.close();
    onEnd?.();
  };
  return () => es.close();
}

// --- template views (merged from the SBX Console template) -----------------

export interface Overview {
  sandboxes: { total: number; running: number; stopped: number; creating: number; error: number };
  users: { total: number; admins: number; operators: number; viewers: number; disabled: number };
  ports_exposed: number;
  approvals_open: number;
  net_events_24h: { total: number; blocked: number };
  recent: Sandbox[];
  audit: { at: string; actor: string; action: string; target?: string; detail?: string }[];
}

/** One user × sandbox cell of the R/W/X matrix. W/X imply R (API enforces). */
export interface PermGrant {
  user_id: string;
  sandbox_id: string;
  r: boolean;
  w: boolean;
  x: boolean;
}

export interface PermMatrix {
  users: { id: string; username: string; role: Role; disabled: boolean }[];
  sandboxes: { id: string; status: string; image: string }[];
  grants: PermGrant[];
}

export interface Monitoring {
  fleet: { total: number; running: number; avg_cpu: number; mem_used_mb: number; mem_limit_mb: number };
  sandboxes: { id: string; cpu_percent: number; mem_usage_bytes: number; mem_limit_bytes: number; pids: number }[];
  history: { t: string; cpu: number; mem_mb: number }[];
}

export const getOverview = () => get<Overview>("/v1/overview");
export const getPermissions = () => get<PermMatrix>("/v1/permissions");
export const putPermissions = (grants: PermGrant[]) =>
  put<{ ok: boolean; grants: number }>("/v1/permissions", { grants });
export type Range = "8m" | "1h" | "6h" | "24h";
export const getMonitoring = (range: Range = "8m") =>
  get<Monitoring>(range === "8m" ? "/v1/monitoring" : `/v1/monitoring?range=${range}`);
