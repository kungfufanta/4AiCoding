import type { ButtonHTMLAttributes, ReactNode } from "react";
import { Loader2 } from "lucide-react";
import { cn } from "../lib/cn";

type Variant = "primary" | "secondary" | "ghost" | "danger";
type Size = "md" | "sm";

const variantClasses: Record<Variant, string> = {
  primary: "bg-brand text-brand-fg hover:bg-brand-hover shadow-sm",
  secondary: "border border-border bg-surface text-fg hover:bg-surface-2",
  ghost: "text-muted hover:text-fg hover:bg-surface-2",
  danger: "bg-danger text-white hover:opacity-90",
};

const sizeClasses: Record<Size, string> = {
  md: "h-9 px-3.5 text-sm",
  sm: "h-7.5 px-2.5 text-xs",
};

const buttonBase =
  "inline-flex items-center justify-center gap-1.5 rounded-[var(--radius)] font-medium " +
  "transition-colors cursor-pointer disabled:opacity-60 disabled:pointer-events-none select-none";

export function Button({
  variant = "primary",
  size = "md",
  loading = false,
  className,
  children,
  disabled,
  type = "button",
  ...rest
}: ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: Variant;
  size?: Size;
  loading?: boolean;
}) {
  return (
    <button
      type={type}
      className={cn(buttonBase, variantClasses[variant], sizeClasses[size], className)}
      disabled={disabled || loading}
      {...rest}
    >
      {loading && <Loader2 aria-hidden className="size-4 animate-spin" />}
      {children}
    </button>
  );
}

// Anchor styled as the primary button (SSO sign-in link).
export function ButtonLink({
  href,
  className,
  children,
}: {
  href: string;
  className?: string;
  children: ReactNode;
}) {
  return (
    <a href={href} className={cn(buttonBase, variantClasses.primary, sizeClasses.md, className)}>
      {children}
    </a>
  );
}
