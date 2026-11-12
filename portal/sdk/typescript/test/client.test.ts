/**
 * End-to-end tests for the Portal TypeScript SDK against a stdlib-only
 * fake HTTP server (node:http).
 *
 * Run via:
 *   cd portal/sdk/typescript
 *   pnpm install
 *   pnpm build
 *   node --test dist/test/client.test.js
 *
 * No vitest/jest — only `node:test`.
 */

import { test } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import crypto from "node:crypto";
import { Buffer } from "node:buffer";
import type { AddressInfo } from "node:net";

import { PortalClient } from "../src/client.js";
import { PortalError } from "../src/errors.js";

// 26-char ULID (no `sbx-` prefix — per review #52 contract).
const SANDBOX_ID = "01HGE2K3Z9Q5P7R8T0V1X2Y3Z4";
const AUTH_TOKEN = "sbx_live_test_token";

interface ServerState {
  apiKey: string;
  idempotencyCache: Map<string, unknown>;
  createCount: number;
  files: Map<string, Buffer>;
}

interface FakeServer {
  url: string;
  state: ServerState;
  close: () => Promise<void>;
}

function startFakeServer(): Promise<FakeServer> {
  const state: ServerState = {
    apiKey: AUTH_TOKEN,
    idempotencyCache: new Map(),
    createCount: 0,
    files: new Map(),
  };

  const server = http.createServer((req, res) => {
    const auth = req.headers["authorization"];
    if (auth !== `Bearer ${state.apiKey}`) {
      res.writeHead(401, {
        "content-type": "application/json",
        "retry-after": "1",
      });
      res.end(
        JSON.stringify({
          error: { code: "unauthenticated", message: "missing token" },
        }),
      );
      return;
    }

    const reqUrl = new URL(req.url ?? "/", "http://localhost");
    const path = reqUrl.pathname;
    const query = Object.fromEntries(reqUrl.searchParams.entries());

    const chunks: Buffer[] = [];
    req.on("data", (c: Buffer) => chunks.push(c));
    req.on("end", () => {
      const body = Buffer.concat(chunks);
      route(req.method ?? "GET", path, query, body, req.headers, res, state);
    });
    req.on("error", (err) => {
      res.writeHead(400, { "content-type": "application/json" });
      res.end(JSON.stringify({ error: { code: "bad_request", message: String(err) } }));
    });
  });

  return new Promise<FakeServer>((resolve) => {
    server.listen(0, "127.0.0.1", () => {
      const port = (server.address() as AddressInfo).port;
      resolve({
        url: `http://127.0.0.1:${port}`,
        state,
        close: () =>
          new Promise<void>((r) => {
            server.close(() => r());
          }),
      });
    });
  });
}

function sendJson(
  res: http.ServerResponse,
  status: number,
  obj: unknown,
  extra?: Record<string, string>,
): void {
  const buf = Buffer.from(JSON.stringify(obj), "utf-8");
  const headers: Record<string, string> = {
    "content-type": "application/json",
    "content-length": String(buf.length),
    ...(extra ?? {}),
  };
  res.writeHead(status, headers);
  res.end(buf);
}

function sendNoContent(res: http.ServerResponse): void {
  res.writeHead(204, { "content-length": "0" });
  res.end();
}

function sendText(
  res: http.ServerResponse,
  status: number,
  text: string,
  contentType = "text/plain",
): void {
  const buf = Buffer.from(text, "utf-8");
  res.writeHead(status, {
    "content-type": contentType,
    "content-length": String(buf.length),
  });
  res.end(buf);
}

