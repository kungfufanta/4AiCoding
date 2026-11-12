// Chat page: talk to the platform model (Chat MVP) AND hand tasks to
// the coding agent (A3.4). One conversation pins at most one sandbox
// (decision §13-3); the header carries the attach dropdown and the
// Chat/Agent switch. Agent mode sends the draft as a run: the run card
// streams events over SSE, shows collapsed tool cards and the final
// result with Revert/Stop/Continue. The composer locks while a run —
// this chat's or one started from the API (/v1) — holds the sandbox.

import { useEffect, useMemo, useRef, useState, type KeyboardEvent } from "react";
import { useNavigate, useParams } from "react-router";
import { useQueryClient } from "@tanstack/react-query";
import { Bot, Loader2, MessageSquarePlus, PanelLeft, SendHorizontal, Square, Trash2, User } from "lucide-react";
import { api, ApiError, type AgentRun, type ChatMessage } from "../../api";
import { useAgentRuns, useChatMessages, useChats, useChatStatus, useSandboxAgentRuns } from "../../lib/queries";
import { useSandboxes } from "../sandboxes/hooks";
import { cn } from "../../lib/cn";
import { useShell } from "../../layout/app-shell";
import { Button } from "../../ui/button";
import { useConfirm } from "../../ui/dialog";
import { Select } from "../../ui/select";
import { Skeleton } from "../../ui/skeleton";
import { useToast } from "../../ui/toast";
import { AgentRunCard } from "./agent-run-card";
import { Markdown } from "./markdown";

type Pending = { user: string; reply: string };

const SUGGESTIONS = [
  "Lên ý tưởng landing page cho quán cà phê",
  "Viết hàm debounce bằng TypeScript",
  "Giải thích sandbox là gì và dùng để làm gì",
];

const AGENT_SUGGESTIONS = [
  "Tạo file index.html hiển thị Hello rồi kiểm tra bằng curl",
  "Xem cấu trúc thư mục rồi tóm tắt project trong BRAIN.md",
  "Viết hello.py in ra bảng cửu chương 7 rồi chạy thử",
];

