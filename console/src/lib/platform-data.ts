/* ── Console view-model types + small helpers ──────────────────────
   Everything shown in the console comes from sandboxd /v1; this file only
   holds the shapes the views use and pure helpers. */

export type Role = "Admin" | "Developer" | "Viewer";
export type UserStatus = "Active" | "Suspended";
export type SandboxStatus = "Running" | "Starting" | "Suspended" | "Stopped" | "Error";
export type ViewKey =
  | "overview"
  | "users"
  | "sandboxes"
  | "create"
  | "monitoring"
  | "network"
  | "settings";

export interface User {
  id: string;
  name: string;
  role: Role;
  status: UserStatus;
  joined: string;
  colorIdx: number;
}

export interface Sandbox {
  id: string;
  name: string;
  image: string;
  status: SandboxStatus;
  vcpu: number;
  memGb: number;
  env: string;
  host: string;
  ports: string[];
  uptime: string;
  cpuPct: number;
  memPct: number;
}

export interface Perm {
  r: boolean;
  w: boolean;
  x: boolean;
}
export type PermMatrix = Record<string, Record<string, Perm>>;

/* ── Permission summary line, e.g. "R·W" ──────────────────────── */
export function permLabel(perm: Perm | undefined): string {
  if (!perm) return "—";
  const parts: string[] = [];
  if (perm.r) parts.push("R");
  if (perm.w) parts.push("W");
  if (perm.x) parts.push("X");
  return parts.length ? parts.join("·") : "—";
}

/* ── Activity feed (Overview) ──────────────────────────────────── */
export interface ActivityItem {
  time: string;
  text: string;
  kind: "deploy" | "info" | "warn" | "error" | "access";
}

/* ── Notifications (top bar) ───────────────────────────────────── */
export interface Notification {
  id: string;
  tone: "info" | "warn" | "error" | "success";
  title: string;
  body: string;
  time: string;
}

/* ── Avatar palette ────────────────────────────────────────────── */
export const AVATAR_COLORS: Array<{ bg: string; text: string; ring: string }> = [
  { bg: "bg-purple-500/15",  text: "text-purple-700 dark:text-purple-300",   ring: "ring-purple-500/30" },
  { bg: "bg-cyan-500/15",    text: "text-cyan-700 dark:text-cyan-300",       ring: "ring-cyan-500/30" },
  { bg: "bg-amber-500/15",   text: "text-amber-700 dark:text-amber-300",     ring: "ring-amber-500/30" },
  { bg: "bg-emerald-500/15", text: "text-emerald-700 dark:text-emerald-300", ring: "ring-emerald-500/30" },
  { bg: "bg-rose-500/15",    text: "text-rose-700 dark:text-rose-300",       ring: "ring-rose-500/30" },
  { bg: "bg-sky-500/15",     text: "text-sky-700 dark:text-sky-300",         ring: "ring-sky-500/30" },
  { bg: "bg-teal-500/15",    text: "text-teal-700 dark:text-teal-300",       ring: "ring-teal-500/30" },
  { bg: "bg-fuchsia-500/15", text: "text-fuchsia-700 dark:text-fuchsia-300", ring: "ring-fuchsia-500/30" },
  { bg: "bg-lime-500/15",    text: "text-lime-700 dark:text-lime-300",       ring: "ring-lime-500/30" },
  { bg: "bg-orange-500/15",  text: "text-orange-700 dark:text-orange-300",   ring: "ring-orange-500/30" },
  { bg: "bg-slate-500/15",   text: "text-slate-700 dark:text-slate-300",     ring: "ring-slate-500/30" },
];

export function initialsOf(name: string): string {
  return name
    .split(" ")
    .map((w) => w[0])
    .slice(0, 2)
    .join("")
    .toUpperCase();
}

/* ── Egress rule rows (derived from the real allow / deny lists) ── */
export type RuleKind = "domain" | "ip" | "cidr";
export type RuleAction = "allow" | "block";

export interface FirewallRule {
  id: string;
  kind: RuleKind;
  value: string;
  action: RuleAction;
  enabled: boolean;
  hits: number;
  addedBy: string;
  addedAt: string;
}

function isIpv4(v: string): boolean {
  const parts = v.split(".");
  if (parts.length !== 4) return false;
  return parts.every((p) => /^([01]?\d\d?|2[0-4]\d|25[0-5])$/.test(p));
}

/** classify an allow/block target: CIDR carries a prefix, plain IPv4, or a (wildcard) domain */
export function classifyRuleValue(value: string): RuleKind | null {
  const v = value.trim();
  if (!v) return null;
  if (v.includes("/")) {
    const [ip, prefix] = v.split("/");
    if (isIpv4(ip) && /^([0-9]|[12]\d|3[0-2])$/.test(prefix)) return "cidr";
    return null;
  }
  if (isIpv4(v)) return "ip";
  if (/^(\*\.)?([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z]{2,}$/i.test(v)) return "domain";
  return null;
}
