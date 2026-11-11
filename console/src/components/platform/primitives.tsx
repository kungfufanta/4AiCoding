

import { type ReactNode } from "react";
import { cn } from "@/lib/utils";
import {
  type Role,
  type SandboxStatus,
  type UserStatus,
  AVATAR_COLORS,
  initialsOf,
} from "@/lib/platform-data";

/* ── Panel: the standard card surface ─────────────────────────── */
export function Panel({
  className,
  children,
  ...rest
}: React.HTMLAttributes<HTMLDivElement>) {
  return (
    <div
      className={cn(
        "bg-surface border border-line rounded-xl panel-shadow",
        className
      )}
      {...rest}
    >
      {children}
    </div>
  );
}

/* ── Vietnamese sub-label ─────────────────────────────────────── */
export function VI({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <span className={cn("text-[11px] text-gray-600 font-normal", className)}>
      {children}
    </span>
  );
}

/* ── Page header: 28px bold title + bilingual subtitle + actions ─ */
export function PageHeader({
  title,
  subtitle,
  vi,
  breadcrumb,
  actions,
}: {
  title: string;
  subtitle?: string;
  vi?: string;
  breadcrumb?: ReactNode;
  actions?: ReactNode;
}) {
  return (
    <header className="flex flex-col gap-4 md:flex-row md:items-end md:justify-between">
      <div className="min-w-0">
        {breadcrumb}
        <h1 className="text-[28px] leading-tight font-bold tracking-tight text-ink">
          {title}
        </h1>
        {(subtitle || vi) && (
          <p className="mt-1.5 text-sm text-gray-400">
            {subtitle}
            {vi && (
              <span className="text-gray-500">
                <span className="mx-2 text-gray-600">·</span>
                {vi}
              </span>
            )}
          </p>
        )}
      </div>
      {actions && <div className="flex flex-wrap items-center gap-2.5">{actions}</div>}
    </header>
  );
}

/* ── Section title (card headers) ─────────────────────────────── */
export function SectionTitle({
  title,
  vi,
  right,
  className,
}: {
  title: string;
  vi?: string;
  right?: ReactNode;
  className?: string;
}) {
  return (
    <div className={cn("flex items-center justify-between gap-3", className)}>
      <div>
        <h2 className="text-sm font-semibold text-ink tracking-tight">{title}</h2>
        {vi && <VI className="block mt-0.5">{vi}</VI>}
      </div>
      {right}
    </div>
  );
}

/* ── KPI card ─────────────────────────────────────────────────── */
export function KpiCard({
  icon,
  label,
  vi,
  value,
  trend,
  accent = "text-data",
  className,
}: {
  icon: ReactNode;
  label: string;
  vi?: string;
  value: ReactNode;
  trend?: { delta: string; up: boolean };
  accent?: string;
  className?: string;
}) {
  return (
    <Panel className={cn("p-5 flex flex-col gap-3 min-w-0", className)}>
      <div className="flex items-start justify-between">
        <div className="size-9 rounded-lg bg-navy/60 border border-line grid place-items-center text-gray-400">
          {icon}
        </div>
        {trend && (
          <span
            className={cn(
              "font-mono text-[11px] px-1.5 py-0.5 rounded-md border",
              trend.up
                ? "text-neon border-neon/25 bg-neon/5"
                : "text-danger border-danger/25 bg-danger/5"
            )}
          >
            {trend.up ? "▲" : "▼"} {trend.delta}
          </span>
        )}
      </div>
      <div>
        <div className={cn("font-mono text-[26px] leading-none font-semibold tracking-tight", accent)}>
          {value}
        </div>
        <div className="mt-1.5 text-xs text-gray-400">
          {label}
          {vi && <VI className="ml-1.5">{vi}</VI>}
        </div>
      </div>
    </Panel>
  );
}

/* ── Sandbox / user status pill: dot + label ──────────────────── */
const STATUS_STYLE: Record<string, { dot: string; text: string; live?: boolean }> = {
  Running:   { dot: "bg-neon",   text: "text-neon",   live: true },
  Active:    { dot: "bg-neon",   text: "text-neon",   live: true },
  Starting:  { dot: "bg-warn",   text: "text-warn" },
  Suspended: { dot: "bg-danger", text: "text-danger" },
  Stopped:   { dot: "bg-gray-500", text: "text-gray-400" },
  Error:     { dot: "bg-danger", text: "text-danger" },
};

