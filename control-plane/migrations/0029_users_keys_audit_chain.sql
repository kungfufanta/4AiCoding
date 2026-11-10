-- 0029 (M3b): named users with roles, least-privilege API keys, and a
-- tamper-evident audit chain.

-- Users replace the single console password. The old password (if any)
-- becomes the user "admin" with role admin, so existing installs keep
-- working with username "admin".
CREATE TABLE app_user (
    id            TEXT PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE COLLATE NOCASE,
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL CHECK (role IN ('admin', 'operator', 'viewer')),
    disabled      INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
INSERT INTO app_user (id, username, password_hash, role, disabled, created_at, updated_at)
    SELECT 'legacy-admin', 'admin', password_hash, 'admin', 0, updated_at, updated_at
      FROM console_auth WHERE id = 1;
DROP TABLE console_auth;

-- Sessions now belong to a user. Pre-user sessions cannot be attributed,
-- so everyone signs in again once.
DELETE FROM console_session;
ALTER TABLE console_session ADD COLUMN user_id TEXT REFERENCES app_user(id) ON DELETE CASCADE;

-- API keys: a role (never above the creator's), an optional expiry, and an
-- optional binding to one sandbox. Existing keys keep full access.
ALTER TABLE api_key ADD COLUMN role TEXT NOT NULL DEFAULT 'admin';
ALTER TABLE api_key ADD COLUMN expires_at INTEGER;
ALTER TABLE api_key ADD COLUMN sandbox_id TEXT;

-- Audit hash chain: hash = sha256(prev_hash || canonical row). Rows written
-- before this migration have NULL hashes and are outside the chain.
ALTER TABLE audit_log ADD COLUMN prev_hash TEXT;
ALTER TABLE audit_log ADD COLUMN hash TEXT;
