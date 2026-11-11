-- S2 (docs/PORTAL_PLAN.md 3.2, 6/S2): API keys for agents, plus the
-- SECURITY DEFINER helper the Bearer middleware uses.
--
-- Key shape (plan 3.2): sbx_live_<keyid>_<secret> with a 12-char
-- base32 key id and a ~256-bit base62 secret. Only key_id and
-- sha256(secret) are stored; the secret is shown once at creation
-- (plan 1 #5).

CREATE TABLE IF NOT EXISTS api_keys (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id       UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    project_id   UUID REFERENCES projects(id) ON DELETE SET NULL,
    name         TEXT NOT NULL,
    key_id       TEXT NOT NULL UNIQUE,
    secret_hash  TEXT NOT NULL, -- sha256 hex of the secret part
    scopes       TEXT[] NOT NULL DEFAULT '{}',
    ip_allowlist CIDR[] NULL,   -- reserved (plan 3.2); enforced from S3
    expires_at   TIMESTAMPTZ NULL,
    last_used_at TIMESTAMPTZ NULL,
    revoked_at   TIMESTAMPTZ NULL,
    created_by   UUID NOT NULL REFERENCES users(id),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS api_keys_org_recent ON api_keys (org_id, created_at DESC);

ALTER TABLE api_keys ENABLE ROW LEVEL SECURITY;
CREATE POLICY api_keys_tenant ON api_keys FOR ALL
    USING (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON api_keys TO portal_app;

-- pgcrypto.digest() for the sha256 below (already present on the
-- staging database; postgres images ship contrib).
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- Bearer verification cannot go through the session middleware: the
-- key itself is the identity, so there is no app.org_id to pin yet.
-- Rather than a second, non-RLS database role, the lookup runs in a
-- SECURITY DEFINER function that returns a row only when key_id AND
-- sha256(secret) both match and the key is live. A wrong secret (or a
-- foreign key id) yields an empty set — nothing else is exposed.
-- Touching last_used_at rides along in the same UPDATE ... RETURNING.
CREATE OR REPLACE FUNCTION api_key_verify(p_key_id TEXT, p_secret TEXT)
RETURNS TABLE (
    id         UUID,
    org_id     UUID,
    project_id UUID,
    scopes     TEXT[],
    expires_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ
)
LANGUAGE sql
VOLATILE
SECURITY DEFINER
SET search_path = public
AS $$
    WITH touch AS (
        UPDATE api_keys
        SET last_used_at = now()
        WHERE key_id = p_key_id
          AND secret_hash = encode(digest(p_secret, 'sha256'), 'hex')
          AND revoked_at IS NULL
          AND (expires_at IS NULL OR expires_at > now())
        RETURNING id, org_id, project_id, scopes, expires_at, revoked_at
    )
    SELECT id, org_id, project_id, scopes, expires_at, revoked_at FROM touch
$$;

REVOKE ALL ON FUNCTION api_key_verify(TEXT, TEXT) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION api_key_verify(TEXT, TEXT) TO portal_app;
