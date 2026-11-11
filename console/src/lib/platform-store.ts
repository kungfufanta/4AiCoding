import { create } from "zustand";
import { toast } from "sonner";
import {
  del as apiDel,
  get as apiGet,
  patch as apiPatch,
  post as apiPost,
  put as apiPut,
  type NetMode,
  type Overview,
  type PermGrant,
  type Sandbox as ApiSandbox,
  type User as ApiUser,
} from "@/console/api";
import {
  type ActivityItem,
  type FirewallRule,
  type Notification,
  type Perm,
  type PermMatrix,
  type Role,
  type RuleAction,
  type RuleKind,
  type Sandbox,
  type SandboxStatus,
  type User,
  type ViewKey,
  classifyRuleValue,
} from "./platform-data";

/* ────────────────────────────────────────────────────────────────────
   Platform store — same action surface the SBX template views consume,
   but every read/write goes to the real sandboxd /v1 API (the mock data
   layer in platform-data.ts only supplies types + pure helpers now).
   ──────────────────────────────────────────────────────────────────── */

type ApiRole = "admin" | "operator" | "viewer";
export type Me = { username: string; role: ApiRole };

/** /v1/monitoring response shape (operator-gated) */
interface MonResp {
  fleet?: { total: number; running: number; avg_cpu: number; mem_used_mb: number; mem_limit_mb: number };
  sandboxes: { id: string; cpu_percent: number; mem_usage_bytes: number; mem_limit_bytes: number; pids: number }[];
}

const roleFromApi = (r: string): Role => (r === "admin" ? "Admin" : r === "operator" ? "Developer" : "Viewer");
const roleToApi = (r: Role): ApiRole => (r === "Admin" ? "admin" : r === "Developer" ? "operator" : "viewer");
const statusFromApi = (s: string): SandboxStatus =>
  s === "running" ? "Running" : s === "creating" ? "Starting" : s === "error" ? "Error" : "Stopped";

function hashStr(s: string): number {
  let h = 2166136261;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return h >>> 0;
}

function fmtUptime(fromIso?: string): string {
  if (!fromIso) return "—";
  const ms = Date.now() - new Date(fromIso).getTime();
  if (!isFinite(ms) || ms < 0) return "—";
  const s = Math.floor(ms / 1000);
  const d = Math.floor(s / 86400);
  const hh = `${String(Math.floor((s % 86400) / 3600)).padStart(2, "0")}:${String(
    Math.floor((s % 3600) / 60),
  ).padStart(2, "0")}:${String(s % 60).padStart(2, "0")}`;
  return d > 0 ? `${d}d ${hh}` : hh;
}


function mapUser(u: ApiUser): User {
  const h = hashStr(u.username);
  return {
    id: u.id,
    name: u.username,
    role: roleFromApi(u.role),
    status: u.disabled ? "Suspended" : "Active",
    joined: (u.created_at ?? "").slice(0, 10),
    colorIdx: h % 11,
  };
}

function mapSandbox(s: ApiSandbox, stats?: { cpu: number; mem: number }): Sandbox {
  const status = statusFromApi(s.status);
  return {
    id: s.id,
    name: s.external?.project_id || s.id,
    image: s.image,
    status,
    vcpu: s.resources?.cpus || 4,
    memGb: Math.max(1, Math.round((s.resources?.memory_mb || 8192) / 1024)),
    env: s.egress_mode === "open" ? "Open (monitored)" : s.egress_mode === "none" ? "No network" : "Allow-list",
    host: s.node || "local", // fleet node it runs on (sandboxd keeps container IPs internal)
    ports: s.preview?.port ? [`${s.preview.port}:${s.preview.port}`] : (s.ports ?? []).map((p) => `${p}:${p}`),
    uptime: status === "Running" ? fmtUptime(s.created_at) : "—",
    cpuPct: status === "Running" ? Math.round(stats?.cpu ?? 0) : 0,
    memPct: status === "Running" ? Math.round(stats?.mem ?? 0) : 0,
  };
}

