import { StrictMode, useEffect, useState } from "react";
import { createRoot } from "react-dom/client";
import { get, post, type AuthStatus, type Me } from "./api";
import { can, Link, MeContext, navigate, NoticeProvider, usePath } from "./ui";
import { LoginPage } from "./pages/Login";
import { SandboxListPage } from "./pages/SandboxList";
import { SandboxDetailPage } from "./pages/SandboxDetail";
import { SandboxConfigPage } from "./pages/SandboxConfig";
import { NetworkPage } from "./pages/Network";
import { AdminPage } from "./pages/Admin";
import "./styles.css";

/* ── tiny inline icons (no icon dependency) ─────────────────────────── */
const Ico = {
  box: (
    <svg viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M21 8l-9-5-9 5v8l9 5 9-5V8z" />
      <path d="M3.3 7.6L12 12.5l8.7-4.9M12 22V12.5" />
    </svg>
  ),
  shield: (
    <svg viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M12 22s8-3.5 8-10V5l-8-3-8 3v7c0 6.5 8 10 8 10z" />
      <path d="M9 12l2 2 4-4" />
    </svg>
  ),
  sun: (
    <svg viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" aria-hidden="true">
      <circle cx="12" cy="12" r="4" />
      <path d="M12 2v2m0 16v2M4.9 4.9l1.4 1.4m11.4 11.4l1.4 1.4M2 12h2m16 0h2M4.9 19.1l1.4-1.4m11.4-11.4l1.4-1.4" />
    </svg>
  ),
  moon: (
    <svg viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z" />
    </svg>
  ),
};

function ThemeToggle() {
  const [light, setLight] = useState(() => document.documentElement.classList.contains("light"));
  const toggle = () => {
    const next = !light;
    setLight(next);
    document.documentElement.classList.toggle("light", next);
    try {
      localStorage.setItem("console-theme", next ? "light" : "dark");
    } catch {
      /* private mode */
    }
  };
  return (
    <button className="theme-btn" onClick={toggle} aria-label={light ? "Switch to dark mode" : "Switch to light mode"} title={light ? "Dark mode" : "Light mode"}>
      {light ? Ico.moon : Ico.sun}
    </button>
  );
}

function initials(user: string): string {
  return user
    .split(/[\s._-]+/)
    .map((w) => w[0])
    .slice(0, 2)
    .join("")
    .toUpperCase();
}

function App() {
  const path = usePath();
  const [status, setStatus] = useState<AuthStatus | null>(null);
  const [me, setMe] = useState<Me | null>(null);
  const [checked, setChecked] = useState(false);

  const refresh = async () => {
    try {
      const st = await get<AuthStatus>("/v1/auth/status");
      setStatus(st);
      setMe(st.authenticated ? await get<Me>("/v1/auth/me") : null);
    } catch {
      setMe(null);
    } finally {
      setChecked(true);
    }
  };
  useEffect(() => {
    void refresh();
  }, []);

  if (!checked) return <p className="muted center">Loading…</p>;
  if (!me) return <LoginPage setupNeeded={!!status?.setup_needed} onLogin={refresh} />;

  let page;
  let m: RegExpMatchArray | null;
  if (path === "/" || path === "/sandboxes") page = <SandboxListPage />;
  else if (path === "/sandboxes/new") page = <SandboxConfigPage />;
  else if ((m = path.match(/^\/sandboxes\/([^/]+)\/network$/))) page = <NetworkPage id={m[1]} />;
  else if ((m = path.match(/^\/sandboxes\/([^/]+)$/))) page = <SandboxDetailPage id={m[1]} />;
  else if (path.startsWith("/admin")) page = <AdminPage tab={path.split("/")[2] || "policy"} />;
  else page = <p>Not found. <Link to="/">Back to sandboxes</Link></p>;

  const logout = async () => {
    try {
      await post("/v1/auth/logout");
    } finally {
      setMe(null);
      navigate("/");
    }
  };

  const adminActive = path.startsWith("/admin");

  return (
    <MeContext.Provider value={me}>
      <div className="shell">
        <aside className="side">
          <Link to="/" className="brand">
            <span className="brand-mark" aria-hidden="true">▣</span>
            <span className="brand-col">
              4AiCoding
              <small>Sandbox Console</small>
            </span>
          </Link>
          <nav className="side-nav" aria-label="Main">
            <Link to="/" className={!adminActive ? "active" : ""}>
              <span className="nav-ico" aria-hidden="true">{Ico.box}</span> Sandboxes
            </Link>
            {can(me, "operator") && (
              <Link to="/admin" className={adminActive ? "active" : ""}>
                <span className="nav-ico" aria-hidden="true">{Ico.shield}</span> Admin
              </Link>
            )}
          </nav>
          <div className="side-foot">
            <div className="who">
              <span className="avatar" aria-hidden="true">{initials(me.user)}</span>
              <span className="who-txt" style={{ minWidth: 0 }}>
                <b>{me.user}</b>
                <span className="role">{me.role}</span>
              </span>
            </div>
            <div className="foot-row">
              <span className="ver">v0.4.0</span>
              <span className="row" style={{ gap: 6 }}>
                <ThemeToggle />
                {me.kind === "user" && (
                  <button className="ghost" onClick={logout} style={{ padding: "6px 10px", fontSize: 12 }}>
                    Sign out
                  </button>
                )}
              </span>
            </div>
          </div>
        </aside>
        <div className="main-col">
          <main>{page}</main>
        </div>
      </div>
    </MeContext.Provider>
  );
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <NoticeProvider>
      <App />
    </NoticeProvider>
  </StrictMode>,
);
