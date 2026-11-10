import { useEffect, useState } from "react";

type Health = { status: string; error?: string };

// S0 skeleton: one page that proves the wiring end to end — the SPA
// builds, loads and can reach the backend's health probes. S1 swaps
// this for the real dashboard (login → orgs → projects).
export function App() {
  const [health, setHealth] = useState<Health | null>(null);
  const [ready, setReady] = useState<Health | null>(null);

  useEffect(() => {
    fetch("/healthz").then((r) => r.json()).then(setHealth).catch(() => setHealth({ status: "unreachable" }));
    fetch("/readyz").then((r) => r.json()).then(setReady).catch(() => setReady({ status: "unreachable" }));
  }, []);

  return (
    <main className="shell">
      <header className="topbar">
        <span className="brand">Portal</span>
        <span className="sub">sandboxes for agents</span>
      </header>
      <section className="cards">
        <div className="card">
          <h2>/healthz</h2>
          <p className={health?.status === "ok" ? "ok" : "bad"}>{health?.status ?? "…"}</p>
        </div>
        <div className="card">
          <h2>/readyz</h2>
          <p className={ready?.status === "ready" ? "ok" : "bad"}>{ready?.status ?? "…"}</p>
        </div>
      </section>
      <footer className="foot">S0 skeleton — see docs/PORTAL_PLAN.md for the milestone plan.</footer>
    </main>
  );
}
