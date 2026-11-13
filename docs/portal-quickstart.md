# Portal Quickstart

> Goal: create a sandbox, run a command in it, read a file, stream
> logs, and clean up — using the Portal SDK or MCP. You should not
> need to write raw HTTP.

This is the long-form companion to the in-app Quickstart page
(`/orgs/:slug/quickstart`). The in-app page has the same snippets with
copy buttons; this doc adds context, troubleshooting, and the
scope/quota rules the SDK hides from you.

## Prerequisites

1. A Portal account on `https://chat.devhub.io.vn` (or a local dev
   stack at `http://127.0.0.1:8080`).
2. An API key with the scopes you need:
   - `sandbox:create` to create/delete/start/stop sandboxes
   - `sandbox:exec` to exec and read logs
   - `sandboxes:files` to read/write/list files
   - `usage:read` to read usage
3. A Portal project (optional; if your key is pinned to one project,
   the SDK uses that project automatically).

Create a key from the **API keys** page; the secret is shown once.

## Python

```bash
pip install portal-sdk
```

```python
from portal_sdk import PortalClient

client = PortalClient(
    base_url="https://chat.devhub.io.vn",
    api_key="sbx_live_REPLACE_ME",
)

# 1. Create a sandbox
sb = client.create_sandbox(image="ubuntu:24.04")
print(f"sandbox id: {sb.id}")

# 2. Run a command
result = client.exec(sb.id, cmd=["bash", "-lc", "echo 1+1 | bc"])
print(result.stdout.strip())  # "2"

# 3. Write + read a file
client.write_file(sb.id, path="hello.txt", data=b"hello world")
content = client.read_file(sb.id, path="hello.txt")
print(content.data)  # "hello world"

# 4. Stream logs (SSE)
for line in client.logs_follow(sb.id):
    print(line, end="")

# 5. Clean up
client.delete_sandbox(sb.id)
```

The SDK auto-injects an `Idempotency-Key` (UUID4) on `create_sandbox`
and `exec`, so a network retry does not create a second sandbox or
re-run the command. Pass `idempotency_key=...` to reuse one yourself.

## TypeScript

```bash
pnpm add @4aicoding/portal-sdk
```

```typescript
import { PortalClient } from "@4aicoding/portal-sdk";

const client = new PortalClient({
  baseURL: "https://chat.devhub.io.vn",
  apiKey: "sbx_live_REPLACE_ME",
});

const sb = await client.createSandbox({ image: "ubuntu:24.04" });
console.log("sandbox id:", sb.id);

const result = await client.exec(sb.id, { cmd: ["bash", "-lc", "echo 1+1 | bc"] });
console.log(result.stdout.trim());  // "2"

await client.writeFile(sb.id, { path: "hello.txt" }, Buffer.from("hello world"));
const content = await client.readFile(sb.id, { path: "hello.txt" });
console.log(content.data);

for await (const line of client.logsFollow(sb.id)) {
  process.stdout.write(line);
}

await client.deleteSandbox(sb.id);
```

## curl

```bash
# 1. Create a sandbox
curl -X POST https://chat.devhub.io.vn/v1/sandboxes \
  -H "Authorization: Bearer sbx_live_REPLACE_ME" \
  -H "Idempotency-Key: $(uuidgen)" \
  -H "Content-Type: application/json" \
  -d '{"image":"ubuntu:24.04"}'

# 2. Run a command (replace SANDBOX_ID)
curl -X POST https://chat.devhub.io.vn/v1/sandboxes/SANDBOX_ID/exec \
  -H "Authorization: Bearer sbx_live_REPLACE_ME" \
  -H "Idempotency-Key: $(uuidgen)" \
  -H "Content-Type: application/json" \
  -d '{"cmd":["bash","-lc","echo 1+1 | bc"]}'

# 3. Read a file
curl "https://chat.devhub.io.vn/v1/sandboxes/SANDBOX_ID/files/content?path=hello.txt" \
  -H "Authorization: Bearer sbx_live_REPLACE_ME"

# 4. Stream logs (SSE)
curl -N "https://chat.devhub.io.vn/v1/sandboxes/SANDBOX_ID/logs?follow=true" \
  -H "Authorization: Bearer sbx_live_REPLACE_ME"

# 5. Delete
curl -X DELETE "https://chat.devhub.io.vn/v1/sandboxes/SANDBOX_ID" \
  -H "Authorization: Bearer sbx_live_REPLACE_ME"
```

## Handing a task to the agent (/v1)

