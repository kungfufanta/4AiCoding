// Quickstart page (PORTAL_PLAN_V2 P6): three tabs (Python / TypeScript /
// curl) with copy buttons, plus the MCP JSON config block. Snippets
// are extracted verbatim from the SDK examples so they stay in sync.
//
// The page reads the org slug from the URL to build a per-org
// quickstart link (e.g. the MCP server URL). It does not show the
// API key — the user pastes their own from the API Keys page.

import { useState } from "react";
import { Check, Copy, Terminal } from "lucide-react";
import { useShell } from "../../layout/app-shell";
import { Card } from "../../ui/card";
import { Button } from "../../ui/button";

const PYTHON_SNIPPET = `from portal_sdk import PortalClient

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

# 4. Stream logs
for line in client.logs_follow(sb.id):
    print(line, end="")

# 5. Clean up
client.delete_sandbox(sb.id)`;

const TYPESCRIPT_SNIPPET = `import { PortalClient } from "@4aicoding/portal-sdk";

const client = new PortalClient({
  baseURL: "https://chat.devhub.io.vn",
  apiKey: "sbx_live_REPLACE_ME",
});

// 1. Create a sandbox
const sb = await client.createSandbox({ image: "ubuntu:24.04" });
console.log("sandbox id:", sb.id);

// 2. Run a command
const result = await client.exec(sb.id, { cmd: ["bash", "-lc", "echo 1+1 | bc"] });
console.log(result.stdout.trim());  // "2"

// 3. Write + read a file
await client.writeFile(sb.id, { path: "hello.txt" }, Buffer.from("hello world"));
const content = await client.readFile(sb.id, { path: "hello.txt" });
console.log(content.data);  // "hello world"

// 4. Stream logs
for await (const line of client.logsFollow(sb.id)) {
  process.stdout.write(line);
}

// 5. Clean up
await client.deleteSandbox(sb.id);`;

const CURL_SNIPPET = `# 1. Create a sandbox
curl -X POST https://chat.devhub.io.vn/v1/sandboxes \\
  -H "Authorization: Bearer sbx_live_REPLACE_ME" \\
  -H "Idempotency-Key: $(uuidgen)" \\
  -H "Content-Type: application/json" \\
  -d '{"image":"ubuntu:24.04"}'

# 2. Run a command (replace SANDBOX_ID)
curl -X POST https://chat.devhub.io.vn/v1/sandboxes/SANDBOX_ID/exec \\
  -H "Authorization: Bearer sbx_live_REPLACE_ME" \\
  -H "Idempotency-Key: $(uuidgen)" \\
  -H "Content-Type: application/json" \\
  -d '{"cmd":["bash","-lc","echo 1+1 | bc"]}'

# 3. Read a file
curl "https://chat.devhub.io.vn/v1/sandboxes/SANDBOX_ID/files/content?path=hello.txt" \\
  -H "Authorization: Bearer sbx_live_REPLACE_ME"

# 4. Stream logs (SSE)
curl -N "https://chat.devhub.io.vn/v1/sandboxes/SANDBOX_ID/logs?follow=true" \\
  -H "Authorization: Bearer sbx_live_REPLACE_ME"

# 5. Delete
curl -X DELETE "https://chat.devhub.io.vn/v1/sandboxes/SANDBOX_ID" \\
  -H "Authorization: Bearer sbx_live_REPLACE_ME"`;

type Tab = "python" | "typescript" | "curl";

export function QuickstartPage() {
  const { org } = useShell();
  const [tab, setTab] = useState<Tab>("python");

  const snippets: Record<Tab, string> = {
    python: PYTHON_SNIPPET,
    typescript: TYPESCRIPT_SNIPPET,
    curl: CURL_SNIPPET,
  };

  const mcpConfig = {
    mcpServers: {
      "4aicoding-portal": {
        url: `https://chat.devhub.io.vn/mcp`,
        headers: {
          Authorization: "Bearer sbx_live_REPLACE_ME",
        },
      },
    },
  };

  return (
    <div className="space-y-4">
      <div>
        <h1 className="text-lg font-semibold tracking-tight text-fg">Quickstart</h1>
        <p className="text-sm text-muted">
          Create a sandbox, run a command, read a file — three ways. Replace
          {" "}<code className="rounded bg-fg/5 px-1 py-0.5 text-xs">sbx_live_REPLACE_ME</code>{" "}
          with your API key from the <a href={`/orgs/${org.slug}/api-keys`} className="text-brand underline">API Keys page</a>.
        </p>
      </div>

      <Card className="p-0">
        <div className="flex items-center gap-1 border-b border-fg/5 p-2">
          {(["python", "typescript", "curl"] as const).map((t) => (
            <button
              key={t}
              onClick={() => setTab(t)}
              className={`rounded-md px-3 py-1.5 text-sm font-medium transition ${
                tab === t
                  ? "bg-brand text-brand-fg"
                  : "text-muted hover:bg-fg/5 hover:text-fg"
              }`}
              aria-pressed={tab === t}
            >
              {t === "python" ? "Python" : t === "typescript" ? "TypeScript" : "curl"}
            </button>
          ))}
        </div>
        <div className="relative">
          <CopyButton text={snippets[tab]} className="absolute right-3 top-3" />
          <pre className="overflow-x-auto p-4 text-xs leading-relaxed text-fg">
            <code>{snippets[tab]}</code>
          </pre>
        </div>
      </Card>

      <Card className="p-5">
        <div className="mb-3 flex items-center gap-2">
          <Terminal aria-hidden className="size-4 text-muted" />
          <h2 className="text-sm font-medium text-fg">MCP server config</h2>
        </div>
        <p className="mb-3 text-xs text-muted">
          Drop this into your MCP client (Claude Desktop, Cursor, etc.) to call
          the Portal's sandbox surface as MCP tools. The server forwards your
          Bearer token to the Portal API; the Portal's scope gate applies.
        </p>
        <div className="relative">
          <CopyButton text={JSON.stringify(mcpConfig, null, 2)} className="absolute right-3 top-3" />
          <pre className="overflow-x-auto rounded-lg bg-fg/5 p-4 text-xs leading-relaxed text-fg">
            <code>{JSON.stringify(mcpConfig, null, 2)}</code>
          </pre>
        </div>
        <p className="mt-3 text-xs text-muted">
          Tools exposed: <code className="rounded bg-fg/5 px-1">sandbox_create</code>,
          {" "}<code className="rounded bg-fg/5 px-1">sandbox_exec</code>,
          {" "}<code className="rounded bg-fg/5 px-1">sandbox_delete</code>,
          {" "}<code className="rounded bg-fg/5 px-1">read_file</code>,
          {" "}<code className="rounded bg-fg/5 px-1">write_file</code>,
          {" "}<code className="rounded bg-fg/5 px-1">list_files</code>.
        </p>
      </Card>
    </div>
  );
}

// CopyButton shows a copy icon and flips to a check for 1.5s after copy.
function CopyButton({ text, className }: { text: string; className?: string }) {
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      // clipboard API not available (e.g. insecure context) — silent
    }
  };
  return (
    <Button
      variant="ghost"
      size="sm"
      onClick={copy}
      className={className}
      aria-label={copied ? "Copied" : "Copy to clipboard"}
    >
      {copied ? (
        <Check aria-hidden className="size-3.5 text-success" />
      ) : (
        <Copy aria-hidden className="size-3.5" />
      )}
    </Button>
  );
}
