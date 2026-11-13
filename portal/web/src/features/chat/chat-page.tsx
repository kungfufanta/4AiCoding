// Chat page (Chat MVP + A3.4 agent mode): talk to the platform model
// with replies streamed token by token, or — after attaching one
// sandbox — hand the job to the coding agent (docs/AGENT_PLAN_A3.md
// §10): collapsed tool cards, a result card with Revert/Continue, a
// Stop button and SSE that survives reloads.
// Conversations are private to the signed-in user.

import { useEffect, useRef, useState, type KeyboardEvent } from "react";
import { useNavigate, useParams } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Bot,
  ChevronDown,
  Loader2,
  MessageSquarePlus,
  PanelLeft,
  SendHorizontal,
  Square,
  Trash2,
  User,
} from "lucide-react";
import { api, ApiError, type AgentRun, type ChatMessage } from "../../api";
import { useChatMessages, useChats, useChatStatus } from "../../lib/queries";
import { useSandboxes, statusTone } from "../sandboxes/hooks";
import { cn } from "../../lib/cn";
import { useShell } from "../../layout/app-shell";
import { Button } from "../../ui/button";
import { useConfirm } from "../../ui/dialog";
import { Dropdown, DropdownItem, DropdownLabel } from "../../ui/dropdown";
import { Select } from "../../ui/select";
import { Skeleton } from "../../ui/skeleton";
import { useToast } from "../../ui/toast";
import { Markdown } from "./markdown";
import { RunView } from "./agent-run";
import { useAgentRun } from "./use-agent-run";

type Pending = { user: string; reply: string };
type Mode = "chat" | "agent";

const SUGGESTIONS = [
  "Lên ý tưởng landing page cho quán cà phê",
  "Viết hàm debounce bằng TypeScript",
  "Giải thích sandbox là gì và dùng để làm gì",
];

const DOT: Record<string, string> = {
  success: "bg-success",
  warning: "bg-warning",
  danger: "bg-danger",
  neutral: "bg-muted",
};

