// Toast notifications (PORTAL_PLAN_V2 §4): errors and successes surface
// here instead of alert(). aria-live so screen readers announce them.

import {
  createContext,
  useCallback,
  useContext,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { AlertCircle, CheckCircle2, Info, X } from "lucide-react";
import { cn } from "../lib/cn";

type ToastKind = "success" | "error" | "info";
type ToastItem = { id: number; kind: ToastKind; message: string };

const ToastContext = createContext<(kind: ToastKind, message: string) => void>(() => {});

export function useToast() {
  return useContext(ToastContext);
}

const kindStyles: Record<ToastKind, string> = {
  success: "text-success",
  error: "text-danger",
  info: "text-brand",
};

const kindIcons: Record<ToastKind, ReactNode> = {
  success: <CheckCircle2 aria-hidden className="size-4.5" />,
  error: <AlertCircle aria-hidden className="size-4.5" />,
  info: <Info aria-hidden className="size-4.5" />,
};

const TOAST_TTL_MS = 4500;

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([]);
  const nextID = useRef(1);

  const toast = useCallback((kind: ToastKind, message: string) => {
    const id = nextID.current++;
    setItems((cur) => [...cur, { id, kind, message }]);
    window.setTimeout(() => {
      setItems((cur) => cur.filter((t) => t.id !== id));
    }, TOAST_TTL_MS);
  }, []);

  const dismiss = (id: number) => setItems((cur) => cur.filter((t) => t.id !== id));

  const value = useMemo(() => toast, [toast]);

  return (
    <ToastContext.Provider value={value}>
      {children}
      <div
        aria-live="polite"
        className="pointer-events-none fixed bottom-4 right-4 z-[70] flex w-80 max-w-[calc(100vw-2rem)] flex-col gap-2"
      >
        {items.map((t) => (
          <div
            key={t.id}
            role="status"
            className={cn(
              "toast-item pointer-events-auto flex items-start gap-2.5 rounded-[var(--radius)] border border-border bg-surface p-3 text-sm shadow-lg",
            )}
          >
            <span className={cn("mt-0.5 shrink-0", kindStyles[t.kind])}>{kindIcons[t.kind]}</span>
            <span className="flex-1 text-fg">{t.message}</span>
            <button
              aria-label="Dismiss notification"
              onClick={() => dismiss(t.id)}
              className="shrink-0 rounded p-0.5 text-muted transition-colors hover:bg-surface-2 hover:text-fg"
            >
              <X aria-hidden className="size-3.5" />
            </button>
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  );
}