function route(
  method: string,
  path: string,
  query: Record<string, string>,
  body: Buffer,
  reqHeaders: http.IncomingHttpHeaders,
  res: http.ServerResponse,
  state: ServerState,
): void {
  // ----- GET ---------------------------------------------------------
  if (method === "GET") {
    if (path === "/v1/sandboxes") {
      sendJson(res, 200, {
        sandboxes: [
          { id: SANDBOX_ID, status: "running", image: "ubuntu:24.04" },
        ],
        unreachable_nodes: [],
      });
      return;
    }
    if (path === `/v1/sandboxes/${SANDBOX_ID}`) {
      sendJson(res, 200, {
        id: SANDBOX_ID,
        status: "running",
        image: "ubuntu:24.04",
        created_at: "2024-01-01T00:00:00Z",
        updated_at: "2024-01-01T00:00:00Z",
      });
      return;
    }
    if (path === `/v1/sandboxes/${SANDBOX_ID}/files`) {
      sendJson(res, 200, {
        path: query.path ?? "",
        recursive: query.recursive === "true",
        entries: [
          { path: "hello.txt", name: "hello.txt", size: 5, is_dir: false },
        ],
        truncated: false,
      });
      return;
    }
    if (path === `/v1/sandboxes/${SANDBOX_ID}/files/content`) {
      const fp = query.path ?? "";
      const content = state.files.get(fp) ?? Buffer.alloc(0);
      sendJson(res, 200, {
        path: fp,
        size: content.length,
        offset: 0,
        encoding: "utf-8",
        data: content.toString("utf-8"),
        truncated: false,
      });
      return;
    }
    if (path === `/v1/sandboxes/${SANDBOX_ID}/logs`) {
      if (query.follow === "true") {
        res.writeHead(200, { "content-type": "text/event-stream" });
        res.write("data: line1\n\n");
        res.write("data: line2\n\n");
        res.end();
        return;
      }
      sendText(res, 200, "log line 1\nlog line 2\n");
      return;
    }
    if (path === "/v1/usage") {
      sendJson(res, 200, {
        from: "2024-01-01T00:00:00Z",
        to: "2024-01-02T00:00:00Z",
        kind: "all",
        series: [
          {
            day: "2024-01-01T00:00:00Z",
            kind: "sandbox_seconds",
            quantity: 3600,
          },
        ],
        plan: {
          plan_id: "free",
          plan_name: "Free",
          monthly_vcpu_seconds: 100000,
          used_vcpu_seconds: 3600,
          max_concurrent_sandboxes: 3,
          running_sandboxes: 1,
        },
      });
      return;
    }
    sendJson(res, 404, { error: { code: "not_found", message: path } });
    return;
  }

  // ----- POST --------------------------------------------------------
  if (method === "POST") {
    if (path === "/v1/sandboxes") {
      const idem = reqHeaders["idempotency-key"];
      if (typeof idem === "string" && state.idempotencyCache.has(idem)) {
        sendJson(res, 201, state.idempotencyCache.get(idem));
        return;
      }
      state.createCount += 1;
      const data = body.length > 0 ? JSON.parse(body.toString("utf-8")) : {};
      const sandbox = {
        id: SANDBOX_ID,
        status: "running",
        image: data.image ?? "ubuntu:24.04",
        created_at: "2024-01-01T00:00:00Z",
        updated_at: "2024-01-01T00:00:00Z",
      };
      if (typeof idem === "string") {
        state.idempotencyCache.set(idem, sandbox);
      }
      sendJson(res, 201, sandbox);
      return;
    }
    if (path === "/v1/tokens") {
      sendJson(res, 200, {
        token: "agent_tok_test",
        expires_at: "2024-01-01T01:00:00Z",
      });
      return;
    }
    if (path === `/v1/sandboxes/${SANDBOX_ID}/start`) {
      sendNoContent(res);
      return;
    }
    if (path === `/v1/sandboxes/${SANDBOX_ID}/stop`) {
      sendNoContent(res);
      return;
    }
    if (path === `/v1/sandboxes/${SANDBOX_ID}/exec`) {
      if (query.stream === "ndjson") {
        res.writeHead(200, { "content-type": "application/x-ndjson" });
        res.write('{"stream":"stdout","data":"A\\n"}\n');
        res.write('{"stream":"stdout","data":"A\\n"}\n');
        res.write('{"exit_code":0,"timed_out":false,"duration_ms":12}\n');
        res.end();
        return;
      }
      sendJson(res, 200, {
        stdout: "hi\n",
        stderr: "",
        exit_code: 0,
        timed_out: false,
        truncated: false,
        duration_ms: 5,
      });
      return;
    }
    if (path === `/v1/sandboxes/${SANDBOX_ID}/files/move`) {
      const data = body.length > 0 ? JSON.parse(body.toString("utf-8")) : {};
      sendJson(res, 200, { from: data.from ?? "", to: data.to ?? "" });
      return;
    }
    sendJson(res, 404, { error: { code: "not_found", message: path } });
    return;
  }

  // ----- PUT ---------------------------------------------------------
  if (method === "PUT") {
    if (path === `/v1/sandboxes/${SANDBOX_ID}/files/content`) {
      const fp = query.path ?? "";
      state.files.set(fp, body);
      const sha = crypto.createHash("sha256").update(body).digest("hex");
      sendJson(res, 200, { path: fp, size: body.length, sha256: sha });
      return;
    }
    sendJson(res, 404, { error: { code: "not_found", message: path } });
    return;
  }

  // ----- DELETE ------------------------------------------------------
  if (method === "DELETE") {
    if (path === `/v1/sandboxes/${SANDBOX_ID}`) {
      sendNoContent(res);
      return;
    }
    if (path === `/v1/sandboxes/${SANDBOX_ID}/files`) {
      const fp = query.path ?? "";
      state.files.delete(fp);
      sendNoContent(res);
      return;
    }
    sendJson(res, 404, { error: { code: "not_found", message: path } });
    return;
  }

  sendJson(res, 404, { error: { code: "not_found", message: path } });
}