export function ChatPage() {
  const { org } = useShell();
  const { chatId } = useParams();
  const navigate = useNavigate();
  const qc = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();

  const status = useChatStatus(org.id);
  const chats = useChats(org.id);
  const messages = useChatMessages(org.id, chatId);
  const sandboxes = useSandboxes(org.id);
  const runs = useAgentRuns(org.id, chatId);

  // A3.4 header state: the conversation's pinned sandbox comes from
  // the chats list (chatConversationJSON.sandbox_id).
  const currentChat = chats.data?.find((c) => c.id === chatId);
  const sandboxID = currentChat?.sandbox_id ?? "";
  const sandboxRows = sandboxes.data?.sandboxes ?? [];
  const chatHasRuns = (runs.data?.length ?? 0) > 0;

  const [mode, setMode] = useState<"chat" | "agent">("chat");
  const [draft, setDraft] = useState("");
  const [pending, setPending] = useState<Pending | null>(null);
  const [attaching, setAttaching] = useState(false);
  const [listOpen, setListOpen] = useState(false);
  const abortRef = useRef<AbortController | null>(null);
  const scrollRef = useRef<HTMLDivElement>(null);
  const base = `/orgs/${org.slug}/chat`;

  useEffect(() => () => abortRef.current?.abort(), []);
  useEffect(() => {
    setListOpen(false);
    setMode("chat");
    setPending(null);
  }, [chatId]);

  const shown: ChatMessage[] = messages.data ?? [];
  const runList: AgentRun[] = useMemo(() => (runs.data ?? []).slice().reverse(), [runs.data]);
  const activeRun = runs.data?.find((r) => r.status === "queued" || r.status === "running");

  // Cross-surface lock: an active run on the ATTACHED sandbox that
  // does not belong to this conversation (created via /v1 or another
  // chat) — plan §10 "Agent đang chạy (từ API)".
  const sandboxRuns = useSandboxAgentRuns(org.id, sandboxID || undefined);
  const externalRun = sandboxRuns.data?.find(
    (r) => (r.status === "queued" || r.status === "running") && r.conversation_id !== chatId,
  );

  useEffect(() => {
    const el = scrollRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [shown.length, pending?.reply, pending?.user, runList.length, activeRun?.steps, activeRun?.status]);

  const disabled = status.data?.enabled === false;
  const busy = pending !== null;
  const agentBusy = Boolean(activeRun) || Boolean(externalRun);
  // A2: per-message model picker. The options come from the effective
  // connection's allowed list (chat status); empty list = only the
  // connection's default model, so the picker hides.
  const availableModels = status.data?.models ?? [];
  const [selectedModel, setSelectedModel] = useState("");

  async function ensureChat(): Promise<string | null> {
    if (chatId) return chatId;
    try {
      const created = await api.createChat(org.id);
      qc.setQueryData(["chat-messages", org.id, created.id], []);
      await qc.invalidateQueries({ queryKey: ["chats", org.id] });
      navigate(`${base}/${created.id}`, { replace: false });
      return created.id;
    } catch (err) {
      toast("error", err instanceof Error ? err.message : String(err));
      return null;
    }
  }

  async function attach(sbxID: string) {
    if (!sbxID) return;
    setAttaching(true);
    try {
      const id = await ensureChat();
      if (!id) return;
      await api.attachChatSandbox(org.id, id, sbxID);
      await qc.invalidateQueries({ queryKey: ["chats", org.id] });
      toast("success", "Đã gắn sandbox vào hội thoại này.");
    } catch (err) {
      if (err instanceof ApiError && err.code === "sandbox_locked") {
        toast("error", "Hội thoại đã có run — không đổi được sandbox.");
      } else {
        toast("error", err instanceof Error ? err.message : String(err));
      }
    } finally {
      setAttaching(false);
    }
  }

  async function send(text: string) {
    const content = text.trim();
    if (!content || busy) return;
    if (mode === "agent") return sendAgent(content);
    setDraft("");
    setPending({ user: content, reply: "" });

    const id = await ensureChat();
    if (!id) {
      setPending(null);
      setDraft(content);
      return;
    }

    const ctrl = new AbortController();
    abortRef.current = ctrl;
    const settle = async () => {
      await qc.invalidateQueries({ queryKey: ["chat-messages", org.id, id] });
      await qc.invalidateQueries({ queryKey: ["chats", org.id] });
      setPending(null);
      abortRef.current = null;
    };
    try {
      await api.sendChatMessage(
        org.id,
        id,
        content,
        {
          onDelta: (t) => setPending((p) => (p ? { ...p, reply: p.reply + t } : p)),
          onDone: () => {},
          onError: (_code, message) => toast("error", message),
        },
        { model: selectedModel || undefined, signal: ctrl.signal },
      );
    } catch (err) {
      if (!(err instanceof DOMException && err.name === "AbortError")) {
        toast("error", err instanceof ApiError ? err.message : String(err));
      }
    } finally {
      await settle();
    }
  }

  // Agent mode: the draft becomes a run (plan §10). The composer is
  // already locked while a run is active; a 409 here covers the race
  // where a /v1-run started a moment ago.
  async function sendAgent(content: string) {
    setDraft("");
    const id = await ensureChat();
    if (!id) return;
    if (!sandboxID) {
      setDraft(content);
      toast("error", "Gắn sandbox vào hội thoại trước khi chạy agent.");
      return;
    }
    try {
      await api.createAgentRun(org.id, id, { prompt: content, model: selectedModel || undefined });
      await qc.invalidateQueries({ queryKey: ["agent-runs", org.id, id] });
    } catch (err) {
      if (err instanceof ApiError && err.code === "run_in_progress") {
        toast("error", "Agent đang chạy (từ API) — đợi run hiện tại xong rồi thử lại.");
      } else {
        toast("error", err instanceof Error ? err.message : String(err));
      }
      setDraft(content);
    }
  }

  async function remove(id: string) {
    const ok = await confirm({
      title: "Delete this chat?",
      description: "The conversation and all its messages are removed for good.",
      confirmLabel: "Delete",
      danger: true,
    });
    if (!ok) return;
    try {
      await api.deleteChat(org.id, id);
      await qc.invalidateQueries({ queryKey: ["chats", org.id] });
      if (id === chatId) navigate(base);
    } catch (err) {
      toast("error", err instanceof Error ? err.message : String(err));
    }
  }

  function onKey(e: KeyboardEvent<HTMLTextAreaElement>) {
    if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) {
      e.preventDefault();
      void send(draft);
    }
  }

  const empty = !chatId && !pending;
  const composerLocked = disabled || (mode === "agent" && agentBusy);
  const composerHint = mode === "agent"
    ? externalRun
      ? "Agent đang chạy (từ API)"
      : activeRun
        ? "Agent đang chạy — bấm Dừng nếu muốn kết thúc sớm"
        : "Giao việc cho agent (Enter để gửi)"
    : "Nhắn tin… (Enter để gửi, Shift+Enter xuống dòng)";

  const suggestions = mode === "agent" ? AGENT_SUGGESTIONS : SUGGESTIONS;

  return (
    <div className="-my-2 grid h-[calc(100dvh-3.5rem-2rem)] min-h-[420px] grid-cols-1 gap-4 md:grid-cols-[220px_minmax(0,1fr)]">
      {/* Conversation list */}
      <aside
        className={cn(
          "min-h-0 flex-col rounded-xl border border-border bg-surface p-2 md:flex",
          listOpen ? "fixed inset-x-4 top-16 bottom-4 z-40 flex shadow-lg" : "hidden",
        )}
        aria-label="Conversations"
      >
        <Button className="mb-2 w-full" onClick={() => navigate(base)} disabled={busy}>
          <MessageSquarePlus aria-hidden className="size-4" />
          New chat
        </Button>
        <div className="min-h-0 flex-1 space-y-0.5 overflow-y-auto">
          {chats.isPending ? (
            <div className="space-y-2 p-1">
              <Skeleton className="h-7" />
              <Skeleton className="h-7" />
            </div>
          ) : (chats.data ?? []).length === 0 ? (
            <p className="px-2 py-3 text-xs text-muted">No conversations yet.</p>
          ) : (
            chats.data!.map((c) => (
              <div
                key={c.id}
                className={cn(
                  "group flex items-center rounded-[var(--radius)] text-sm",
                  c.id === chatId ? "bg-brand-soft text-brand" : "text-fg hover:bg-surface-2",
                )}
              >
                <button
                  className="min-w-0 flex-1 truncate px-2.5 py-2 text-left"
                  onClick={() => navigate(`${base}/${c.id}`)}
                  disabled={busy}
                  title={c.title}
                >
                  {c.title}
                </button>
                <button
                  aria-label={`Delete ${c.title}`}
                  className="mr-1 rounded p-1 text-muted opacity-0 hover:text-danger focus:opacity-100 group-hover:opacity-100"
                  onClick={() => void remove(c.id)}
                  disabled={busy}
                >
                  <Trash2 aria-hidden className="size-3.5" />
                </button>
              </div>
            ))
          )}
        </div>
      </aside>

      {/* Conversation */}
      <section className="flex min-h-0 min-w-0 flex-col rounded-xl border border-border bg-surface">
        <header className="flex flex-wrap items-center gap-2 border-b border-border px-4 py-2.5">
          <Button
            variant="ghost"
            size="sm"
            className="md:hidden"
            aria-label="Show conversations"
            onClick={() => setListOpen((o) => !o)}
          >
            <PanelLeft aria-hidden className="size-4" />
          </Button>
          <h1 className="max-w-[12rem] truncate text-sm font-semibold text-fg sm:max-w-none">
            {currentChat?.title ?? "New chat"}
          </h1>

          {/* A3.4: sandbox attach + Chat/Agent switch */}
          <div className="ml-auto flex items-center gap-2">
            <Select
              ariaLabel="Sandbox"
              value={sandboxID}
              onValueChange={(v) => void attach(v)}
              disabled={attaching || chatHasRuns || sandboxRows.length === 0}
              placeholder={sandboxID ? undefined : "Gắn sandbox"}
              className="h-7 max-w-44 font-mono text-[11px]"
              options={
                sandboxID && !sandboxRows.some((s) => s.id === sandboxID)
                  ? [{ value: sandboxID, label: `${sandboxID.slice(-6)} (đã xoá)` }]
                  : sandboxRows.map((s) => ({
                      value: s.id,
                      label: `…${s.id.slice(-6)} · ${s.status}`,
                    }))
              }
            />
            <div
              role="group"
              aria-label="Chat mode"
              className="flex overflow-hidden rounded-[var(--radius)] border border-border text-xs"
            >
              <button
                aria-pressed={mode === "chat"}
                className={cn(
                  "px-2.5 py-1.5 transition-colors",
                  mode === "chat" ? "bg-brand-soft text-brand" : "text-muted hover:bg-surface-2",
                )}
                onClick={() => setMode("chat")}
              >
                Chat
              </button>
              <button
                aria-pressed={mode === "agent"}
                className={cn(
                  "px-2.5 py-1.5 transition-colors",
                  mode === "agent" ? "bg-brand-soft text-brand" : "text-muted hover:bg-surface-2",
                )}
                onClick={() => setMode("agent")}
              >
                Agent
              </button>
            </div>
          </div>
          {availableModels.length > 1 ? (
            <Select
              ariaLabel="Model"
              value={selectedModel || status.data?.model || ""}
              onValueChange={setSelectedModel}
              options={availableModels.map((m) => ({ value: m, label: m }))}
              className="h-7 font-mono text-[11px]"
            />
          ) : status.data?.model ? (
            <span className="shrink-0 whitespace-nowrap rounded-full bg-surface-2 px-2 py-0.5 font-mono text-[11px] text-muted">
              {selectedModel || status.data.model}
            </span>
          ) : null}
        </header>

        {externalRun && (
          <p className="border-b border-border bg-warning/10 px-4 py-1.5 text-xs text-warning" data-testid="external-run-banner">
            Agent đang chạy (từ API) trên sandbox của hội thoại này — ô nhập tạm khoá.
          </p>
        )}

        <div ref={scrollRef} className="min-h-0 flex-1 space-y-5 overflow-y-auto px-4 py-5" aria-live="polite">
          {disabled ? (
            <div className="mx-auto max-w-md pt-10 text-center">
              <Bot aria-hidden className="mx-auto size-8 text-muted" />
              <p className="mt-3 text-sm font-medium text-fg">Chat is not configured yet</p>
              <p className="mt-1 text-sm text-muted">
                The operator needs to set <code className="font-mono">PORTAL_DEFAULT_LLM_API_KEY</code> on the
                Portal deployment.
              </p>
            </div>
          ) : empty ? (
            <div className="mx-auto max-w-lg pt-8 text-center">
              <Bot aria-hidden className="mx-auto size-9 text-brand" />
              <p className="mt-3 text-base font-semibold text-fg">Hôm nay bạn muốn làm gì?</p>
              <p className="mt-1 text-sm text-muted">
                {mode === "agent"
                  ? "Gắn sandbox, bật Agent rồi giao việc — agent làm trực tiếp trong sandbox."
                  : "Hỏi về code, website hay sandbox. Trợ lý chưa chạy được code."}
              </p>
              <div className="mt-5 grid gap-2">
                {suggestions.map((s) => (
                  <button
                    key={s}
                    onClick={() => void send(s)}
                    className="rounded-[var(--radius)] border border-border px-3 py-2 text-left text-sm text-fg transition-colors hover:bg-surface-2"
                  >
                    {s}
                  </button>
                ))}
              </div>
            </div>
          ) : messages.isPending && chatId && !pending ? (
            <div className="space-y-3">
              <Skeleton className="h-12 w-2/3" />
              <Skeleton className="ml-auto h-10 w-1/2" />
            </div>
          ) : (
            <>
              {shown.map((m) => (
                <Bubble key={m.id} role={m.role} content={m.content} />
              ))}
              {runList.map((r) => (
                <AgentRunBlock key={r.id} orgID={org.id} run={r} onContinue={(p) => setDraft(p)} />
              ))}
              {pending && (
                <>
                  <Bubble role="user" content={pending.user} />
                  <Bubble role="assistant" content={pending.reply} streaming />
                </>
              )}
            </>
          )}
        </div>

        <form
          className="border-t border-border p-3"
          onSubmit={(e) => {
            e.preventDefault();
            void send(draft);
          }}
        >
          <div className="flex items-end gap-2 rounded-[var(--radius)] border border-border bg-bg p-2 focus-within:ring-2 focus-within:ring-[var(--ring)]">
            <textarea
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              onKeyDown={onKey}
              rows={1}
              maxLength={16000}
              placeholder={composerLocked && mode === "agent" ? composerHint : disabled ? "Chat is not configured" : composerHint}
              aria-label="Message"
              disabled={composerLocked}
              className="max-h-48 min-h-9 min-w-0 flex-1 resize-none bg-transparent focus-visible:shadow-none px-1.5 py-1.5 text-sm text-fg outline-none placeholder:text-muted/70 field-sizing-content"
            />
            {mode === "agent" && activeRun ? (
              <Button
                variant="secondary"
                aria-label="Stop"
                onClick={async () => {
                  try {
                    await api.cancelAgentRun(org.id, activeRun.id);
                    await qc.invalidateQueries({ queryKey: ["agent-runs", org.id] });
                  } catch (err) {
                    toast("error", err instanceof Error ? err.message : String(err));
                  }
                }}
              >
                <Square aria-hidden className="size-4" />
              </Button>
            ) : (
              <Button type="submit" className="shrink-0" aria-label="Send" disabled={composerLocked || !draft.trim()}>
                <SendHorizontal aria-hidden className="size-4" />
              </Button>
            )}
          </div>
        </form>
      </section>
    </div>
  );
}

