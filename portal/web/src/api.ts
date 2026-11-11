// api.ts — BFF client helpers. Same-origin cookies authenticate the
// dashboard (BFF model, PORTAL_PLAN 3.1); the SPA echoes the CSRF
// cookie back as X-CSRF-Token on every state-changing call.

export type User = { id: string; email: string; name: string; created_at: string };
export type Org = { id: string; slug: string; name: string; plan_id: string };
export type Member = { org_id: string; user_id: string; role: string; email: string; name: string };
export type Project = { id: string; org_id: string; slug: string; name: string };
export type AuditEvent = {
  id: string;
  actor_type: string;
  actor_id: string;
  action: string;
  target: string;
  metadata: string;
  occurred_at: string;
};

export type AuthMethods = { sso: boolean; password: boolean };
export type Me = { user: User; orgs: Org[] };

// API keys (PORTAL_PLAN 3.2): the stored view never carries the
// secret — only the create response does, exactly once.
export type ApiKey = {
  id: string;
  key_id: string;
  name: string;
  scopes: string[];
  project_id: string | null;
  expires_at: string | null;
  last_used_at: string | null;
  revoked_at: string | null;
  created_at: string;
};
export type ApiKeyCreateBody = {
  name: string;
  scopes: string[];
  project_id?: string;
  expires_in_days?: number;
};

// Chat MVP: conversations are private to their author.
export type ChatStatus = { enabled: boolean; model: string };
export type ChatConversation = { id: string; title: string; created_at: string; updated_at: string };
export type ChatMessage = { id: string; role: "user" | "assistant"; content: string; created_at: string };

export type ChatStreamHandlers = {
  onUser?: (m: ChatMessage) => void;
  onDelta: (text: string) => void;
  onDone: (m: ChatMessage, title: string) => void;
  onError: (code: string, message: string) => void;
};

// Sandboxes (PORTAL_PLAN_V2 P4): the live view comes from sandboxd via
// the BFF gateway; project_id is the Portal-side pointer.
export type SandboxResources = { cpus: number; memory_mb: number };
export type SandboxPreview = { url: string; port: number; status: string };
export type Sandbox = {
  id: string;
  status: string;
  image: string;
  command?: string[];
  resources: SandboxResources;
  egress: string[];
  egress_mode: string;
  egress_deny?: string[];
  ports: number[];
  preview?: SandboxPreview;
  node?: string;
  error_message?: string;
  last_active_at?: string;
  created_at: string;
  updated_at?: string;
  project_id?: string;
};
export type SandboxList = {
  configured: boolean;
  sandboxes: Sandbox[];
  unreachable_nodes?: string[];
};
export type SandboxCreateBody = {
  image: string;
  project_id?: string;
  command?: string[];
  env?: Record<string, string>;
  resources?: { cpus: number; memory_mb: number };
  ports?: number[];
  egress?: { mode: string; allow?: string[] };
  workspace?: { repo_url: string; branch?: string };
};
export type ExecResult = {
  stdout: string;
  stderr: string;
  exit_code: number;
  timed_out?: boolean;
  truncated?: boolean;
  duration_ms?: number;
};

export class ApiError extends Error {
  status: number;
  code: string;
  constructor(status: number, code: string, message: string) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

function csrfToken(): string {
  const m = document.cookie.match(/(?:^|;\s*)portal_csrf=([^;]*)/);
  return m ? decodeURIComponent(m[1]) : "";
}

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: {
      ...(body !== undefined ? { "Content-Type": "application/json" } : {}),
      ...(method !== "GET" ? { "X-CSRF-Token": csrfToken() } : {}),
    },
    body: body !== undefined ? JSON.stringify(body) : undefined,
    credentials: "same-origin",
  });
  if (res.status === 204) return undefined as T;
  const data = (await res.json().catch(() => ({}))) as {
    error?: { code: string; message: string };
  };
  if (!res.ok) {
    throw new ApiError(res.status, data.error?.code ?? "unknown", data.error?.message ?? res.statusText);
  }
  return data as T;
}

