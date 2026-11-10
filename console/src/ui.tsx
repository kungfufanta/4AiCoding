import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from "react";
import { ApiError, isApprovalPending, type Me, type Role } from "./api";

// --- routing (history API; the app has six screens, no router dependency) --

export function navigate(to: string) {
  if (to === location.pathname + location.search) return;
  history.pushState(null, "", to);
  window.dispatchEvent(new PopStateEvent("popstate"));
}

export function usePath(): string {
  const [path, setPath] = useState(location.pathname);
  useEffect(() => {
    const on = () => setPath(location.pathname);
    window.addEventListener("popstate", on);
    return () => window.removeEventListener("popstate", on);
  }, []);
  return path;
}

export function Link({ to, children, className }: { to: string; children: ReactNode; className?: string }) {
  return (
    <a
      href={to}
      className={className}
      onClick={(e) => {
        if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return;
        e.preventDefault();
        navigate(to);
      }}
    >
      {children}
    </a>
  );
}

// --- session ------------------------------------------------------------

export const MeContext = createContext<Me | null>(null);
export const useMe = () => useContext(MeContext);

const rank: Record<Role, number> = { viewer: 1, operator: 2, admin: 3 };
export function can(me: Me | null, need: Role): boolean {
  return !!me && (rank[me.role] ?? 0) >= rank[need];
}

// --- notices --------------------------------------------------------------

type Notice = { kind: "ok" | "err" | "info"; text: string };
const NoticeContext = createContext<(n: Notice) => void>(() => {});
export const useNotify = () => useContext(NoticeContext);

export function NoticeProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<(Notice & { id: number })[]>([]);
  const push = useCallback((n: Notice) => {
    const id = Date.now() + Math.random();
    setItems((xs) => [...xs, { ...n, id }]);
    setTimeout(() => setItems((xs) => xs.filter((x) => x.id !== id)), n.kind === "err" ? 8000 : 4000);
  }, []);
  return (
    <NoticeContext.Provider value={push}>
      {children}
      <div className="toasts" role="status">
        {items.map((n) => (
          <div key={n.id} className={`toast ${n.kind}`}>
            {n.text}
          </div>
        ))}
      </div>
    </NoticeContext.Provider>
  );
}

/**
 * Runs an action and reports its outcome. A 202 approval response is shown as
 * "waiting for approval" rather than success.
 */
export function useAction() {
  const notify = useNotify();
  const [busy, setBusy] = useState(false);
  const run = useCallback(
    async <T,>(fn: () => Promise<T>, ok?: string): Promise<T | undefined> => {
      setBusy(true);
      try {
        const r = await fn();
        if (isApprovalPending(r)) {
          notify({ kind: "info", text: `Needs an admin's approval (${r.approval_id}). Repeat it once approved.` });
          return r;
        }
        if (ok) notify({ kind: "ok", text: ok });
        return r;
      } catch (e) {
        notify({ kind: "err", text: e instanceof ApiError ? `${e.message} (${e.code})` : String(e) });
        return undefined;
      } finally {
        setBusy(false);
      }
    },
    [notify],
  );
  return { run, busy };
}

/** Loads data from fn, re-running when deps change; reload() refreshes. */
export function useLoad<T>(fn: () => Promise<T>, deps: unknown[] = []) {
  const [data, setData] = useState<T | undefined>();
  const [error, setError] = useState<string | undefined>();
  const [tick, setTick] = useState(0);
  useEffect(() => {
    let live = true;
    fn().then(
      (d) => live && (setData(d), setError(undefined)),
      (e) => live && setError(e instanceof Error ? e.message : String(e)),
    );
    return () => {
      live = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, tick]);
  return { data, error, reload: () => setTick((t) => t + 1) };
}

// --- small components ----------------------------------------------------

export function StatusBadge({ status }: { status: string }) {
  return <span className={`badge st-${status}`}>{status}</span>;
}

export function Section({ title, actions, children }: { title: string; actions?: ReactNode; children: ReactNode }) {
  return (
    <section className="card">
      <header className="card-head">
        <h2>{title}</h2>
        {actions && <div className="row">{actions}</div>}
      </header>
      {children}
    </section>
  );
}

export function Loading({ error }: { error?: string }) {
  return error ? <p className="error">{error}</p> : <p className="muted">Loading…</p>;
}

/** The equivalent curl command, per PLAN §6 ("every button has a curl"). */
export function Curl({ method, path, body }: { method: string; path: string; body?: unknown }) {
  const b = body === undefined ? "" : ` -H 'content-type: application/json' -d '${JSON.stringify(body)}'`;
  return (
    <details className="curl">
      <summary>curl</summary>
      <code>{`curl -s -X ${method} -H "Authorization: Bearer $TOKEN" $API${path}${b}`}</code>
    </details>
  );
}

/** Edits a list of strings as one-per-line text. */
export function LinesInput({
  value,
  onChange,
  placeholder,
  rows = 4,
}: {
  value: string[];
  onChange: (v: string[]) => void;
  placeholder?: string;
  rows?: number;
}) {
  const [text, setText] = useState(value.join("\n"));
  useEffect(() => setText(value.join("\n")), [value.join("\n")]); // eslint-disable-line react-hooks/exhaustive-deps
  return (
    <textarea
      rows={rows}
      value={text}
      placeholder={placeholder}
      onChange={(e) => {
        setText(e.target.value);
        onChange(
          e.target.value
            .split("\n")
            .map((s) => s.trim())
            .filter(Boolean),
        );
      }}
    />
  );
}

export function fmtTime(v?: string | number): string {
  if (v === undefined || v === "" || v === 0) return "—";
  const d = typeof v === "number" ? new Date(v * 1000) : new Date(v);
  return isNaN(d.getTime()) ? String(v) : d.toLocaleString();
}

export function fmtBytes(n?: number): string {
  if (!n) return "0 B";
  const u = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) {
    n /= 1024;
    i++;
  }
  return `${n.toFixed(i ? 1 : 0)} ${u[i]}`;
}
