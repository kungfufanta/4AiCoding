// fake-llm — a deterministic OpenAI-compatible SSE server for the
// A3.4 Playwright suite (docs/AGENT_PLAN_A3.md §11 A3.4 "LLM giả …
// kịch bản cố định"). Zero dependencies so the compose build stays
// tiny; it speaks just enough of the wire format for the Portal's
// llm.StreamTools client (data: JSON lines + [DONE]).
//
// Scenario (agent mode, tools present) — keyed by the LAST user
// message and how many tool results the history already carries:
//   0 tool results → text + write_file {path:"hello.txt", …}
//   1 tool result  → run_command {command:"cat hello.txt"}
//   ≥2             → finish {summary:…}
// Prompt markers pick the pacing:
//   "SLOW" → 900 ms pauses (reload mid-run test)
//   "HANG" → 45 s silence (cancel test — the agent ctx drops first)
// Plain chat (no tools field) gets a one-line text reply.
import http from "node:http";

const PORT = Number(process.env.FAKE_LLM_PORT ?? 9999);

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const textChunk = (content) => ({
  choices: [{ index: 0, delta: { content }, finish_reason: null }],
});

const toolChunk = (name, args) => ({
  choices: [
    {
      index: 0,
      delta: {
        tool_calls: [
          {
            index: 0,
            id: `call_${name}`,
            type: "function",
            function: { name, arguments: args },
          },
        ],
      },
      finish_reason: null,
    },
  ],
});

const endChunk = () => ({
  choices: [{ index: 0, delta: {}, finish_reason: "tool_calls" }],
});

const server = http.createServer(async (req, res) => {
  if (req.method === "GET" && req.url === "/healthz") {
    res.writeHead(200, { "Content-Type": "text/plain" });
    res.end("ok");
    return;
  }
  if (req.method !== "POST" || !req.url.endsWith("/chat/completions")) {
    res.writeHead(404, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ error: { message: "not found" } }));
    return;
  }

  let body = "";
  for await (const part of req) body += part;
  let parsed;
  try {
    parsed = JSON.parse(body);
  } catch {
    res.writeHead(400, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ error: { message: "bad json" } }));
    return;
  }

  res.writeHead(200, {
    "Content-Type": "text/event-stream",
    "Cache-Control": "no-cache",
  });
  const gone = () => res.destroyed;
  const send = (obj) => {
    if (gone()) return false;
    res.write(`data: ${JSON.stringify(obj)}\n\n`);
    return true;
  };

  const msgs = Array.isArray(parsed.messages) ? parsed.messages : [];
  const users = msgs.filter((m) => m.role === "user").map((m) => String(m.content ?? ""));
  const lastUser = users[users.length - 1] ?? "";
  const toolResults = msgs.filter((m) => m.role === "tool").length;

  if (lastUser.includes("HANG")) {
    // The cancel test: stay silent until the agent's context is
    // cancelled (the disconnect ends this request).
    await sleep(45_000);
    if (gone()) return;
  }
  const pace = lastUser.includes("SLOW") ? 900 : 30;

  const hasTools = Array.isArray(parsed.tools) && parsed.tools.length > 0;
  if (!hasTools) {
    send(textChunk("pong — fake-llm."));
    res.write("data: [DONE]\n\n");
    res.end();
    return;
  }

  if (toolResults === 0) {
    send(textChunk("Đang tạo file hello.txt…"));
    await sleep(pace);
    send(
      toolChunk("write_file", JSON.stringify({ path: "hello.txt", content: "hello from agent\n" })),
    );
  } else if (toolResults === 1) {
    await sleep(pace);
    send(toolChunk("run_command", JSON.stringify({ command: "cat hello.txt" })));
  } else {
    await sleep(pace);
    send(
      toolChunk(
        "finish",
        JSON.stringify({
          summary: "Đã tạo hello.txt với nội dung “hello from agent” và đọc lại bằng cat — file đúng nội dung.",
        }),
      ),
    );
  }
  send(endChunk());
  res.write("data: [DONE]\n\n");
  res.end();
});

server.listen(PORT, "0.0.0.0", () => {
  console.log(`fake-llm listening on :${PORT}`);
});
