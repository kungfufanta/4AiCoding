/**
 * 4AiCoding Portal SDK — Quickstart snippet.
 *
 * Source of truth for the Quickstart page on the Portal UI. Runs
 * end-to-end against a real Portal: create a sandbox, exec `echo 1+1`,
 * read container logs, then delete.
 *
 * Usage:
 *   PORTAL_API_KEY=sbx_live_xxx node --import tsx examples/quickstart.ts
 *
 * (Compiled version: `node dist/examples/quickstart.js` after `pnpm build`
 * — note the build config only emits dist/src + dist/test by default; to
 * compile examples add them to tsconfig.json's `include`.)
 */

import { PortalClient } from "../src/client.js";

async function main(): Promise<void> {
  // 1. Configure the client (Portal API key `sbx_live_...`).
  const apiKey = process.env.PORTAL_API_KEY ?? "sbx_live_REPLACE_ME";
  const baseURL = process.env.PORTAL_BASE_URL ?? "https://chat.devhub.io.vn";
  const client = new PortalClient({ baseURL, apiKey, timeout: 30 });

  // 2. Create a sandbox.
  const sb = await client.createSandbox({ image: "ubuntu:24.04" });
  console.log(`created sandbox: id=${sb.id} status=${sb.status}`);

  try {
    // 3. Exec a command (buffered).
    const result = await client.exec(sb.id, { cmd: ["sh", "-c", "echo $((1+1))"] });
    console.log(`exec: exit=${result.exit_code} stdout=${JSON.stringify(result.stdout)}`);

    // 4. Stream exec output as NDJSON.
    console.log("streaming exec output:");
    for await (const chunk of client.execStream(sb.id, {
      cmd: ["sh", "-c", "for i in 1 2 3; do echo line $i; done"],
      timeout_s: 10,
    })) {
      if (chunk.stream) {
        console.log(`  ${chunk.stream}: ${JSON.stringify(chunk.data)}`);
      } else if (chunk.exit_code !== undefined) {
        console.log(
          `  exit_code=${chunk.exit_code} duration_ms=${chunk.duration_ms}`,
        );
      }
    }

    // 5. Read container logs as text (no follow).
    const logs = await client.getLogs(sb.id, { tail: 50 });
    console.log(`logs (last 50 lines):\n${logs}`);

    // 6. Follow logs as SSE (until the server closes the stream).
    console.log("following logs…");
    for await (const line of client.logsFollow(sb.id, { tail: 10 })) {
      console.log(`  sse: ${line}`);
    }
  } finally {
    // 7. Delete the sandbox.
    await client.deleteSandbox(sb.id);
    console.log(`deleted sandbox: id=${sb.id}`);
  }
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
