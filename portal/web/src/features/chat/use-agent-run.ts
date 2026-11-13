// use-agent-run — A3.4: rebuilds one agent run's timeline from its
// event stream. The SSE endpoint replays from seq 0 (?since=0), which
// doubles as the reload reconstruction path (plan §10: "reload trang
// thì dựng lại từ GET runs + events"), and the server closes the
// stream right after done — EventSource would otherwise reconnect
// forever, so the hook closes it on done/reverted and treats
// repeated connection errors as a broken stream (the timeline stays
// at what arrived; the run row refetch settles the final state).
// refreshKey (bumped by the caller after an out-of-band event such as
// Revert, whose rows land after the stream closed) does NOT reopen the
// SSE stream — that would replay the same timeline a second time and
// race the JSON rebuild below, duplicating every message/tool card
// (caught on the preview screenshots: text and tool cards rendered 2×).
// The JSON replay is the single refresh path: it fetches ALL events
// (including rows appended after done) and rebuilds the timeline from
// scratch, so it is idempotent no matter how often it runs.

import { useEffect, useState } from "react";
import { api, type AgentEvent, type AgentRun, type AgentRunResult } from "../../api";

// One tool call of a run: the tool_call event opens it, the
// matching tool_result event completes it.
export type AgentToolStep = {
  id: string;
  name: string;
  args?: Record<string, unknown>;
  ok?: boolean;
  exit_code?: number;
  duration_ms?: number;
  output_preview?: string;
  truncated?: boolean;
  error?: string;
  error_code?: string;
};

export type AgentTimeline = {
  phase: string; // queued | starting | checkpoint | running | postcheck
  tool_mode?: string; // native | fallback
  warning?: string;
  text: string; // assistant deltas so far
  tools: AgentToolStep[];
  build?: { status: string; command?: string; tail?: string };
  done?: { status: string; error_code?: string; result?: AgentRunResult };
  reverted?: { files_restored: number; files_removed: number };
};

const STREAM_TYPES = ["status", "message", "tool_call", "tool_result", "build", "done", "reverted"] as const;

function freshTimeline(run: AgentRun): AgentTimeline {
  return { phase: run.status === "queued" ? "queued" : "starting", text: "", tools: [] };
}

function str(v: unknown): string {
  return typeof v === "string" ? v : "";
}
function num(v: unknown): number | undefined {
  return typeof v === "number" ? v : undefined;
}

function reduce(prev: AgentTimeline, ev: AgentEvent): AgentTimeline {
  const d = ev.data ?? {};
  switch (ev.type) {
    case "status":
      return {
        ...prev,
        phase: str(d.phase) || prev.phase,
        tool_mode: str(d.tool_mode) || prev.tool_mode,
        warning: str(d.warning) || prev.warning,
      };
    case "message":
      return { ...prev, text: prev.text + str(d.delta) };
    case "tool_call":
      return {
        ...prev,
        tools: [...prev.tools, { id: str(d.id), name: str(d.name), args: d.args_preview as Record<string, unknown> | undefined }],
      };
    case "tool_result":
      return {
        ...prev,
        tools: prev.tools.map((t) =>
          t.id === str(d.id)
            ? {
                ...t,
                ok: d.ok === true,
                exit_code: num(d.exit_code),
                duration_ms: num(d.duration_ms),
                output_preview: str(d.output_preview),
                truncated: d.truncated === true,
                error: str(d.error) || undefined,
                error_code: str(d.error_code) || undefined,
              }
            : t,
        ),
      };
    case "build":
      return { ...prev, build: { status: str(d.status), command: str(d.command) || undefined, tail: str(d.tail) || undefined } };
    case "done":
      return {
        ...prev,
        phase: "done",
        done: {
          status: str(d.status),
          error_code: str(d.error_code) || undefined,
          result: d.result as AgentRunResult | undefined,
        },
      };
    case "reverted":
      return {
        ...prev,
        reverted: { files_restored: num(d.files_restored) ?? 0, files_removed: num(d.files_removed) ?? 0 },
      };
    default:
      return prev;
  }
}

export function useAgentRun(orgID: string, run: AgentRun | undefined, refreshKey = 0) {
  const [timeline, setTimeline] = useState<AgentTimeline | null>(null);
  const [live, setLive] = useState(false);

  useEffect(() => {
    setTimeline(null);
    setLive(false);
    if (!run) return;

    let es: EventSource | null = null;
    let doneSeen = false;
    let errors = 0;
    setTimeline(freshTimeline(run));

    es = new EventSource(`${api.agentRunEventsUrl(orgID, run.id)}?since=0`);
    const onEvent = (type: string) => (e: MessageEvent<string>) => {
      let data: Record<string, unknown>;
      try {
        data = JSON.parse(e.data) as Record<string, unknown>;
      } catch {
        return;
      }
      setTimeline((prev) => reduce(prev ?? freshTimeline(run), { seq: 0, type, data, created_at: "" }));
      if (type === "done") {
        // done is the run's last event — the server closes the
        // stream now; close the source so it does not reconnect.
        doneSeen = true;
        setLive(false);
        es?.close();
      }
    };
    for (const t of STREAM_TYPES) es.addEventListener(t, onEvent(t) as EventListener);
    // The stream's own error frame ({"code":"internal"}) — distinct
    // from the native connection failure handled by onerror below.
    es.addEventListener("error", (() => {
      doneSeen = true;
      setLive(false);
      es?.close();
    }) as EventListener);
    es.onopen = () => {
      errors = 0;
      setLive(true);
    };
    es.onerror = () => {
      if (doneSeen) {
        es?.close();
        return;
      }
      errors++;
      // EventSource retries on its own with Last-Event-ID; after
      // three failures give up and leave the timeline as-is.
      if (errors >= 3) {
        es?.close();
        setLive(false);
      }
    };
    return () => es?.close();
  }, [orgID, run?.id]); // eslint-disable-line react-hooks/exhaustive-deps

  // refreshKey > 0: an out-of-band event (Revert) landed AFTER the
  // stream closed — the SSE endpoint stops at done, so the only way
  // to see the reverted event is the JSON replay (?follow=false).
  // Rebuild from a fresh timeline: the fetch returns the run's FULL
  // event history, so appending it onto what the stream already
  // rendered would double every delta and tool card.
  useEffect(() => {
    if (refreshKey === 0 || !run) return;
    let cancelled = false;
    api
      .agentRunEvents(orgID, run.id)
      .then((events) => {
        if (cancelled) return;
        setTimeline(() => {
          let t = freshTimeline(run);
          for (const ev of events) t = reduce(t, { ...ev, data: ev.data ?? {} });
          return t;
        });
      })
      .catch(() => {
        // Keep whatever the stream showed; the run row refetch still
        // carries reverted_at for the badge.
      });
    return () => {
      cancelled = true;
    };
  }, [refreshKey]); // eslint-disable-line react-hooks/exhaustive-deps

  return { timeline, live };
}
