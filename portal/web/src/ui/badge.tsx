import type { ReactNode } from "react";
import { cn } from "../lib/cn";

type Variant = "neutral" | "success" | "warning" | "danger" | "brand";

const variantClasses: Record<Variant, string> = {
  neutral: "border-border bg-surface-2 text-muted",
  success: "border-transparent bg-success/15 text-success",
  warning: "border-transparent bg-warning/15 text-warning",
  danger: "border-transparent bg-danger/15 text-danger",
  brand: "border-transparent bg-brand-soft text-brand",
};

export function Badge({
  variant = "neutral",
  className,
  children,
}: {
  variant?: Variant;
  className?: string;
  children: ReactNode;
}) {
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-xs font-medium",
        variantClasses[variant],
        className,
      )}
    >
      {children}
    </span>
  );
}