/** derive per-sandbox firewall rows from the real egress allow/deny lists */
function firewallFrom(s: ApiSandbox): { rules: FirewallRule[]; policy: RuleAction } {
  const date = (s.created_at ?? "").slice(0, 10) || "—";
  const rules: FirewallRule[] = [];
  (s.egress ?? []).forEach((v, i) =>
    rules.push({
      id: `fwr-${s.id}-a-${i}`,
      kind: classifyRuleValue(v) ?? "domain",
      value: v,
      action: "allow",
      enabled: true,
      hits: 0,
      addedBy: "policy",
      addedAt: date,
    }),
  );
  (s.egress_deny ?? []).forEach((v, i) =>
    rules.push({
      id: `fwr-${s.id}-d-${i}`,
      kind: classifyRuleValue(v) ?? "domain",
      value: v,
      action: "block",
      enabled: true,
      hits: 0,
      addedBy: "policy",
      addedAt: date,
    }),
  );
  return { rules, policy: s.egress_mode === "open" ? "allow" : "block" };
}

// kept for when an ACL backend lands (GET /v1/permissions)
export function grantsToMatrix(grants: PermGrant[]): PermMatrix {
  const m: PermMatrix = {};
  for (const g of grants) {
    m[g.user_id] = m[g.user_id] ?? {};
    m[g.user_id][g.sandbox_id] = { r: g.r, w: g.w, x: g.x };
  }
  return m;
}

function matrixToGrants(m: PermMatrix): PermGrant[] {
  const out: PermGrant[] = [];
  for (const [user_id, row] of Object.entries(m))
    for (const [sandbox_id, p] of Object.entries(row))
      if (p.r || p.w || p.x) out.push({ user_id, sandbox_id, r: p.r || p.w || p.x, w: p.w, x: p.x });
  return out;
}

function mapActivity(o: Overview): ActivityItem[] {
  return (o.audit ?? []).slice(0, 8).map((a) => ({
    time: new Date(a.at).toLocaleTimeString("en-GB", { hour12: false }),
    text: `${a.actor} ${a.action}${a.target ? ` → ${a.target}` : ""}${a.detail ? ` — ${a.detail}` : ""}`,
    kind: /login|auth|password/.test(a.action)
      ? "access"
      : /create|start|restart|deploy/.test(a.action)
        ? "deploy"
        : /stop|suspend|delete|revoke/.test(a.action)
          ? "warn"
          : /error|denied|fail/.test(a.action)
            ? "error"
            : "info",
  }));
}

function mapNotifications(o: Overview): Notification[] {
  const notes: Notification[] = [];
  if (o.approvals_open > 0)
    notes.push({
      id: "n-approvals",
      tone: "info",
      title: "Access request",
      body: `${o.approvals_open} gated action(s) waiting for an admin decision.`,
      time: "now",
    });
  for (const s of (o.recent ?? []).filter((x) => x.status === "creating").slice(0, 2))
    notes.push({ id: `n-${s.id}`, tone: "warn", title: `${s.id} starting`, body: `${s.image} is provisioning.`, time: "1m" });
  if (o.net_events_24h.blocked > 0)
    notes.push({
      id: "n-egress",
      tone: "error",
      title: "Egress blocked",
      body: `${o.net_events_24h.blocked} outbound request(s) blocked in the last 24h.`,
      time: "24h",
    });
  notes.push({
    id: "n-audit",
    tone: "success",
    title: "Audit pipeline healthy",
    body: `${o.net_events_24h.total} network event(s) processed in the last 24h.`,
    time: "24h",
  });
  return notes.slice(0, 5);
}

const emptyPerm: Perm = { r: false, w: false, x: false };

interface PlatformState {
  ready: boolean;
  error: string | null;
  me: Me | null;
  users: User[];
  sandboxes: Sandbox[];
  /** working copy edited in the permission matrix */
  perms: PermMatrix;
  /** last saved baseline — used by Reset & dirty check */
  savedPerms: PermMatrix;

  /** per-sandbox egress firewall rules (allow / block domains & IPs) */
  firewall: Record<string, FirewallRule[]>;
  /** per-sandbox default egress policy */
  egressPolicy: Record<string, RuleAction>;
  /** sandbox opened in the detail (network / firewall) view */
  selectedSandboxId: string | null;