export const api = {
  // Which sign-in surfaces the server offers right now (SSO needs an
  // OIDC provider; password is the local bootstrap admin).
  authMethods: async (): Promise<AuthMethods> => {
    const res = await fetch("/auth/methods");
    if (!res.ok) throw new ApiError(res.status, "unknown", res.statusText);
    return (await res.json()) as AuthMethods;
  },
  passwordLogin: (username: string, password: string) =>
    call<{ ok: boolean }>("POST", "/auth/login", { username, password }),
  me: () => call<{ user: User; orgs: Org[] }>("GET", "/api/me"),
  createOrg: (name: string) => call<Org>("POST", "/api/orgs", { name }),
  listProjects: (orgID: string) => call<Project[]>("GET", `/api/orgs/${orgID}/projects`),
  createProject: (orgID: string, name: string) =>
    call<Project>("POST", `/api/orgs/${orgID}/projects`, { name }),
  listMembers: (orgID: string) => call<Member[]>("GET", `/api/orgs/${orgID}/members`),
  setMemberRole: (orgID: string, userID: string, role: string) =>
    call<{ user_id: string; role: string }>("PATCH", `/api/orgs/${orgID}/members/${userID}`, { role }),
  createInvite: (orgID: string, email: string, role: string) =>
    call<{ url: string }>("POST", `/api/orgs/${orgID}/invites`, { email, role }),
  acceptInvite: (orgID: string, token: string) =>
    call<{ org_id: string; role: string }>("POST", `/api/orgs/${orgID}/invites/accept`, { token }),
  audit: (orgID: string) => call<AuditEvent[]>("GET", `/api/orgs/${orgID}/audit`),
  listApiKeys: (orgID: string) => call<ApiKey[]>("GET", `/api/orgs/${orgID}/api-keys`),
  createApiKey: (orgID: string, body: ApiKeyCreateBody) =>
    call<{ key: ApiKey; secret: string }>("POST", `/api/orgs/${orgID}/api-keys`, body),
  revokeApiKey: (orgID: string, keyID: string) =>
    call<{ key_id: string; status: string }>("DELETE", `/api/orgs/${orgID}/api-keys/${encodeURIComponent(keyID)}`),
  // Sandboxes (P4). Streams (logs follow, metrics) are consumed with
  // native EventSource against the same URLs — cookies ride along.
  listSandboxes: (orgID: string, projectID?: string) =>
    call<SandboxList>("GET", `/api/orgs/${orgID}/sandboxes${projectID ? `?project_id=${encodeURIComponent(projectID)}` : ""}`),
  createSandbox: (orgID: string, body: SandboxCreateBody) =>
    call<Sandbox>("POST", `/api/orgs/${orgID}/sandboxes`, body),
  getSandbox: (orgID: string, id: string) =>
    call<Sandbox>("GET", `/api/orgs/${orgID}/sandboxes/${encodeURIComponent(id)}`),
  execSandbox: (orgID: string, id: string, cmd: string) =>
    call<ExecResult>("POST", `/api/orgs/${orgID}/sandboxes/${encodeURIComponent(id)}/exec`, { cmd }),
  startSandbox: (orgID: string, id: string) =>
    call<void>("POST", `/api/orgs/${orgID}/sandboxes/${encodeURIComponent(id)}/start`),
  stopSandbox: (orgID: string, id: string) =>
    call<void>("POST", `/api/orgs/${orgID}/sandboxes/${encodeURIComponent(id)}/stop`),
  deleteSandbox: (orgID: string, id: string) =>
    call<void>("DELETE", `/api/orgs/${orgID}/sandboxes/${encodeURIComponent(id)}`),
  sandboxLogsUrl: (orgID: string, id: string, opts?: { tail?: number; follow?: boolean }) =>
    `/api/orgs/${orgID}/sandboxes/${encodeURIComponent(id)}/logs?tail=${opts?.tail ?? 500}${opts?.follow ? "&follow=true" : ""}`,
  sandboxMetricsStreamUrl: (orgID: string, id: string) =>
    `/api/orgs/${orgID}/sandboxes/${encodeURIComponent(id)}/metrics/stream?interval=2`,
  logout: () => call<void>("POST", "/auth/logout"),
  chatStatus: () => call<ChatStatus>("GET", "/api/chat/status"),
  listChats: (orgID: string) => call<ChatConversation[]>("GET", `/api/orgs/${orgID}/chats`),
  createChat: (orgID: string) => call<ChatConversation>("POST", `/api/orgs/${orgID}/chats`, {}),
  deleteChat: (orgID: string, chatID: string) => call<void>("DELETE", `/api/orgs/${orgID}/chats/${chatID}`),
  chatMessages: (orgID: string, chatID: string) =>
    call<ChatMessage[]>("GET", `/api/orgs/${orgID}/chats/${chatID}/messages`),
  sendChatMessage: (orgID: string, chatID: string, content: string, h: ChatStreamHandlers, signal?: AbortSignal) =>
    streamChat(`/api/orgs/${orgID}/chats/${chatID}/messages`, content, h, signal),
};

// streamChat POSTs one message and parses the SSE reply (events
// user / delta / done / error). EventSource cannot POST or send the
// CSRF header, so this reads the fetch body stream directly.
async function streamChat(path: string, content: string, h: ChatStreamHandlers, signal?: AbortSignal) {
  const res = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-CSRF-Token": csrfToken(), Accept: "text/event-stream" },
    body: JSON.stringify({ content }),
    credentials: "same-origin",
    signal,
  });
  if (!res.ok || !res.body) {
    const data = (await res.json().catch(() => ({}))) as { error?: { code: string; message: string } };
    throw new ApiError(res.status, data.error?.code ?? "unknown", data.error?.message ?? res.statusText);
  }
  const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
  let buf = "";
  let finished = false;
  const dispatch = (block: string) => {
    let event = "message";
    const data: string[] = [];
    for (const line of block.split("\n")) {
      if (line.startsWith("event:")) event = line.slice(6).trim();
      else if (line.startsWith("data:")) data.push(line.slice(5).trimStart());
    }
    if (data.length === 0) return;
    const payload = JSON.parse(data.join("\n")) as Record<string, unknown>;
    switch (event) {
      case "user":
        h.onUser?.(payload.message as ChatMessage);
        break;
      case "delta":
        h.onDelta(String(payload.text ?? ""));
        break;
      case "done":
        finished = true;
        h.onDone(payload.message as ChatMessage, String(payload.title ?? ""));
        break;
      case "error":
        finished = true;
        h.onError(String(payload.code ?? "error"), String(payload.message ?? "error"));
        break;
    }
  };
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += value.replace(/\r\n/g, "\n");
    let i: number;
    while ((i = buf.indexOf("\n\n")) >= 0) {
      dispatch(buf.slice(0, i));
      buf = buf.slice(i + 2);
    }
  }
  if (buf.trim()) dispatch(buf);
  if (!finished) h.onError("disconnected", "the connection closed before the reply finished");
}