// ===========================================================================
// Tests
// ===========================================================================

let server: FakeServer;

test("setup fake server", async () => {
  server = await startFakeServer();
  assert.ok(server.url.startsWith("http://127.0.0.1:"));
});

test("create sandbox", async () => {
  const client = new PortalClient({
    baseURL: server.url,
    apiKey: AUTH_TOKEN,
    timeout: 5,
  });
  const sb = await client.createSandbox({ image: "ubuntu:24.04" });
  assert.equal(sb.id, SANDBOX_ID);
  assert.equal(sb.image, "ubuntu:24.04");
  assert.equal(sb.status, "running");
});

test("create sandbox auto-sends idempotency key", async () => {
  const client = new PortalClient({
    baseURL: server.url,
    apiKey: AUTH_TOKEN,
    timeout: 5,
  });
  // Snapshot cache size — proves SDK sends a key (cache grows by 1).
  const before = server.state.idempotencyCache.size;
  const sb = await client.createSandbox({ image: "ubuntu:24.04" });
  assert.equal(sb.id, SANDBOX_ID);
  assert.equal(server.state.idempotencyCache.size, before + 1);
});

test("idempotency dedup", async () => {
  const client = new PortalClient({
    baseURL: server.url,
    apiKey: AUTH_TOKEN,
    timeout: 5,
  });
  const key = "fixed-idem-key-123";
  const sb1 = await client.createSandbox({
    image: "ubuntu:24.04",
    idempotency_key: key,
  });
  const sb2 = await client.createSandbox({
    image: "ubuntu:24.04",
    idempotency_key: key,
  });
  assert.equal(sb1.id, sb2.id);
  // Server should have processed the create only once.
  // Note: previous tests have populated the cache, so we check createCount
  // increased by exactly 1 across these two calls. We snapshot before.
  const before = server.state.createCount;
  await client.createSandbox({
    image: "ubuntu:24.04",
    idempotency_key: key,
  });
  assert.equal(server.state.createCount, before);
});

test("get sandbox", async () => {
  const client = new PortalClient({
    baseURL: server.url,
    apiKey: AUTH_TOKEN,
    timeout: 5,
  });
  const sb = await client.getSandbox(SANDBOX_ID);
  assert.equal(sb.id, SANDBOX_ID);
  assert.equal(sb.status, "running");
});

test("list sandboxes", async () => {
  const client = new PortalClient({
    baseURL: server.url,
    apiKey: AUTH_TOKEN,
    timeout: 5,
  });
  const result = await client.listSandboxes();
  assert.equal(result.sandboxes.length, 1);
  assert.equal(result.sandboxes[0].id, SANDBOX_ID);
  assert.deepEqual(result.unreachable_nodes, []);
});

test("start / stop / delete", async () => {
  const client = new PortalClient({
    baseURL: server.url,
    apiKey: AUTH_TOKEN,
    timeout: 5,
  });
  await client.startSandbox(SANDBOX_ID);
  await client.stopSandbox(SANDBOX_ID);
  await client.deleteSandbox(SANDBOX_ID);
});

test("exec buffered", async () => {
  const client = new PortalClient({
    baseURL: server.url,
    apiKey: AUTH_TOKEN,
    timeout: 5,
  });
  const result = await client.exec(SANDBOX_ID, { cmd: ["echo", "hi"] });
  assert.equal(result.exit_code, 0);
  assert.equal(result.stdout, "hi\n");
  assert.equal(result.timed_out, false);
});

test("exec stream yields stdout + exit", async () => {
  const client = new PortalClient({
    baseURL: server.url,
    apiKey: AUTH_TOKEN,
    timeout: 5,
  });
  const events: unknown[] = [];
  for await (const chunk of client.execStream(SANDBOX_ID, {
    cmd: ["yes", "A"],
    timeout_s: 2,
  })) {
    events.push(chunk);
  }
  const stdoutEvents = events.filter((e) => "stream" in (e as object));
  const exitEvents = events.filter((e) => "exit_code" in (e as object));
  assert.ok(stdoutEvents.length >= 1, "expected >=1 stdout chunk");
  assert.ok(exitEvents.length >= 1, "expected >=1 exit chunk");
  assert.equal((exitEvents[0] as { exit_code: number }).exit_code, 0);
});

