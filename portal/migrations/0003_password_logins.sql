-- S1.5: local username/password login (bootstrap admin).
--
-- The owner decision (2026-11-11) adds a local admin account so the
-- dashboard is reachable before — or without — an OIDC provider:
-- https://chat.devhub.io.vn ships with admin login enabled while the
-- Zitadel setup is pending. PORTAL_PLAN 1.4 still prefers SSO; the
-- password path is the explicit fallback (no self-service signup,
-- no MFA). The bootstrap user + hash are created by
-- internal/passwordauth.EnsureBootstrap at boot (PORTAL_ADMIN_USERNAME
-- / _PASSWORD / _EMAIL, defaults admin / admin123).
--
-- user-scoped like users / web_sessions (no org_id): NOT RLS-
-- protected; access happens behind the authenticated BFF only.

CREATE TABLE IF NOT EXISTS password_logins (
    user_id       UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    username      CITEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL, -- bcrypt (golang.org/x/crypto/bcrypt)
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

GRANT SELECT, INSERT, UPDATE, DELETE ON password_logins TO portal_app;
