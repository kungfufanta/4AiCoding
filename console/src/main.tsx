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

  return (
    <MeContext.Provider value={me}>
      <div className="shell">
        <nav className="top">
          <Link to="/" className="brand">4AiCoding</Link>
          <Link to="/" className={path.startsWith("/admin") ? "" : "active"}>Sandboxes</Link>
          {can(me, "operator") && <Link to="/admin" className={path.startsWith("/admin") ? "active" : ""}>Admin</Link>}
          <span className="spacer" />
          <span className="muted">
            {me.user} · {me.role}
          </span>
          {me.kind === "user" && (
            <button className="ghost" onClick={logout}>
              Sign out
            </button>
          )}
        </nav>
        <main>{page}</main>
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
