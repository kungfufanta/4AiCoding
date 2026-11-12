/**
 * PortalClient — thin Web-fetch-based client for the Portal /v1 API.
 *
 * Cross-runtime (Node 18+ / Deno / Bun / browser). No external deps.
 * Auth: Bearer Portal API key (`sbx_live_...`) or agent token.
 *
 * Idempotency: `createSandbox` and `exec` auto-inject an
 * `Idempotency-Key` (UUID4) header when the caller doesn't pass one.
 * Retry: GET requests are retried once on HTTP 502/503/504.
 */

import { execStream, logsFollow } from "./stream.js";
import { PortalError } from "./errors.js";
import type {
  CreateSandboxOptions,
  CreateTokenOptions,
  ExecOptions,
  ExecResult,
  ExecStreamChunk,
  FileContent,
  FileListing,
  FileWriteResult,
  ListFilesOptions,
  PortalClientOptions,
  ReadFileOptions,
  Sandbox,
  UsageOptions,
  UsageResponse,
  WriteFileOptions,
} from "./types.js";

const USER_AGENT = "portal-sdk-typescript/0.1.0";
const DEFAULT_TIMEOUT_SEC = 30;
const RETRY_STATUSES = new Set([502, 503, 504]);

export class PortalClient {
  readonly baseURL: string;
  readonly apiKey: string;
  readonly timeoutMs: number;

  constructor(opts: PortalClientOptions) {
    if (!opts.baseURL) throw new TypeError("baseURL is required");
    if (!opts.apiKey) throw new TypeError("apiKey is required");
    this.baseURL = opts.baseURL.replace(/\/+$/, "");
    this.apiKey = opts.apiKey;
    this.timeoutMs = (opts.timeout ?? DEFAULT_TIMEOUT_SEC) * 1000;
  }

  // ====================================================================
  // Sandboxes
  // ====================================================================

  async createSandbox(opts: CreateSandboxOptions): Promise<Sandbox> {
    const body: Record<string, unknown> = { image: opts.image };
    if (opts.project_id !== undefined) body.project_id = opts.project_id;
    if (opts.command !== undefined) body.command = opts.command;
    if (opts.env !== undefined) body.env = opts.env;
    if (opts.resources !== undefined) body.resources = opts.resources;
    if (opts.ports !== undefined) body.ports = opts.ports;
    if (opts.egress !== undefined) body.egress = opts.egress;
    if (opts.workspace !== undefined) body.workspace = opts.workspace;
    if (opts.ttl_seconds !== undefined) body.ttl_seconds = opts.ttl_seconds;
    const idem = opts.idempotency_key ?? crypto.randomUUID();
    const result = await this._json("POST", "/v1/sandboxes", {
      body,
      headers: { "idempotency-key": idem },
    });
    return result as Sandbox;
  }

  async getSandbox(id: string): Promise<Sandbox> {
    const result = await this._json("GET", `/v1/sandboxes/${id}`, {
      retryGet: true,
    });
    return result as Sandbox;
  }

  async listSandboxes(
    projectId?: string,
  ): Promise<{ sandboxes: Sandbox[]; unreachable_nodes: string[] }> {
    const result = await this._json("GET", "/v1/sandboxes", {
      query: projectId ? { project_id: projectId } : undefined,
      retryGet: true,
    });
    return result as { sandboxes: Sandbox[]; unreachable_nodes: string[] };
  }

  async startSandbox(id: string): Promise<void> {
    await this._json("POST", `/v1/sandboxes/${id}/start`, { noContent: true });
  }

  async stopSandbox(id: string): Promise<void> {
    await this._json("POST", `/v1/sandboxes/${id}/stop`, { noContent: true });
  }

  async deleteSandbox(id: string): Promise<void> {
    await this._json("DELETE", `/v1/sandboxes/${id}`, { noContent: true });
  }

  // ====================================================================
  // Exec
  // ====================================================================

