/**
 * Streaming helpers for the Portal SDK.
 *
 * - `execStream` — async generator yielding parsed NDJSON lines from
 *   `POST /v1/sandboxes/{id}/exec?stream=ndjson`.
 * - `logsFollow` — async generator yielding SSE `data:` payloads from
 *   `GET /v1/sandboxes/{id}/logs?follow=true`.
 *
 * Both functions take `client` as the first arg so they can be invoked
 * as `client.execStream(...)` / `client.logsFollow(...)` via the thin
 * wrappers on `PortalClient`. Uses Web `fetch` + `ReadableStream`
 * reader — works in Node 18+ / Deno / Bun / browsers.
 */

import type { PortalClient } from "./client.js";
import type { ExecOptions, ExecStreamChunk } from "./types.js";
import { PortalError } from "./errors.js";

const USER_AGENT = "portal-sdk-typescript/0.1.0";

/**
 * Stream an exec run as NDJSON. Each yielded value is a parsed JSON
 * object: `{"stream":"stdout"|"stderr","data":"..."}` for chunks, and
 * `{"exit_code":...,"timed_out":...,"duration_ms":...}` for the
 * terminal line.
 */
export async function* execStream(
  client: PortalClient,
  sandboxId: string,
  opts: ExecOptions,
): AsyncGenerator<ExecStreamChunk> {
  const body: Record<string, unknown> = { cmd: opts.cmd };
  if (opts.cwd !== undefined) body.cwd = opts.cwd;
  if (opts.env !== undefined) body.env = opts.env;
  // Field contract (review #47): `stdin` is a plain string, NOT base64.
  if (opts.stdin !== undefined) body.stdin = opts.stdin;
  if (opts.timeout_s !== undefined) body.timeout_s = opts.timeout_s;
  if (opts.max_output_bytes !== undefined) {
    body.max_output_bytes = opts.max_output_bytes;
  }

  // Idempotency-Key auto-injected (review #47 contract).
  const idempotencyKey = opts.idempotency_key ?? crypto.randomUUID();
  const url = `${client.baseURL}/v1/sandboxes/${sandboxId}/exec?stream=ndjson`;

  const resp = await fetch(url, {
    method: "POST",
    headers: {
      authorization: `Bearer ${client.apiKey}`,
      accept: "application/x-ndjson",
      "content-type": "application/json",
      "idempotency-key": idempotencyKey,
      "user-agent": USER_AGENT,
    },
    body: JSON.stringify(body),
  });

  if (!resp.ok || !resp.body) {
    const text = resp.body ? await resp.text() : "";
    throw makeError(resp.status, resp.headers, text);
  }

  const reader = resp.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      let idx: number;
      while ((idx = buffer.indexOf("\n")) >= 0) {
        const line = buffer.slice(0, idx);
        buffer = buffer.slice(idx + 1);
        const trimmed = line.trim();
        if (!trimmed) continue;
        try {
          yield JSON.parse(trimmed) as ExecStreamChunk;
        } catch {
          // Skip malformed lines — should not happen with a conforming server.
        }
      }
    }
    const tail = buffer.trim();
    if (tail) {
      try {
        yield JSON.parse(tail) as ExecStreamChunk;
      } catch {
        // ignore trailing malformed line
      }
    }
  } finally {
    try {
      await reader.cancel();
    } catch {
      // best-effort cleanup
    }
  }
}

/**
 * Follow container logs as Server-Sent Events. Each yielded value is
 * the data payload of one `data:` line (without the `data: ` prefix
 * and trailing newline).
 */
export async function* logsFollow(
  client: PortalClient,
  sandboxId: string,
  opts?: { tail?: number },
): AsyncGenerator<string> {
  const params = new URLSearchParams({ follow: "true" });
  if (opts?.tail !== undefined) params.set("tail", String(opts.tail));
  const url = `${client.baseURL}/v1/sandboxes/${sandboxId}/logs?${params.toString()}`;

  const resp = await fetch(url, {
    method: "GET",
    headers: {
      authorization: `Bearer ${client.apiKey}`,
      accept: "text/event-stream",
      "user-agent": USER_AGENT,
    },
  });

  if (!resp.ok || !resp.body) {
    const text = resp.body ? await resp.text() : "";
    throw makeError(resp.status, resp.headers, text);
  }

  const reader = resp.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      let idx: number;
      while ((idx = buffer.indexOf("\n")) >= 0) {
        const line = buffer.slice(0, idx);
        buffer = buffer.slice(idx + 1);
        if (line.startsWith("data:")) {
          let payload = line.slice(5);
          if (payload.startsWith(" ")) payload = payload.slice(1);
          yield payload;
        }
        // Other SSE fields (event:, id:, retry:) and blank lines ignored.
      }
    }
    if (buffer.startsWith("data:")) {
      let payload = buffer.slice(5);
      if (payload.startsWith(" ")) payload = payload.slice(1);
      yield payload;
    }
  } finally {
    try {
      await reader.cancel();
    } catch {
      // best-effort cleanup
    }
  }
}

// ---------------------------------------------------------------------------
// internal
// ---------------------------------------------------------------------------

function makeError(status: number, headers: Headers, text: string): PortalError {
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
