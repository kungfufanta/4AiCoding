import type { InputHTMLAttributes, ReactNode } from "react";
import { cn } from "../lib/cn";

export function Input({ className, ...rest }: InputHTMLAttributes<HTMLInputElement>) {
  return (
    <input
      className={cn(
        "h-9 w-full rounded-[var(--radius)] border border-border bg-bg px-3 text-sm text-fg",
        "placeholder:text-muted/70 disabled:opacity-60",
        className,
      )}
      {...rest}
    />
  );
}

// Label + control + hint, the standard form row.
export function Field({
  label,
  hint,
  children,
}: {
  label: string;
  hint?: ReactNode;
  children: ReactNode;
}) {
  return (
    <label className="block space-y-1.5">
      <span className="text-sm font-medium text-fg">{label}</span>
      {children}
      {hint !== undefined && <span className="block text-xs text-muted">{hint}</span>}
    </label>
  );
}
