// Copy-to-clipboard row for values shown once (invite URL now, API
// keys in P2). Never stores the value beyond this component's props.

import { useEffect, useRef, useState } from "react";
import { Check, Copy } from "lucide-react";
import { cn } from "../lib/cn";

export function CopyField({ value, className }: { value: string; className?: string }) {
  const [copied, setCopied] = useState(false);
  const timer = useRef<number | undefined>(undefined);

  useEffect(() => () => window.clearTimeout(timer.current), []);

  async function copy() {
    try {
      await navigator.clipboard.writeText(value);
      setCopied(true);
      window.clearTimeout(timer.current);
      timer.current = window.setTimeout(() => setCopied(false), 1500);
    } catch {
      // Clipboard permission denied — the value stays selectable.
    }
  }

  return (
    <div
      className={cn(
        "flex items-stretch overflow-hidden rounded-[var(--radius)] border border-border bg-bg",
        className,
      )}
    >
      <code className="min-w-0 flex-1 truncate px-3 py-2 font-mono text-xs text-fg select-all">
        {value}
      </code>
      <button
        type="button"
        onClick={copy}
        aria-label="Copy to clipboard"
        className="flex shrink-0 items-center gap-1 border-l border-border bg-surface-2 px-2.5 text-xs font-medium text-muted transition-colors hover:text-fg"
      >
        {copied ? (
          <Check aria-hidden className="size-3.5 text-success" />
        ) : (
          <Copy aria-hidden className="size-3.5" />
        )}
        {copied ? "Copied" : "Copy"}
      </button>
    </div>
  );
}
