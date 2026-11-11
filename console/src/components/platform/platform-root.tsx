

import { useEffect, useState } from "react";
import { Box } from "lucide-react";
import { get as apiGet, post as apiPost } from "@/console/api";
import { usePlatform } from "@/lib/platform-store";
import { AppShell } from "./app-shell";

/* ── platform login — template design tokens, bilingual labels ─────── */

function PlatformLogin({ setupNeeded, onLogin }: { setupNeeded: boolean; onLogin: () => void }) {
  const [username, setUsername] = useState("admin");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError("");
    if (setupNeeded && password !== confirm) {
      setError("Passwords do not match · Mật khẩu không khớp.");
      return;
    }
    setBusy(true);
    try {
      await apiPost(setupNeeded ? "/v1/auth/setup" : "/v1/auth/login", { username, password });
      onLogin();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  const field =
    "h-10 w-full rounded-lg border border-line bg-navy/60 px-3 text-sm text-gray-100 placeholder:text-gray-600 outline-none focus-visible:ring-2 focus-visible:ring-ring";

  return (
    <div className="grid-bg flex min-h-screen items-start justify-center pt-[16vh]">
      <form onSubmit={submit} className="w-[380px] rounded-xl border border-line bg-surface p-7 panel-shadow">
        <div className="mb-5 flex items-center gap-3">
          <div className="grid size-10 place-items-center rounded-lg border border-neon/30 bg-neon/10">
            <Box className="size-5 text-neon" />
          </div>
          <div>
            <div className="text-lg font-bold leading-tight tracking-tight text-ink">4AiCoding</div>
            <div className="font-mono text-[9.5px] uppercase tracking-wider text-gray-500">Sandbox Console</div>
          </div>
        </div>
        <p className="mb-5 text-sm text-gray-400">
          {setupNeeded
            ? "No users yet. Create the first admin account · Chưa có người dùng — hãy tạo admin đầu tiên."
            : "Sign in to the sandbox console · Đăng nhập vào bảng điều khiển."}
        </p>
        <label className="mb-3 flex flex-col gap-1.5 text-xs font-medium text-gray-400">
          Username · Tên đăng nhập
          <input className={field} value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="username" required />
        </label>
        <label className="mb-3 flex flex-col gap-1.5 text-xs font-medium text-gray-400">
          Password · Mật khẩu
          <input
            className={field}
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete={setupNeeded ? "new-password" : "current-password"}
            minLength={setupNeeded ? 8 : undefined}
            required
            autoFocus
          />
        </label>
        {setupNeeded && (
          <label className="mb-3 flex flex-col gap-1.5 text-xs font-medium text-gray-400">
            Confirm password · Xác nhận mật khẩu
            <input className={field} type="password" value={confirm} onChange={(e) => setConfirm(e.target.value)} autoComplete="new-password" required />
          </label>
        )}
        {error && <p className="mb-3 text-sm text-danger">{error}</p>}
        <button
          type="submit"
          disabled={busy}
          className="mt-1 w-full rounded-lg bg-neon px-4 py-2.5 text-sm font-semibold text-navy transition-all hover:bg-neon/90 hover:shadow-[0_0_20px_var(--glow-mid)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-50"
        >
          {setupNeeded ? "Create admin · Tạo admin" : "Sign in · Đăng nhập"}
        </button>
      </form>
    </div>
  );
}

/**
 * Auth + data boot for the SBX platform UI: checks the sandboxd session,
 * loads the store from /v1, then either shows the platform login or the
 * full AppShell.
 */
export default function PlatformRoot() {
  const [mounted, setMounted] = useState(false);
  const [authed, setAuthed] = useState<boolean | null>(null);
  const [setupNeeded, setSetupNeeded] = useState(false);
  const [authTick, setAuthTick] = useState(0);

  useEffect(() => {
    setMounted(true);
  }, []);

  useEffect(() => {
    if (!mounted) return;
    let live = true;
    (async () => {
      try {
        const st = await apiGet<{ enabled: boolean; authenticated: boolean; setup_needed?: boolean }>("/v1/auth/status");
        if (!live) return;
        setSetupNeeded(!!st.setup_needed);
        if (!st.authenticated) {
          setAuthed(false);
          return;
        }
        // sandboxd answers {kind, user, role}; the store wants {username, role}
        const m = await apiGet<{ user?: string; username?: string; role: "admin" | "operator" | "viewer" }>("/v1/auth/me");
        if (!live) return;
        usePlatform.setState({ me: { username: m.username ?? m.user ?? "", role: m.role } });
        await usePlatform.getState().refreshAll();
        if (live) setAuthed(true);
      } catch {
        if (live) setAuthed(false);
      }
    })();
    return () => {
      live = false;
    };
  }, [mounted, authTick]);

  if (!mounted || authed === null) {
    return (
      <div className="min-h-screen bg-navy" aria-busy="true">
        <div className="grid min-h-screen place-items-center">
          <div className="font-mono text-xs text-gray-600">4AiCoding · loading…</div>
        </div>
      </div>
    );
  }

  if (!authed) {
    return <PlatformLogin setupNeeded={setupNeeded} onLogin={() => setAuthTick((t) => t + 1)} />;
  }

  return <AppShell />;
}
