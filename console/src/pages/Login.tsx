import { useState } from "react";
import { ApiError, post } from "../api";

export function LoginPage({ setupNeeded, onLogin }: { setupNeeded: boolean; onLogin: () => void }) {
  const [username, setUsername] = useState("admin");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError("");
    if (setupNeeded && password !== confirm) {
      setError("Passwords do not match.");
      return;
    }
    setBusy(true);
    try {
      await post(setupNeeded ? "/v1/auth/setup" : "/v1/auth/login", { username, password });
      onLogin();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="login">
      <form className="card" onSubmit={submit}>
        <div className="login-brand">
          <span className="brand-mark" aria-hidden="true">▣</span>
          <div>
            <h1>4AiCoding</h1>
            <p className="muted" style={{ margin: 0 }}>
              Sandbox Console
            </p>
          </div>
        </div>
        <p className="muted">
          {setupNeeded ? "No users yet. Create the first admin account." : "Sign in to the sandbox console."}
        </p>
        <label>
          Username
          <input value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="username" required />
        </label>
        <label>
          Password
          <input
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
          <label>
            Confirm password
            <input type="password" value={confirm} onChange={(e) => setConfirm(e.target.value)} autoComplete="new-password" required />
          </label>
        )}
        {error && <p className="error">{error}</p>}
        <button type="submit" disabled={busy}>
          {setupNeeded ? "Create admin" : "Sign in"}
        </button>
      </form>
    </div>
  );
}