  /** live audit tail + derived notifications (top bar) */
  activity: ActivityItem[];
  notifications: Notification[];

  /** live fleet metrics from /v1/monitoring (running sandboxes only) */
  fleet: { running: number; avgCpu: number; memUsedMb: number; memLimitMb: number; pids: number } | null;
  /** real counters from /v1/overview + /v1/nodes behind the Overview KPIs */
  overviewNums: {
    approvalsOpen: number;
    netTotal: number;
    netBlocked: number;
    portsExposed: number;
    nodesOnline: number;
    nodesTotal: number;
  } | null;

  refreshAll: () => Promise<void>;
  /** light poll of /v1/monitoring — keeps KPIs + charts on live values */
  pollLive: () => Promise<void>;
  signOut: () => Promise<void>;

  setUserRole: (userId: string, role: Role) => void;
  setUserStatus: (userId: string, status: User["status"]) => void;
  deleteUser: (userId: string) => void;
  addUser: (u: { name: string; password?: string; role: Role }) => void;

  setSandboxStatus: (id: string, status: SandboxStatus) => void;
  /** POST /v1/sandboxes/:id/restart — real restart with lazy lifecycle */
  restartSandbox: (id: string) => void;

  openSandbox: (id: string) => void;
  closeSandbox: () => void;

  addRule: (sbxId: string, r: { kind: RuleKind; value: string; action: RuleAction }) => void;
  removeRule: (sbxId: string, ruleId: string) => void;
  toggleRule: (sbxId: string, ruleId: string) => void;
  setEgressPolicy: (sbxId: string, policy: RuleAction) => void;
  /** live tick — bumps hit counters of enabled rules while the sandbox runs */
  tickFirewall: (sbxId: string) => void;

  togglePerm: (userId: string, sandboxId: string, kind: keyof Perm) => void;
  resetPerms: () => void;
  savePerms: () => void;
}

/** persist a sandbox's rule list as the real egress allow/deny arrays */
async function pushEgress(sbxId: string, rules: FirewallRule[], mode?: NetMode) {
  const allow = rules.filter((r) => r.action === "allow" && r.enabled).map((r) => r.value);
  const deny = rules.filter((r) => r.action === "block" && r.enabled).map((r) => r.value);
  // sandboxd: PUT /v1/sandboxes/{id}/network/egress {mode, allow, deny}
  const cur: NetMode = usePlatform.getState().egressPolicy[sbxId] === "allow" ? "open" : "allowlist";
  await apiPut(`/v1/sandboxes/${sbxId}/network/egress`, { mode: mode ?? cur, allow, deny });
}

const BACKOFF_MS = 3500; // lazy creating→running lifecycle on the API