// One agent run: the prompt as a user bubble + the run card beneath it.
function AgentRunBlock({ orgID, run, onContinue }: { orgID: string; run: AgentRun; onContinue: (p: string) => void }) {
  return (
    <>
      <Bubble role="user" content={run.prompt} chip="Agent" />
      <AgentRunCard orgID={orgID} run={run} onContinue={onContinue} />
    </>
  );
}

function Bubble({
  role,
  content,
  streaming,
  chip,
}: {
  role: string;
  content: string;
  streaming?: boolean;
  chip?: string;
}) {
  const mine = role === "user";
  return (
    <div className={cn("flex gap-3", mine && "flex-row-reverse")}>
      <span
        className={cn(
          "flex size-7 shrink-0 items-center justify-center rounded-full",
          mine ? "bg-surface-2 text-muted" : "bg-brand text-brand-fg",
        )}
      >
        {mine ? <User aria-hidden className="size-4" /> : <Bot aria-hidden className="size-4" />}
      </span>
      <div
        className={cn(
          "min-w-0 max-w-[85%] rounded-xl px-3.5 py-2.5",
          mine ? "bg-brand-soft text-fg" : "border border-border bg-surface",
        )}
      >
        {chip && (
          <span className="mb-1 inline-block rounded-full bg-surface-2 px-2 py-0.5 text-[10px] font-semibold tracking-wide text-muted uppercase">
            {chip}
          </span>
        )}
        {mine ? (
          <p className="whitespace-pre-wrap break-words text-sm text-fg">{content}</p>
        ) : content ? (
          <Markdown text={content} />
        ) : streaming ? (
          <Loader2 aria-label="Thinking" className="size-4 animate-spin text-muted" />
        ) : null}
      </div>
    </div>
  );
}