The agent executes one task inside the sandbox's workspace: it reads
the project, calls tools (`write_file`, `edit_file`, `search`,
`run_command`, …), keeps a git checkpoint of the workspace **before**
it touches anything, and finishes with a summary plus
`files_changed`. Everything it did is revertible with one call.

Scopes: `agent:run` to create/cancel/revert runs, `agent:read` to
read runs and events. One active run per sandbox — a second create
answers `409 run_in_progress`.

```bash
BASE=https://chat.devhub.io.vn
KEY=sbx_live_REPLACE_ME
SBX=SANDBOX_ID

# 1. Hand over the task (agent:run). Idempotency-Key replays the
#    original run on retry instead of starting a second one.
curl -X POST "$BASE/v1/sandboxes/$SBX/agent/runs" \
  -H "Authorization: Bearer $KEY" \
  -H "Idempotency-Key: $(uuidgen)" \
  -H "Content-Type: application/json" \
  -d '{"prompt":"tạo file index.html hiển thị Hello rồi kiểm tra bằng curl","max_steps":30}'

# 2. Follow the events live (SSE). `?since=0` replays the run from
#    the start — pass the last seen seq instead and the stream
#    resumes exactly there (Last-Event-ID works too). The server
#    closes the stream right after `done`.
curl -N "$BASE/v1/agent/runs/RUN_ID/events?since=0" \
  -H "Authorization: Bearer $KEY"

# 3. Read the final run (status, summary, files_changed, steps, tokens).
curl "$BASE/v1/agent/runs/RUN_ID" -H "Authorization: Bearer $KEY"

# 4. Stop a run that is going the wrong way (agent:run).
curl -X POST "$BASE/v1/agent/runs/RUN_ID/cancel" -H "Authorization: Bearer $KEY"

# 5. Undo the whole run: roll the workspace back to its checkpoint
#    (agent:run; latest finished run of the sandbox, once).
curl -X POST "$BASE/v1/agent/runs/RUN_ID/revert" -H "Authorization: Bearer $KEY"
```

The event stream frames every step as `event: <type>` + one JSON
`data:` line — `status` (phase, tool_mode), `message` (assistant text
deltas), `tool_call` / `tool_result` (subject line + preview, never
file content), `build`, `done` (final status + result),
`reverted`. Terminal statuses: `succeeded`, `failed` (with
`error_code`), `cancelled`.

Python — create, follow, revert:

```python
import json, requests

BASE = "https://chat.devhub.io.vn"
H = {"Authorization": "Bearer sbx_live_REPLACE_ME"}
SBX = "SANDBOX_ID"

# 1. Create the run (agent:run)
run = requests.post(
    f"{BASE}/v1/sandboxes/{SBX}/agent/runs", headers=H,
    json={"prompt": "tạo file index.html hiển thị Hello rồi kiểm tra bằng curl"},
    timeout=30,
).json()
print("run:", run["id"], run["status"])            # queued

# 2. Follow the events until the run finishes (agent:read)
final = None
with requests.get(f"{BASE}/v1/agent/runs/{run['id']}/events",
                  headers=H, params={"since": "0"}, stream=True,
                  timeout=600) as s:
    event = None
    for raw in s.iter_lines(decode_unicode=True):
        if raw.startswith("event: "):
            event = raw[7:]
        elif raw.startswith("data: "):
            data = json.loads(raw[6:])
            if event == "tool_call":
                print("tool:", data.get("name"))
            elif event == "done":
                final = data
        elif not raw:
            event = None
print("final:", final and final.get("status"))      # succeeded

# 3. The workspace diff the run produced
run = requests.get(f"{BASE}/v1/agent/runs/{run['id']}", headers=H).json()
for f in run["result"]["files_changed"]:
    print(f["status"], f["path"])                   # A index.html

# 4. Undo it (optional) — workspace back to the checkpoint
requests.post(f"{BASE}/v1/agent/runs/{run['id']}/revert", headers=H)
```

TypeScript — same flow with `fetch` and an SSE reader:

