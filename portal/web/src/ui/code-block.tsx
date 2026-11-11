// Multi-line code block with a copy button (quickstart snippets).

import { useEffect, useRef, useState } from "react";
import { Check, Copy } from "lucide-react";

export function CodeBlock({ code, ariaLabel }: { code: string; ariaLabel?: string }) {
  const [copied, setCopied] = useState(false);
  const timer = useRef<number | undefined>(undefined);

  useEffect(() => () => window.clearTimeout(timer.current), []);

  async function copy() {
    try {
      await navigator.clipboard.writeText(code);
      setCopied(true);
      window.clearTimeout(timer.current);
      timer.current = window.setTimeout(() => setCopied(false), 1500);
    } catch {
      // Clipboard denied — the text stays selectable.
    }
  }

  return (
    <div className="relative rounded-[var(--radius)] border border-border bg-bg">
      <pre
        aria-label={ariaLabel}
        className="overflow-x-auto p-3 pr-12 font-mono text-xs leading-relaxed text-fg whitespace-pre"
      >
        {code}
      </pre>
      <button
        type="button"
        onClick={copy}
        aria-label="Copy snippet"
        className="absolute right-2 top-2 rounded-md border border-border bg-surface p-1.5 text-muted transition-colors hover:text-fg"
      >
        {copied ? (
          <Check aria-hidden className="size-3.5 text-success" />
        ) : (
          <Copy aria-hidden className="size-3.5" />
        )}
      </button>
    </div>
  );
}