  async exec(sandboxId: string, opts: ExecOptions): Promise<ExecResult> {
    const body: Record<string, unknown> = { cmd: opts.cmd };
    if (opts.cwd !== undefined) body.cwd = opts.cwd;
    if (opts.env !== undefined) body.env = opts.env;
    // Field contract (review #47): `stdin` is a plain string, NOT base64.
    if (opts.stdin !== undefined) body.stdin = opts.stdin;
    if (opts.timeout_s !== undefined) body.timeout_s = opts.timeout_s;
    if (opts.max_output_bytes !== undefined) {
      body.max_output_bytes = opts.max_output_bytes;
    }
    const idem = opts.idempotency_key ?? crypto.randomUUID();
    const result = await this._json("POST", `/v1/sandboxes/${sandboxId}/exec`, {
      body,
      headers: { "idempotency-key": idem },
    });
    return result as ExecResult;
  }

  execStream(
    sandboxId: string,
    opts: ExecOptions,
  ): AsyncGenerator<ExecStreamChunk> {
    return execStream(this, sandboxId, opts);
  }

  // ====================================================================
  // Files
  // ====================================================================

  async listFiles(
    sandboxId: string,
    opts?: ListFilesOptions,
  ): Promise<FileListing> {
    const query: Record<string, string | number | boolean> = {};
    if (opts?.path !== undefined) query.path = opts.path;
    if (opts?.recursive !== undefined) query.recursive = opts.recursive;
    if (opts?.max_entries !== undefined) query.max_entries = opts.max_entries;
    const result = await this._json(
      "GET",
      `/v1/sandboxes/${sandboxId}/files`,
      { query, retryGet: true },
    );
    return result as FileListing;
  }

  async readFile(
    sandboxId: string,
    opts: ReadFileOptions,
  ): Promise<FileContent> {
    const query: Record<string, string | number> = { path: opts.path };
    if (opts.offset !== undefined) query.offset = opts.offset;
    if (opts.limit !== undefined) query.limit = opts.limit;
    const result = await this._json(
      "GET",
      `/v1/sandboxes/${sandboxId}/files/content`,
      { query, retryGet: true },
    );
    return result as FileContent;
  }

  async writeFile(
    sandboxId: string,
    opts: WriteFileOptions,
  ): Promise<FileWriteResult> {
    // Body is raw bytes (application/octet-stream), NOT JSON-wrapped.
    const data =
      typeof opts.data === "string"
        ? new TextEncoder().encode(opts.data)
        : opts.data;
    const query: Record<string, string> = { path: opts.path };
    if (opts.if_match !== undefined) query.if_match = opts.if_match;
    const result = await this._json(
      "PUT",
      `/v1/sandboxes/${sandboxId}/files/content`,
      { body: data, query },
    );
    return result as FileWriteResult;
  }

  async deleteFile(sandboxId: string, path: string): Promise<void> {
    await this._json("DELETE", `/v1/sandboxes/${sandboxId}/files`, {
      query: { path },
      noContent: true,
    });
  }

  async moveFile(
    sandboxId: string,
    from: string,
    to: string,
  ): Promise<{ from: string; to: string }> {
    const result = await this._json(
      "POST",
      `/v1/sandboxes/${sandboxId}/files/move`,
      { body: { from, to } },
    );
    return result as { from: string; to: string };
  }

  // ====================================================================
  // Logs
  // ====================================================================

  async getLogs(
    sandboxId: string,
    opts?: { tail?: number; follow?: boolean },
  ): Promise<string> {
    const query: Record<string, string | number | boolean> = {};
    if (opts?.tail !== undefined) query.tail = opts.tail;
    if (opts?.follow !== undefined) query.follow = opts.follow;
    const result = await this._json(
      "GET",
      `/v1/sandboxes/${sandboxId}/logs`,
      { query, retryGet: true, responseType: "text" },
    );
    return result as string;
  }

  logsFollow(
    sandboxId: string,
    opts?: { tail?: number },
  ): AsyncGenerator<string> {
    return logsFollow(this, sandboxId, opts);
  }

  // ====================================================================
  // Usage
  // ====================================================================

