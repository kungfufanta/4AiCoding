/**
 * TypeScript types for the Portal /v1 SDK.
 *
 * Mirrors portal/openapi.yaml — every interface corresponds to a schema
 * or a request/response shape. Cross-runtime compatible (Node / Deno /
 * Bun / browser) — no Node-specific imports here.
 */

export type SandboxStatus =
  | "pending"
  | "created"
  | "starting"
  | "running"
  | "stopped"
  | "exited"
  | "failed";

export interface SandboxResources {
  cpus?: number;
  memory_mb?: number;
}

export interface SandboxPreview {
  url?: string;
  port?: number;
  status?: string;
}

export interface Sandbox {
  id: string;
  status: SandboxStatus;
  image: string;
  resources?: SandboxResources;
  ports?: number[];
  egress?: string[];
  egress_mode?: string;
  preview?: SandboxPreview;
  project_id?: string | null;
  created_at?: string;
  updated_at?: string;
}

export interface ExecResult {
  stdout: string;
  stderr: string;
  exit_code: number;
  timed_out: boolean;
  truncated: boolean;
  duration_ms: number;
}

/** One NDJSON line from `?stream=ndjson` exec. Either a chunk or
 *  the terminal exit summary. */
export interface ExecStreamChunk {
  stream?: "stdout" | "stderr";
  data?: string;
  exit_code?: number;
  timed_out?: boolean;
  duration_ms?: number;
}

export interface FileEntry {
  path: string;
  name?: string;
  size?: number;
  mode?: string;
  is_dir?: boolean;
}

export interface FileListing {
  path: string;
  recursive: boolean;
  entries: FileEntry[];
  truncated: boolean;
}

export interface FileContent {
  path: string;
  size: number;
  offset: number;
  encoding: "utf-8" | "base64";
  data: string;
  truncated: boolean;
}

export interface FileWriteResult {
  path: string;
  size: number;
  sha256: string;
}

export interface UsagePoint {
  day: string;
  kind: "sandbox_seconds" | "vcpu_seconds";
  quantity: number;
}

export interface PlanUsage {
  plan_id: string;
  plan_name: string;
  monthly_vcpu_seconds: number;
  used_vcpu_seconds: number;
  max_concurrent_sandboxes: number;
  running_sandboxes: number;
}

export interface UsageResponse {
  from: string;
  to: string;
  kind: string;
  series: UsagePoint[];
  plan?: PlanUsage;
}

/** The `error` object embedded in a Portal error response. */
export interface PortalErrorDetail {
  code: string;
  message: string;
  /** Present on 422 plan_limit. */
  field?: string;
  /** Present on 422 plan_limit. */
  limit?: unknown;
  /** Present on 422 plan_limit. */
  used?: unknown;
}

// ---------------------------------------------------------------------------
// Request option bags
// ---------------------------------------------------------------------------

export interface CreateSandboxOptions {
  image: string;
  project_id?: string;
  command?: string[];
  env?: Record<string, string>;
  resources?: { cpus?: number; memory_mb?: number };
  ports?: number[];
  egress?: { mode?: "none" | "allowlist" | "open"; allow?: string[] };
  workspace?: { repo_url?: string; branch?: string };
  ttl_seconds?: number;
  /** Auto-generated UUID4 if omitted (review #47 contract). */
  idempotency_key?: string;
}

export interface ExecOptions {
  cmd: string[];
  cwd?: string;
  env?: Record<string, string>;
  /** Plain string, NOT base64 (review #47 contract). */
  stdin?: string;
  timeout_s?: number;
  max_output_bytes?: number;
  /** Auto-generated UUID4 if omitted (review #47 contract). */
  idempotency_key?: string;
}

export interface ListFilesOptions {
  path?: string;
  recursive?: boolean;
  max_entries?: number;
}

export interface ReadFileOptions {
  path: string;
  offset?: number;
  limit?: number;
}

export interface WriteFileOptions {
  path: string;
  /** Raw bytes. Strings are UTF-8 encoded. Sent as
   *  `application/octet-stream` (NOT JSON-wrapped). */
  data: Uint8Array | string;
  /** sha256 of the content the caller last saw (optimistic concurrency). */
  if_match?: string;
}

export interface LogsOptions {
  tail?: number;
  follow?: boolean;
}

export interface UsageOptions {
  /** ISO-8601 datetime. */
  from?: string;
  to?: string;
  kind?: "sandbox_seconds" | "vcpu_seconds";
}

export interface CreateTokenOptions {
  scopes: string[];
  ttl_seconds?: number;
  agent_run_id?: string;
}

export interface PortalClientOptions {
  baseURL: string;
  apiKey: string;
  /** Per-request socket timeout in seconds (default 30). */
  timeout?: number;
}
