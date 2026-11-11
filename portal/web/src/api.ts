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
  logout: () => call<void>("POST", "/auth/logout"),
};
