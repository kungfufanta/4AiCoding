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