export const usePlatform = create<PlatformState>((set, get) => ({
  ready: false,
  error: null,
  me: null,
  users: [],
  sandboxes: [],
  perms: {},
  savedPerms: {},
  firewall: {},
  egressPolicy: {},
  selectedSandboxId: null,
  activity: [],
  notifications: [],
  fleet: null,
  overviewNums: null,

  refreshAll: async () => {
    try {
      const [sbxR, usersR] = await Promise.all([
        apiGet<{ sandboxes: ApiSandbox[] }>("/v1/sandboxes"),
        apiGet<{ users: ApiUser[] }>("/v1/users").catch(() => ({ users: [] as ApiUser[] })),
      ]);

      // per-sandbox cpu/mem/pids + fleet totals — operator only; viewers see nulls
      const stats = new Map<string, { cpu: number; mem: number }>();
      let fleet: PlatformState["fleet"] = null;
      try {
        const mon = await apiGet<MonResp>("/v1/monitoring");
        for (const s of mon.sandboxes) {
          stats.set(s.id, {
            cpu: s.cpu_percent,
            mem: s.mem_limit_bytes ? (s.mem_usage_bytes / s.mem_limit_bytes) * 100 : 0,
          });
        }
        if (mon.fleet)
          fleet = {
            running: mon.fleet.running,
            avgCpu: mon.fleet.avg_cpu,
            memUsedMb: mon.fleet.mem_used_mb,
            memLimitMb: mon.fleet.mem_limit_mb,
            pids: mon.sandboxes.reduce((a, s) => a + (s.pids ?? 0), 0),
          };
      } catch {
        /* viewer role or none running */
      }

      // permission matrix — operator only
      // no ACL backend yet (/v1/permissions does not exist): keep the matrix empty
      const matrix: PermMatrix = {};

      // audit tail + notifications + real KPI counters
      let activity: ActivityItem[] = [];
      let notifications: Notification[] = [];
      let overviewNums: PlatformState["overviewNums"] = null;
      try {
        const o = await apiGet<Overview>("/v1/overview");
        activity = mapActivity(o);
        notifications = mapNotifications(o);
        overviewNums = {
          approvalsOpen: o.approvals_open,
          netTotal: o.net_events_24h.total,
          netBlocked: o.net_events_24h.blocked,
          portsExposed: o.ports_exposed,
          nodesOnline: 0,
          nodesTotal: 0,
        };
      } catch {
        /* viewer role */
      }
      try {
        const n = await apiGet<{ nodes: { status: string }[] }>("/v1/nodes");
        if (overviewNums) {
          overviewNums.nodesTotal = n.nodes.length;
          overviewNums.nodesOnline = n.nodes.filter((x) => x.status === "online").length;
        }
      } catch {
        /* nodes unseen by this role */
      }

      const firewall: Record<string, FirewallRule[]> = {};
      const egressPolicy: Record<string, RuleAction> = {};
      for (const s of sbxR.sandboxes) {
        const fw = firewallFrom(s);
        firewall[s.id] = fw.rules;
        egressPolicy[s.id] = fw.policy;
      }

      set({
        ready: true,
        error: null,
        users: usersR.users.map(mapUser),
        sandboxes: sbxR.sandboxes.map((s) => mapSandbox(s, stats.get(s.id))),
        perms: matrix,
        savedPerms: matrix,
        firewall,
        egressPolicy,
        activity,
        notifications,
        fleet,
        overviewNums,
      });
    } catch (e) {
      set({ ready: true, error: e instanceof Error ? e.message : String(e) });
    }
  },

  pollLive: async () => {
    try {
      const mon = await apiGet<MonResp>("/v1/monitoring");
      const stats = new Map<string, { cpu: number; mem: number; pids: number }>();
      for (const s of mon.sandboxes)
        stats.set(s.id, {
          cpu: s.cpu_percent,
          mem: s.mem_limit_bytes ? (s.mem_usage_bytes / s.mem_limit_bytes) * 100 : 0,
          pids: s.pids ?? 0,
        });
      set((s) => ({
        fleet: mon.fleet
          ? {
              running: mon.fleet.running,
              avgCpu: mon.fleet.avg_cpu,
              memUsedMb: mon.fleet.mem_used_mb,
              memLimitMb: mon.fleet.mem_limit_mb,
              pids: mon.sandboxes.reduce((a, x) => a + (x.pids ?? 0), 0),
            }
          : s.fleet,
        sandboxes: s.sandboxes.map((b) => {
          const st = stats.get(b.id);
          if (st) return { ...b, cpuPct: Math.round(st.cpu), memPct: Math.round(st.mem) };
          return b.status === "Running" ? { ...b, cpuPct: 0, memPct: 0 } : b;
        }),
      }));
    } catch {
      /* viewer role or transient error — keep last known values */
    }
  },

  signOut: async () => {
    try {
      await apiPost("/v1/auth/logout");
    } finally {
      window.location.reload();
    }
  },

  setUserRole: (userId, role) => {
    set((s) => ({ users: s.users.map((u) => (u.id === userId ? { ...u, role } : u)) }));
    apiPatch(`/v1/users/${userId}`, { role: roleToApi(role) }).catch(() => {
      toast.error("Could not update the role — reverting.");
      void get().refreshAll();
    });
  },

  setUserStatus: (userId, status) => {
    const disabled = status === "Suspended";
    set((s) => ({ users: s.users.map((u) => (u.id === userId ? { ...u, status } : u)) }));
    apiPatch(`/v1/users/${userId}`, { disabled }).catch(() => {
      toast.error("Could not update the user — reverting.");
      void get().refreshAll();
    });
  },

  deleteUser: (userId) => {
    set((s) => ({
      users: s.users.filter((u) => u.id !== userId),
      perms: Object.fromEntries(Object.entries(s.perms).filter(([uid]) => uid !== userId)),
    }));
    apiDel(`/v1/users/${userId}`).catch(() => {
      toast.error("Could not delete the user — reverting.");
      void get().refreshAll();
    });
  },

  addUser: ({ name, password: given, role }) => {
    const generated = !given;
    const password = given || `sbx-${crypto.getRandomValues(new Uint32Array(2)).join("").slice(0, 12)}A1`;
    apiPost<ApiUser>("/v1/users", { username: name.trim(), password, role: roleToApi(role) })
      .then((u) => {
        toast.success(`User ${u.username ?? name} created`, {
          description: generated ? `Temporary password: ${password} — share it securely.` : `Role: ${role}`,
          duration: generated ? 30000 : 5000,
        });
        void get().refreshAll();
      })
      .catch((e) => toast.error(e instanceof Error ? e.message : "Could not create the user"));
  },

  setSandboxStatus: (id, status) => {
    const cur = get().sandboxes.find((b) => b.id === id)?.status;
    if (cur === undefined) {
      // unknown id (e.g. the optimistic row was already swapped for the real
      // sandbox) — don't fire lifecycle calls, just resync
      setTimeout(() => void get().refreshAll(), 800);
      return;
    }
    if (cur === status) {
      if (status === "Running" && cur === "Running") return;
      if (status !== "Running") return;
    }
    if (status === "Starting" || status === "Running") {
      // resume — the views optimistically flip to Running shortly after
      set((s) => ({
        sandboxes: s.sandboxes.map((b) =>
          b.id === id ? { ...b, status: status === "Running" && cur !== "Stopped" && cur !== "Suspended" ? status : "Starting" } : b,
        ),
      }));
      if (cur === "Stopped" || cur === "Suspended" || cur === undefined) {
        apiPost(`/v1/sandboxes/${id}/start`)
          .then(() => setTimeout(() => void get().refreshAll(), BACKOFF_MS))
          .catch((e) => toast.error(e instanceof Error ? e.message : `Could not start ${id}`));
      } else if (status === "Running") {
        // template views flip to Running while the API is still lazy-creating
        setTimeout(() => void get().refreshAll(), 1200);
      }
    } else {
      // Stopped / Suspended — release compute (snapshot semantics on the API)
      set((s) => ({ sandboxes: s.sandboxes.map((b) => (b.id === id ? { ...b, status, uptime: "—" } : b)) }));
      apiPost(`/v1/sandboxes/${id}/stop`)
        .then(() => setTimeout(() => void get().refreshAll(), 800))
        .catch((e) => toast.error(e instanceof Error ? e.message : `Could not stop ${id}`));
    }
  },

  restartSandbox: (id) => {
    set((s) => ({
      sandboxes: s.sandboxes.map((b) => (b.id === id ? { ...b, status: "Starting", uptime: "00:00:00" } : b)),
    }));
    apiPost(`/v1/sandboxes/${id}/restart`)
      .then(() => setTimeout(() => void get().refreshAll(), BACKOFF_MS))
      .catch((e) => {
        toast.error(e instanceof Error ? e.message : `Could not restart ${id}`);
        void get().refreshAll();
      });
  },

  openSandbox: (id) => set({ selectedSandboxId: id }),
  closeSandbox: () => set({ selectedSandboxId: null }),

  addRule: (sbxId, { kind, value, action }) => {
    const rule: FirewallRule = {
      id: `fwr-${Date.now()}`,
      kind,
      value: value.trim(),
      action,
      enabled: true,
      hits: 0,
      addedBy: get().me?.username ?? "admin",
      addedAt: new Date().toISOString().slice(0, 10),
    };
    const next = [rule, ...(get().firewall[sbxId] ?? [])];
    set((s) => ({ firewall: { ...s.firewall, [sbxId]: next } }));
    pushEgress(sbxId, next).catch(() => {
      toast.error("Could not persist the rule — reverting.");
      void get().refreshAll();
    });
  },

  removeRule: (sbxId, ruleId) => {
    const next = (get().firewall[sbxId] ?? []).filter((r) => r.id !== ruleId);
    set((s) => ({ firewall: { ...s.firewall, [sbxId]: next } }));
    pushEgress(sbxId, next).catch(() => {
      toast.error("Could not persist the removal — reverting.");
      void get().refreshAll();
    });
  },

  toggleRule: (sbxId, ruleId) => {
    const next = (get().firewall[sbxId] ?? []).map((r) => (r.id === ruleId ? { ...r, enabled: !r.enabled } : r));
    set((s) => ({ firewall: { ...s.firewall, [sbxId]: next } }));
    pushEgress(sbxId, next).catch(() => {
      toast.error("Could not persist the toggle — reverting.");
      void get().refreshAll();
    });
  },

  setEgressPolicy: (sbxId, policy) => {
    set((s) => ({ egressPolicy: { ...s.egressPolicy, [sbxId]: policy } }));
    const rules = get().firewall[sbxId] ?? [];
    pushEgress(sbxId, rules, policy === "allow" ? "open" : "allowlist").catch(() => {
      toast.error("Could not change the egress policy — reverting.");
      void get().refreshAll();
    });
  },

  tickFirewall: (sbxId) =>
    set((s) => {
      const rules = s.firewall[sbxId] ?? [];
      if (!rules.length) return {};
      const idx = Math.floor(Math.random() * rules.length);
      if (!rules[idx].enabled) return {};
      const bump = Math.floor(Math.random() * 4);
      if (!bump) return {};
      return {
        firewall: {
          ...s.firewall,
          [sbxId]: rules.map((r, i) => (i === idx ? { ...r, hits: r.hits + bump } : r)),
        },
      };
    }),

  togglePerm: (userId, sandboxId, kind) =>
    set((s) => {
      const userPerms = { ...(s.perms[userId] ?? {}) };
      const cur = userPerms[sandboxId] ?? emptyPerm;
      let next: Perm = { ...cur, [kind]: !cur[kind] };
      // cascade: W/X imply R — revoking R revokes W/X
      if ((kind === "w" || kind === "x") && next[kind]) next = { ...next, r: true };
      if (kind === "r" && !next.r) next = { r: false, w: false, x: false };
      userPerms[sandboxId] = next;
      return { perms: { ...s.perms, [userId]: userPerms } };
    }),

  resetPerms: () => set((s) => ({ perms: s.savedPerms })),

  savePerms: () => {
    const grants = matrixToGrants(get().perms);
    putPermissionsSafe(grants)
      .then(() => {
        set({ savedPerms: get().perms });
        toast.success("Permissions saved", { description: `${grants.length} grant(s) written to the API.` });
      })
      .catch((e) => toast.error(e instanceof Error ? e.message : "Saving permissions failed"));
  },
}));

async function putPermissionsSafe(grants: PermGrant[]) {
  // PUT /v1/permissions is admin-gated; the server enforces the W/X ⇒ R cascade
  return apiPut<{ ok: boolean; grants: number }>("/v1/permissions", { grants });
}

/** convenience for the create-sandbox view's success navigation */
export const VIEW_AFTER_CREATE: ViewKey = "sandboxes";

export function permsDirty(a: PermMatrix, b: PermMatrix): boolean {
  const ids = new Set([...Object.keys(a), ...Object.keys(b)]);
  for (const uid of ids) {
    const sbxs = new Set([...Object.keys(a[uid] ?? {}), ...Object.keys(b[uid] ?? {})]);
    for (const sid of sbxs) {
      const pa = a[uid]?.[sid];
      const pb = b[uid]?.[sid];
      if ((pa?.r ?? false) !== (pb?.r ?? false)) return true;
      if ((pa?.w ?? false) !== (pb?.w ?? false)) return true;
      if ((pa?.x ?? false) !== (pb?.x ?? false)) return true;
    }
  }
  return false;
}