export function StatusPill({
  status,
  className,
}: {
  status: SandboxStatus | UserStatus;
  className?: string;
}) {
  const s = STATUS_STYLE[status] ?? STATUS_STYLE.Stopped;
  return (
    <span className={cn("inline-flex items-center gap-1.5 text-xs font-medium", s.text, className)}>
      <span className={cn("size-1.5 rounded-full", s.dot, s.live && "dot-live")} />
      {status}
    </span>
  );
}

/* ── Role pill badge ──────────────────────────────────────────── */
const ROLE_STYLE: Record<Role, string> = {
  Admin:     "text-admin border-admin/30 bg-admin/10",
  Developer: "text-info border-info/30 bg-info/10",
  Viewer:    "text-gray-400 border-gray-500/30 bg-gray-500/10",
};

export function RoleBadge({ role, className }: { role: Role; className?: string }) {
  return (
    <span
      className={cn(
        "inline-flex items-center px-2 py-0.5 rounded-full border font-mono text-[11px] leading-4",
        ROLE_STYLE[role],
        className
      )}
    >
      {role}
    </span>
  );
}

/* ── User avatar (initials circle) ────────────────────────────── */
export function UserAvatar({
  name,
  colorIdx,
  size = "md",
  className,
}: {
  name: string;
  colorIdx: number;
  size?: "sm" | "md" | "lg";
  className?: string;
}) {
  const c = AVATAR_COLORS[colorIdx % AVATAR_COLORS.length];
  const dim = size === "lg" ? "size-11 text-sm" : size === "sm" ? "size-7 text-[10px]" : "size-9 text-xs";
  return (
    <span
      className={cn(
        "inline-grid place-items-center rounded-full font-semibold ring-1 select-none shrink-0",
        dim,
        c.bg,
        c.text,
        c.ring,
        className
      )}
    >
      {initialsOf(name)}
    </span>
  );
}

/* ── Segmented control (All / Active / Suspended, 1h/6h/24h …) ── */
export function Segmented<T extends string>({
  options,
  value,
  onChange,
  size = "md",
  className,
}: {
  options: Array<{ value: T; label: string }>;
  value: T;
  onChange: (v: T) => void;
  size?: "sm" | "md";
  className?: string;
}) {
  return (
    <div
      role="tablist"
      className={cn(
        "inline-flex items-center gap-0.5 rounded-lg border border-line bg-navy/60 p-0.5",
        className
      )}
    >
      {options.map((o) => {
        const active = o.value === value;
        return (
          <button
            key={o.value}
            role="tab"
            aria-selected={active}
            onClick={() => onChange(o.value)}
            className={cn(
              "rounded-md font-medium transition-colors outline-none focus-visible:ring-2 focus-visible:ring-ring",
              size === "sm" ? "px-2.5 py-1 text-[11px]" : "px-3 py-1.5 text-xs",
              active
                ? "bg-neon/10 text-neon shadow-[inset_0_0_0_1px_var(--glow-mid)]"
                : "text-gray-400 hover:text-gray-200 hover:bg-fg/5"
            )}
          >
            {o.label}
          </button>
        );
      })}
    </div>
  );
}

/* ── R/W/X permission chip ────────────────────────────────────── */
export function PermChip({
  label,
  on,
  onClick,
  disabled,
}: {
  label: "R" | "W" | "X";
  on: boolean;
  onClick: () => void;
  disabled?: boolean;
}) {
  return (
    <button
      type="button"
      aria-pressed={on}
      disabled={disabled}
      onClick={onClick}
      className={cn(
        "w-8 h-6 rounded-full font-mono text-[11px] leading-none transition-all outline-none focus-visible:ring-2 focus-visible:ring-ring",
        on
          ? "bg-neon text-navy font-bold shadow-[0_0_10px_var(--glow-mid)] hover:bg-neon/90"
          : "border border-gray-600 text-gray-500 hover:border-gray-400 hover:text-gray-300",
        disabled && "opacity-40 pointer-events-none"
      )}
    >
      {label}
    </button>
  );
}

/* ── Mono text helper ─────────────────────────────────────────── */
export function Mono({
  children,
  className,
}: {
  children: ReactNode;
  className?: string;
}) {
  return <span className={cn("font-mono", className)}>{children}</span>;
}