export function ChatPage() {
  const { org, me, isAdmin } = useShell();
  const { chatId } = useParams();
  const navigate = useNavigate();
  const qc = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();

  const status = useChatStatus(org.id);
  const chats = useChats(org.id);
  const messages = useChatMessages(org.id, chatId);
  const sandboxes = useSandboxes(org.id);

  const [draft, setDraft] = useState("");
  const [pending, setPending] = useState<Pending | null>(null);
  const [listOpen, setListOpen] = useState(false);
  const [mode, setMode] = useState<Mode>("chat");
  const [agentPending, setAgentPending] = useState(false);
  const abortRef = useRef<AbortController | null>(null);
  const scrollRef = useRef<HTMLDivElement>(null);
  const base = `/orgs/${org.slug}/chat`;

  useEffect(() => () => abortRef.current?.abort(), []);
  useEffect(() => {
    setListOpen(false);
    setMode("chat"); // mode is per chat — a fresh chat starts in Chat
  }, [chatId]);

  const chat = chats.data?.find((c) => c.id === chatId);
  const attached = chat?.sandbox_id ?? "";
  const sbx = sandboxes.data?.sandboxes.find((s) => s.id === attached);

  // The chat's runs (newest first); only the latest one gets the full
  // run block — full history views are A4.
  const runsQuery = useQuery({
    queryKey: ["agent-runs", org.id, chatId],
    queryFn: () => api.listChatAgentRuns(org.id, chatId!),
    enabled: org.id !== "" && !!chatId,
  });
  const latestRun: AgentRun | undefined = runsQuery.data?.[0];
  const ownActive = latestRun?.status === "queued" || latestRun?.status === "running";
  // Bumped on revert: the SSE stream already closed at done, so the
  // reverted event only reaches the UI through a fresh replay.
  const [runRefresh, setRunRefresh] = useState(0);
  const { timeline } = useAgentRun(org.id, latestRun, runRefresh);

  // Runs others started on the pinned sandbox (the /v1 API) — the
  // composer locks while such a run holds the sandbox (plan §10).
  const sandboxRuns = useQuery({
    queryKey: ["sandbox-agent-runs", org.id, attached],
    queryFn: () => api.listSandboxAgentRuns(org.id, attached),
    enabled: attached !== "",
    refetchInterval: 10_000,
  });
  const foreignActive = (sandboxRuns.data ?? []).some(
    (r) => (r.status === "queued" || r.status === "running") && (!r.conversation_id || r.conversation_id !== chatId),
  );

  const shown: ChatMessage[] = (messages.data ?? []).filter(
    (m) => !(latestRun && m.agent_run_id === latestRun.id),
  );
  // The run block belongs at the bottom only while the run is the
  // newest activity — after the user chats again it yields.
  const runIsLatest =
    !!latestRun && (!shown.length || shown[shown.length - 1].created_at <= latestRun.created_at);

  const doneStatus = timeline?.done?.status;
  useEffect(() => {
    if (!doneStatus) return;
    void qc.invalidateQueries({ queryKey: ["agent-runs", org.id, chatId] });
    void qc.invalidateQueries({ queryKey: ["chat-messages", org.id, chatId] });
    void qc.invalidateQueries({ queryKey: ["sandbox-agent-runs", org.id] });
  }, [doneStatus, qc, org.id, chatId]);

  useEffect(() => {
    const el = scrollRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [
    shown.length,
    pending?.reply,
    pending?.user,
    timeline?.text.length,
    timeline?.tools.length,
    timeline?.phase,
    latestRun?.id,
    runIsLatest,
  ]);

  const disabled = status.data?.enabled === false;
  const busy = pending !== null;
  const agentBusy = mode === "agent" && (agentPending || ownActive);
  const composerLocked =
    disabled ||
    (mode === "agent" && (!attached || foreignActive || agentBusy));
  // A2: per-message model picker. The options come from the effective
  // connection's allowed list (chat status); empty list = only the
  // connection's default model, so the picker hides.
  const availableModels = status.data?.models ?? [];
  const [selectedModel, setSelectedModel] = useState("");

  const attach = useMutation({
    mutationFn: async (sandboxID: string) => {
      let id = chatId;
      if (!id) {
        const created = await api.createChat(org.id);
        id = created.id;
        qc.setQueryData(["chat-messages", org.id, id], []);
        navigate(`${base}/${id}`);
      }
      await api.attachChatSandbox(org.id, id, sandboxID);
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["chats", org.id] });
      toast("success", "Đã gắn sandbox vào chat này.");
      setMode("agent");
    },
    onError: (err) => {
      toast("error", err instanceof ApiError ? err.message : String(err));
    },
  });

  const cancelRun = useMutation({
    mutationFn: () => api.cancelAgentRun(org.id, latestRun!.id),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ["agent-runs", org.id, chatId] }),
    onError: (err) => toast("error", err instanceof ApiError ? err.message : String(err)),
  });

  const revertRun = useMutation({
    mutationFn: () => api.revertAgentRun(org.id, latestRun!.id),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["agent-runs", org.id, chatId] });
      void qc.invalidateQueries({ queryKey: ["sandbox-agent-runs", org.id] });
      setRunRefresh((k) => k + 1); // replay the events for reverted
      toast("success", "Đã hoàn tác: sandbox trở về trạng thái trước run.");
    },
    onError: (err) => toast("error", err instanceof ApiError ? err.message : String(err)),
  });

  const canRevert =
    !!latestRun &&
    !latestRun.reverted_at &&
    (latestRun.status === "succeeded" || latestRun.status === "failed") &&
    (isAdmin || latestRun.created_by === me.user.id) &&
    ((timeline?.done?.result?.files_changed.length ?? latestRun.result?.files_changed.length ?? 0) > 0);

  async function revertWithConfirm() {
    if (!latestRun) return;
    const files = timeline?.done?.result?.files_changed ?? latestRun.result?.files_changed ?? [];
    const added = files.filter((f) => f.status === "A").length;
    const edited = files.length - added;
    const ok = await confirm({
      title: "Hoàn tác run này?",
      description:
        `Sandbox sẽ trở về checkpoint trước run: ${added} file đã thêm sẽ bị xoá` +
        (edited > 0 ? `, ${edited} file đã sửa sẽ về nội dung cũ` : "") +
        ". File bạn tạo trước run không bị đụng.",
      confirmLabel: "Hoàn tác",
      danger: true,
    });
    if (ok) revertRun.mutate();
  }

  async function send(text: string) {
    const content = text.trim();
    if (!content || busy) return;
    if (mode === "agent") return sendAgentRun(content);
    setDraft("");
    setPending({ user: content, reply: "" });

    let id = chatId;
    try {
      if (!id) {
        const created = await api.createChat(org.id);
        id = created.id;
        qc.setQueryData(["chat-messages", org.id, id], []);
        navigate(`${base}/${id}`, { replace: false });
      }
    } catch (err) {
      setPending(null);
      setDraft(content);
      toast("error", err instanceof Error ? err.message : String(err));
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
        id!,
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

  async function sendAgentRun(prompt: string) {
    if (!prompt || agentBusy || mode !== "agent") return;
    let id = chatId;
    try {
      if (!id) {
        const created = await api.createChat(org.id);
        id = created.id;
        qc.setQueryData(["chat-messages", org.id, id], []);
        navigate(`${base}/${id}`, { replace: false });
      }
    } catch (err) {
      toast("error", err instanceof Error ? err.message : String(err));
      return;
    }
    setDraft("");
    setAgentPending(true);
    try {
      await api.createAgentRun(org.id, id, { prompt, model: selectedModel || undefined });
      await qc.invalidateQueries({ queryKey: ["agent-runs", org.id, id] });
    } catch (err) {
      setDraft(prompt);
      toast("error", err instanceof ApiError ? err.message : String(err));
    } finally {
      setAgentPending(false);
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

  const empty = !chatId && !pending && !latestRun;

  const modeSwitch = (
    <div className="flex shrink-0 rounded-[var(--radius)] border border-border p-0.5" role="group" aria-label="Chế độ">
      <button
        type="button"
        aria-pressed={mode === "chat"}
        onClick={() => setMode("chat")}
        className={cn(
          "rounded-sm px-2 py-0.5 text-xs transition-colors",
          mode === "chat" ? "bg-brand-soft text-brand" : "text-muted hover:text-fg",
        )}
      >
        Chat
      </button>
      <button
        type="button"
        aria-pressed={mode === "agent"}
        disabled={!attached}
        title={attached ? "Giao việc cho agent" : "Gắn sandbox để bật Agent"}
        onClick={() => setMode("agent")}
        className={cn(
          "rounded-sm px-2 py-0.5 text-xs transition-colors",
          mode === "agent" ? "bg-brand-soft text-brand" : "text-muted hover:text-fg",
          !attached && "cursor-not-allowed opacity-50 hover:text-muted",
        )}
      >
        Agent
      </button>
    </div>
  );

  const sandboxPicker = attached ? (
    sbx ? (
      <Dropdown trigger={
        <Button variant="secondary" size="sm" className="max-w-44 font-mono text-[11px]" aria-label="Sandbox của chat">
          <span aria-hidden className={cn("size-1.5 shrink-0 rounded-full", DOT[statusTone(sbx.status)])} />
          <span className="truncate">{attached}</span>
          <ChevronDown aria-hidden className="size-3.5 shrink-0 text-muted" />
        </Button>
      }>
        <DropdownLabel>Sandbox đã gắn</DropdownLabel>
        <DropdownItem disabled>
          <span aria-hidden className={cn("size-1.5 shrink-0 rounded-full", DOT[statusTone(sbx.status)])} />
          <span className="truncate font-mono text-xs">{attached}</span>
          <span className="ml-auto text-xs text-muted">{sbx.status}</span>
        </DropdownItem>
        {runsQuery.data && runsQuery.data.length > 0 && (
          <p className="max-w-52 px-2.5 py-1.5 text-[11px] text-muted">Chat đã có run — sandbox bị khoá (tạo chat mới để đổi).</p>
        )}
      </Dropdown>
    ) : (
      <span className="inline-flex max-w-44 items-center gap-1.5 rounded-[var(--radius)] border border-border px-2 py-1 font-mono text-[11px] text-muted">
        <Loader2 aria-hidden className="size-3 animate-spin" />
        <span className="truncate">{attached}</span>
      </span>
    )
  ) : (
    <Dropdown trigger={
      <Button variant="secondary" size="sm" aria-label="Gắn sandbox" disabled={attach.isPending}>
        {attach.isPending ? <Loader2 aria-hidden className="size-3.5 animate-spin" /> : null}
        Gắn sandbox
        <ChevronDown aria-hidden className="size-3.5 text-muted" />
      </Button>
    }>
      <DropdownLabel>Sandbox của org</DropdownLabel>
      {(sandboxes.data?.sandboxes ?? []).map((s) => (
        <DropdownItem key={s.id} onSelect={() => attach.mutate(s.id)}>
          <span aria-hidden className={cn("size-1.5 shrink-0 rounded-full", DOT[statusTone(s.status)])} />
          <span className="truncate font-mono text-xs">{s.id}</span>
          <span className="ml-auto text-xs text-muted">{s.status}</span>
        </DropdownItem>
      ))}
      {(sandboxes.data?.sandboxes ?? []).length === 0 && (
        <p className="max-w-52 px-2.5 py-2 text-[11px] text-muted">
          Chưa có sandbox nào — tạo ở trang Sandboxes rồi quay lại gắn vào chat.
        </p>
      )}
    </Dropdown>
  );

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
          <h1 className="truncate text-sm font-semibold text-fg">
            {chat?.title ?? "New chat"}
          </h1>
          <div className="ml-auto flex flex-wrap items-center gap-2">
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
            {sandboxPicker}
            {modeSwitch}
          </div>
        </header>

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
          ) : mode === "agent" && !attached && empty ? (
            <div className="mx-auto max-w-lg pt-8 text-center">
              <Bot aria-hidden className="mx-auto size-9 text-brand" />
              <p className="mt-3 text-base font-semibold text-fg">Giao việc cho agent</p>
              <p className="mt-1 text-sm text-muted">
                Gắn một sandbox vào chat (nút “Gắn sandbox” ở trên) rồi mô tả việc — agent làm trực tiếp trong
                sandbox, mỗi run có checkpoint để hoàn tác.
              </p>
            </div>
          ) : empty ? (
            <div className="mx-auto max-w-lg pt-8 text-center">
              <Bot aria-hidden className="mx-auto size-9 text-brand" />
              <p className="mt-3 text-base font-semibold text-fg">Hôm nay bạn muốn làm gì?</p>
              <p className="mt-1 text-sm text-muted">Hỏi về code, website hay sandbox. Trợ lý chưa chạy được code.</p>
              <div className="mt-5 grid gap-2">
                {SUGGESTIONS.map((s) => (
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
              {pending && (
                <>
                  <Bubble role="user" content={pending.user} />
                  <Bubble role="assistant" content={pending.reply} streaming />
                </>
              )}
              {runIsLatest && latestRun && timeline && (
                <>
                  <Bubble role="user" content={latestRun.prompt} />
                  <RunView
                    run={latestRun}
                    timeline={timeline}
                    canRevert={canRevert}
                    reverting={revertRun.isPending}
                    canceling={cancelRun.isPending}
                    onCancel={() => cancelRun.mutate()}
                    onRevert={() => void revertWithConfirm()}
                    onContinue={() => {
                      setMode("agent");
                      setDraft(latestRun.prompt);
                    }}
                  />
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
          {foreignActive && (
            <p className="pb-2 text-xs text-warning" data-testid="foreign-busy">
              Agent đang chạy trên sandbox này (từ API) — ô nhập khoá cho tới khi run đó xong.
            </p>
          )}
          {mode === "agent" && !attached && (
            <p className="pb-2 text-xs text-muted">Gắn sandbox để giao việc cho agent.</p>
          )}
          <div className="flex items-end gap-2 rounded-[var(--radius)] border border-border bg-bg p-2 focus-within:ring-2 focus-within:ring-[var(--ring)]">
            <textarea
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              onKeyDown={onKey}
              rows={1}
              maxLength={16000}
              placeholder={
                disabled
                  ? "Chat is not configured"
                  : mode === "agent"
                    ? attached
                      ? "Mô tả việc cho agent… (Enter để gửi, Shift+Enter xuống dòng)"
                      : "Gắn sandbox để giao việc cho agent"
                    : "Nhắn tin… (Enter để gửi, Shift+Enter xuống dòng)"
              }
              aria-label="Message"
              disabled={composerLocked}
              className="max-h-48 min-h-9 min-w-0 flex-1 resize-none bg-transparent focus-visible:shadow-none px-1.5 py-1.5 text-sm text-fg outline-none placeholder:text-muted/70 field-sizing-content"
            />
            {mode === "agent" && ownActive ? (
              <Button
                variant="secondary"
                aria-label="Stop run"
                onClick={(e) => {
                  e.preventDefault();
                  cancelRun.mutate();
                }}
                disabled={cancelRun.isPending || latestRun?.cancel_requested}
              >
                {cancelRun.isPending || latestRun?.cancel_requested ? (
                  <Loader2 aria-hidden className="size-4 animate-spin" />
                ) : (
                  <Square aria-hidden className="size-4" />
                )}
              </Button>
            ) : busy ? (
              <Button variant="secondary" aria-label="Stop" onClick={() => abortRef.current?.abort()}>
                <Square aria-hidden className="size-4" />
              </Button>
            ) : (
              <Button
                type="submit"
                className="shrink-0"
                aria-label="Send"
                disabled={composerLocked || !draft.trim()}
              >
                {agentBusy ? <Loader2 aria-hidden className="size-4 animate-spin" /> : <SendHorizontal aria-hidden className="size-4" />}
              </Button>
            )}
          </div>
        </form>
      </section>
    </div>
  );
}

function Bubble({ role, content, streaming }: { role: string; content: string; streaming?: boolean }) {
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
