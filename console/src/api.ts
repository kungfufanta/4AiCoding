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
  ports: number[];
  preview?: { url: string; port?: number; status: string };
  visibility: string;
  idle_policy: string;
  error_message?: string;
  last_active_at?: string;
  created_at: string;
}

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