```ts
const BASE = "https://chat.devhub.io.vn";
const H = { Authorization: "Bearer sbx_live_REPLACE_ME" };
const SBX = "SANDBOX_ID";

// 1. Create the run (agent:run)
const run = await (
  await fetch(`${BASE}/v1/sandboxes/${SBX}/agent/runs`, {
    method: "POST",
    headers: { ...H, "Content-Type": "application/json",
               "Idempotency-Key": crypto.randomUUID() },
    body: JSON.stringify({ prompt: "tạo file index.html hiển thị Hello rồi kiểm tra bằng curl" }),
  })
).json();

// 2. Follow the events (agent:read). `?since=<seq>` resumes a stream.
const res = await fetch(`${BASE}/v1/agent/runs/${run.id}/events?since=0`, { headers: H });
const reader = res.body!.getReader();
const decoder = new TextDecoder();
let event = "", buffer = "", done: any = null;
while (!done) {
  const { value, done: eof } = await reader.read();
  if (eof) break; // the server closes right after `done`
  buffer += decoder.decode(value, { stream: true });
  let i: number;
  while ((i = buffer.indexOf("\n")) >= 0) {
    const line = buffer.slice(0, i).trim();
    buffer = buffer.slice(i + 1);
    if (line.startsWith("event: ")) event = line.slice(7);
    else if (line.startsWith("data: ")) {
      const data = JSON.parse(line.slice(6));
      if (event === "tool_call") console.log("tool:", data.name);
      if (event === "done") done = data;
    } else if (!line) event = "";
  }
}
console.log("final:", done.status); // succeeded

// 3. Revert (optional) — back to the checkpoint.
await fetch(`${BASE}/v1/agent/runs/${run.id}/revert`, { method: "POST", headers: H });
```

Notes worth knowing before you automate against this:

- A run carries the workspace it started from: `files_changed` is the
  diff against the run's own checkpoint, and a file the agent renames
  shows up as the pair `D old` + `A new` (never an ambiguous rename
  row), so a revert restores the old name and removes the new one.
- The sandbox may already be busy when you arrive:
  `GET /v1/sandboxes/{id}/agent/runs` lists recent runs newest first
  (`agent:read`) — create only when no run is queued/running.
- Token usage of every run lands in the usage ledger
  (`agent_tokens` / `agent_steps`); the same quota view the UI shows.

## MCP server

The Portal ships an MCP (Model Context Protocol) server that exposes
the sandbox surface as six tools:

- `sandbox_create(image, project_id?, ttl_seconds?, cpus?, memory_mb?)`
- `sandbox_exec(sandbox_id, cmd[], cwd?, stdin?, timeout_s?)`
- `sandbox_delete(sandbox_id)`
- `read_file(sandbox_id, path)`
- `write_file(sandbox_id, path, content)`
- `list_files(sandbox_id, path?)`

Add it to your MCP client (Claude Desktop, Cursor, etc.):

```json
{
  "mcpServers": {
    "4aicoding-portal": {
      "url": "https://chat.devhub.io.vn/mcp",
      "headers": {
        "Authorization": "Bearer sbx_live_REPLACE_ME"
      }
    }
  }
}
```

The server forwards your Bearer token to the Portal API on every tool
call. The Portal's scope gate applies: a key without `sandboxes:files`
gets 403 on `read_file` / `write_file` / `list_files`.

## Errors you will see

| Status | `error.code` | Meaning |
|---|---|---|
| 401 | `invalid_api_key` | Token wrong, revoked, or expired. |
| 403 | `scope_exceeded` | Key lacks the route's scope. |
| 404 | `not_found` | Sandbox does not exist OR belongs to another org/project. The Portal does not reveal which. |
| 409 | `idempotency_in_progress` | Same Idempotency-Key still running. |
| 422 | `idempotency_key_reuse` | Same Idempotency-Key, different body. |
| 422 | `plan_limit` | vCPU/TTL/egress/monthly-vCPU above the org's plan. `error.field`, `error.limit`, `error.used` carry the detail. |
| 429 | `quota_exceeded` | Concurrent sandbox ceiling hit. Stop one or upgrade. |
| 429 | `rate_limited` | Too many requests on one bucket. `Retry-After` is in seconds. |

## Plan limits (free plan example)

| Limit | Free | Pro |
|---|---|---|
| Concurrent sandboxes | 2 | 8 |
| vCPU per sandbox | 1.0 | 4.0 |
| TTL (idle timeout) | 30 min | 6 h |
| Monthly vCPU-seconds | 20 h | 150 h |
| Egress mode | allowlist | allowlist |
| Rate limit (per bucket) | 2 RPS | 20 RPS |

See the **Usage** page for your current month-to-date numbers.

## OpenAPI

The source of truth for the Portal `/v1` API is `portal/openapi.yaml`
in the repo. The SDK is hand-written to match it; if the contract
changes, the openapi changes first, then the SDK, then this doc.
