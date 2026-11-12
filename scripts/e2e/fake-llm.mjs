// fake-llm — a dependency-free OpenAI-compatible chat completions
// server for the portal preview stack (AGENT_PLAN_A3 A3.4: Playwright
// drives the agent loop against a FIXED scenario, no real provider).
//
//   node scripts/e2e/fake-llm.mjs           (LISTEN :9933)
//   POST /v1/chat/completions               (stream + non-stream)
//   GET  /v1/models                         ({ "data": [...] })
//   GET  /healthz                           ("ok")
//
// The script keys off the number of `role:"tool"` messages in the
// request (the agent loop appends one per executed tool call), so the
// conversation always walks the same three rounds:
//
//   0 tool results → tool_call write_file hello.txt
//   1 tool result  → tool_call run_command cat hello.txt
//   2 tool results → tool_call finish(summary)
//
// Each round sleeps DELAY_MS so the browser test can reload the page
// mid-run and press Stop while the loop is still alive. The payload
// wire shape mirrors the A3.0 golden fixtures exactly: tool_calls
// entries carry `{"index", "id", "type":"function",
// "function":{"name","arguments":"<JSON string>"}}`.
// No `usage` chunk is emitted (like WebHub) — the client estimates.

import http from "node:http";

const PORT = Number(process.env.FAKE_LLM_PORT ?? 9933);
const DELAY_MS = Number(process.env.FAKE_LLM_DELAY_MS ?? 1200);

const MODEL = "fake-4.6";

const SCENARIOS = [
  {
    name: "write_file",
    args: { path: "hello.txt", content: "Hello from the A3.4 agent\n" },
  },
  {
    name: "run_command",
    args: { cmd: "cat hello.txt" },
  },
  {
    name: "finish",
    args: {
      summary:
        "Đã tạo hello.txt (2 dòng) và kiểm tra lại bằng cat — nội dung khớp. Run hoàn tất.",
    },
  },
];

function pickRound(body) {
  const msgs = Array.isArray(body?.messages) ? body.messages : [];
  const toolResults = msgs.filter((m) => m?.role === "tool").length;
  return SCENARIOS[Math.min(toolResults, SCENARIOS.length - 1)];
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function readBody(req) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    let size = 0;
    req.on("data", (c) => {
      size += c.length;
      if (size > 4 << 20) {
        reject(new Error("body too large"));
        req.destroy();
        return;
      }
      chunks.push(c);
    });
    req.on("end", () => resolve(Buffer.concat(chunks).toString("utf8")));
    req.on("error", reject);
  });
}

function toolCallChunk(id, scen, first, last) {
  const call = {
    index: 0,
    id,
    type: "function",
    function: {
      name: scen.name,
      // arguments MUST be a JSON string on the wire (A3.0 review fix).
      arguments: JSON.stringify(scen.args),
    },
  };
  return {
    id: "chatcmpl-fake",
    object: "chat.completion.chunk",
    model: MODEL,
    choices: [
      {
        index: 0,
        delta: first ? { role: "assistant", content: "", tool_calls: [call] } : {},
        finish_reason: last ? "tool_calls" : null,
      },
    ],
  };
}

async function handleCompletions(req, res) {
  let body = {};
  try {
    body = JSON.parse(await readBody(req));
  } catch {
    res.writeHead(400, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ error: { message: "bad json" } }));
    return;
  }
  const scen = pickRound(body);
  const callId = `call_${scen.name}_${Math.random().toString(36).slice(2, 8)}`;

  if (body?.stream !== true) {
    await sleep(DELAY_MS);
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(
      JSON.stringify({
        id: "chatcmpl-fake",
        object: "chat.completion",
        model: String(body?.model ?? MODEL),
        choices: [
          {
            index: 0,
            message: {
              role: "assistant",
              content: "fake reply",
              tool_calls: [
                {
                  id: callId,
                  type: "function",
                  function: { name: scen.name, arguments: JSON.stringify(scen.args) },
                },
              ],
            },
            finish_reason: "tool_calls",
          },
        ],
      }),
    );
    return;
  }

  res.writeHead(200, {
    "Content-Type": "text/event-stream",
    "Cache-Control": "no-cache",
    Connection: "keep-alive",
  });
  res.flushHeaders(); // SSE clients (curl, fetch) see the 200 at once
  // Stretch one logical round over a few SSE writes so a mid-run
  // reload and Stop always land while the run is still active.
  const steps = 4;
  for (let i = 0; i < steps; i++) {
    await sleep(DELAY_MS);
    // Client-gone check: the RESPONSE side, not the request side —
    // once the body has been consumed to EOF, req.destroyed is true
    // on modern Node and would silently kill the loop after the
    // headers were already flushed.
    if (res.destroyed) return;
    const first = i === 0;
    const last = i === steps - 1;
    res.write(`data: ${JSON.stringify(toolCallChunk(callId, scen, first, last))}\n\n`);
  }
  res.write("data: [DONE]\n\n");
  res.end();
}

const server = http.createServer((req, res) => {
  const url = new URL(req.url, "http://localhost");
  if (req.method === "GET" && url.pathname === "/healthz") {
    res.writeHead(200, { "Content-Type": "text/plain" });
    res.end("ok");
    return;
  }
  if (req.method === "GET" && url.pathname === "/v1/models") {
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ data: [{ id: MODEL, object: "model" }] }));
    return;
  }
  if (req.method === "POST" && url.pathname === "/v1/chat/completions") {
    handleCompletions(req, res).catch(() => {
      if (!res.headersSent) res.writeHead(500);
      res.end();
    });
    return;
  }
  res.writeHead(404, { "Content-Type": "application/json" });
  res.end(JSON.stringify({ error: { message: "not found" } }));
});

server.listen(PORT, "0.0.0.0", () => {
  console.log(`fake-llm listening on :${PORT} (delay ${DELAY_MS}ms per round)`);
});