test("write + read file", async () => {
  const client = new PortalClient({
    baseURL: server.url,
    apiKey: AUTH_TOKEN,
    timeout: 5,
  });
  const writeResult = await client.writeFile(SANDBOX_ID, {
    path: "hello.txt",
    data: "hello",
  });
  assert.equal(writeResult.size, 5);
  assert.equal(writeResult.path, "hello.txt");
  assert.equal(writeResult.sha256.length, 64);

  const content = await client.readFile(SANDBOX_ID, { path: "hello.txt" });
  assert.equal(content.data, "hello");
  assert.equal(content.encoding, "utf-8");
});

test("write file accepts Uint8Array", async () => {
  const client = new PortalClient({
    baseURL: server.url,
    apiKey: AUTH_TOKEN,
    timeout: 5,
  });
  const writeResult = await client.writeFile(SANDBOX_ID, {
    path: "bin.dat",
    data: new Uint8Array([0x68, 0x69]), // "hi"
  });
  assert.equal(writeResult.size, 2);
});

test("list files", async () => {
  const client = new PortalClient({
    baseURL: server.url,
    apiKey: AUTH_TOKEN,
    timeout: 5,
  });
  const listing = await client.listFiles(SANDBOX_ID, { path: "." });
  assert.equal(listing.entries[0].path, "hello.txt");
});

test("delete file", async () => {
  const client = new PortalClient({
    baseURL: server.url,
    apiKey: AUTH_TOKEN,
    timeout: 5,
  });
  await client.writeFile(SANDBOX_ID, { path: "temp.txt", data: "x" });
  await client.deleteFile(SANDBOX_ID, "temp.txt");
  const content = await client.readFile(SANDBOX_ID, { path: "temp.txt" });
  assert.equal(content.data, "");
});

test("move file", async () => {
  const client = new PortalClient({
    baseURL: server.url,
    apiKey: AUTH_TOKEN,
    timeout: 5,
  });
  const result = await client.moveFile(SANDBOX_ID, "a.txt", "b.txt");
  assert.equal(result.from, "a.txt");
  assert.equal(result.to, "b.txt");
});

test("logs text", async () => {
  const client = new PortalClient({
    baseURL: server.url,
    apiKey: AUTH_TOKEN,
    timeout: 5,
  });
  const data = await client.getLogs(SANDBOX_ID, { tail: 10, follow: false });
  assert.ok(data.includes("log line 1"));
  assert.ok(data.includes("log line 2"));
});

test("logs follow SSE", async () => {
  const client = new PortalClient({
    baseURL: server.url,
    apiKey: AUTH_TOKEN,
    timeout: 5,
  });
  const events: string[] = [];
  for await (const line of client.logsFollow(SANDBOX_ID, { tail: 10 })) {
    events.push(line);
  }
  assert.ok(events.length >= 1);
  assert.ok(events[0].includes("line1"));
});

test("get usage", async () => {
  const client = new PortalClient({
    baseURL: server.url,
    apiKey: AUTH_TOKEN,
    timeout: 5,
  });
  const usage = await client.getUsage();
  assert.equal(usage.plan?.plan_name, "Free");
  assert.equal(usage.series.length, 1);
  assert.equal(usage.series[0].quantity, 3600);
});

test("create token", async () => {
  const client = new PortalClient({
    baseURL: server.url,
    apiKey: AUTH_TOKEN,
    timeout: 5,
  });
  const result = await client.createToken({ scopes: ["sandboxes:read"] });
  assert.equal(result.token, "agent_tok_test");
  assert.equal(result.expires_at, "2024-01-01T01:00:00Z");
});

test("unauthorized raises PortalError", async () => {
  const badClient = new PortalClient({
    baseURL: server.url,
    apiKey: "wrong",
    timeout: 5,
  });
  await assert.rejects(
    () => badClient.getSandbox(SANDBOX_ID),
    (err: unknown) => {
      assert.ok(err instanceof PortalError);
      assert.equal((err as PortalError).statusCode, 401);
      assert.equal((err as PortalError).code, "unauthenticated");
      return true;
    },
  );
});

test("teardown fake server", async () => {
  await server.close();
});