  async getUsage(opts?: UsageOptions): Promise<UsageResponse> {
    const query: Record<string, string> = {};
    if (opts?.from !== undefined) query.from = opts.from;
    if (opts?.to !== undefined) query.to = opts.to;
    if (opts?.kind !== undefined) query.kind = opts.kind;
    const result = await this._json("GET", "/v1/usage", {
      query,
      retryGet: true,
    });
    return result as UsageResponse;
  }

  // ====================================================================
  // Tokens
  // ====================================================================

  async createToken(
    opts: CreateTokenOptions,
  ): Promise<{ token: string; expires_at: string }> {
    const body: Record<string, unknown> = { scopes: opts.scopes };
    if (opts.ttl_seconds !== undefined) body.ttl_seconds = opts.ttl_seconds;
    if (opts.agent_run_id !== undefined) body.agent_run_id = opts.agent_run_id;
    const result = await this._json("POST", "/v1/tokens", { body });
    return result as { token: string; expires_at: string };
  }

  // ====================================================================
  // Internal HTTP plumbing
  // ====================================================================

  private async _json(
    method: string,
    path: string,
    opts: {
      body?: Record<string, unknown> | Uint8Array;
      query?: Record<string, unknown>;
      headers?: Record<string, string>;
      retryGet?: boolean;
      noContent?: boolean;
      responseType?: "json" | "text";
    } = {},
  ): Promise<unknown> {
    let url = this.baseURL + path;
    if (opts.query) {
      const qs = new URLSearchParams();
      for (const [k, v] of Object.entries(opts.query)) {
        if (v !== undefined && v !== null) qs.set(k, String(v));
      }
      const s = qs.toString();
      if (s) url += "?" + s;
    }

    const headers: Record<string, string> = {
      authorization: `Bearer ${this.apiKey}`,
      accept:
        opts.responseType === "text" ? "text/plain" : "application/json",
      "user-agent": USER_AGENT,
      ...(opts.headers ?? {}),
    };

    let reqBody: Uint8Array | string | undefined;
    if (opts.body !== undefined) {
      if (opts.body instanceof Uint8Array) {
        reqBody = opts.body;
        headers["content-type"] = "application/octet-stream";
      } else {
        reqBody = JSON.stringify(opts.body);
        headers["content-type"] = "application/json";
      }
    }

    const retryGet = opts.retryGet ?? false;
    const attempts = retryGet ? 2 : 1;
    let lastError: unknown;
    for (let attempt = 0; attempt < attempts; attempt++) {
      try {
        const controller = new AbortController();
        const timer = setTimeout(() => controller.abort(), this.timeoutMs);
        let resp: Response;
        try {
          resp = await fetch(url, {
            method,
            headers,
            body: reqBody,
            signal: controller.signal,
          });
        } finally {
          clearTimeout(timer);
        }
        if (resp.status === 204 || opts.noContent) {
          return undefined;
        }
        const text = await resp.text();
        if (!resp.ok) {
          throw makeError(resp.status, resp.headers, text);
        }
        if (opts.responseType === "text") return text;
        if (!text) return undefined;
        try {
          return JSON.parse(text);
        } catch {
          return text;
        }
      } catch (e) {
        lastError = e;
        if (
          e instanceof PortalError &&
          retryGet &&
          RETRY_STATUSES.has(e.statusCode) &&
          attempt === 0
        ) {
          continue;
        }
        throw e;
      }
    }
    throw (
      lastError ?? new PortalError(0, "unknown", "request failed without error")
    );
  }
}

// ---------------------------------------------------------------------------
// internal helpers (shared with stream.ts)
// ---------------------------------------------------------------------------

function makeError(
  status: number,
  headers: Headers,
  text: string,
): PortalError {
  let detail: Record<string, unknown> = {};
  try {
    const parsed = JSON.parse(text);
    if (parsed && typeof parsed === "object" && "error" in parsed) {
      detail = (parsed as { error: Record<string, unknown> }).error;
    }
  } catch {
    // ignore — non-JSON error body
  }
  const retryAfter = headers.get("retry-after") ?? undefined;
  return new PortalError(
    status,
    (detail.code as string) ?? "http_error",
    (detail.message as string) ?? `HTTP ${status}`,
    detail,
    retryAfter ?? undefined,
  );
}
