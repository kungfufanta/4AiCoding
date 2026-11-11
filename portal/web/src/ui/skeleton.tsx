import type { ReactNode } from "react";
import { Loader2 } from "lucide-react";
import { cn } from "../lib/cn";

export function Skeleton({ className }: { className?: string }) {
  return <div className={cn("animate-pulse rounded-[var(--radius)] bg-surface-2", className)} />;
}

// Table loading placeholder: five grey rows.
export function SkeletonRows({ cols }: { cols: number }) {
  return (
    <div className="space-y-2" aria-hidden>
      {Array.from({ length: 5 }, (_, i) => (
        <div key={i} className="flex gap-3">
          {Array.from({ length: cols }, (_, j) => (
            <Skeleton key={j} className={j === 0 ? "h-9 flex-1" : "h-9 w-28"} />
          ))}
        </div>
      ))}
    </div>
  );
}

export function EmptyState({
  icon,
  title,
  description,
  action,
}: {
  icon?: ReactNode;
  title: string;
  description?: string;
  action?: ReactNode;
}) {
  return (
    <div className="flex flex-col items-center justify-center gap-2 rounded-[var(--radius)] border border-dashed border-border bg-surface/50 px-6 py-12 text-center">
      {icon && <div className="text-muted">{icon}</div>}
      <p className="text-sm font-medium text-fg">{title}</p>
      {description && <p className="max-w-sm text-sm text-muted">{description}</p>}
      {action && <div className="mt-2">{action}</div>}
    </div>
  );
}

export function FullPageSpinner({ label = "Loading…" }: { label?: string }) {
  return (
    <div className="flex min-h-dvh flex-col items-center justify-center gap-3 text-muted">
      <Loader2 aria-hidden className="size-6 animate-spin" />
      <p className="text-sm">{label}</p>
    </div>
  );
}
