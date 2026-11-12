-- A2 (AGENT_PLAN §5.2): model connections. Two layers:
--   * platform connections (org_id NULL) seeded by the platform admin,
--     every org reads the metadata (never the key);
--   * org connections (org_id NOT NULL) created by the org's owner/admin,
--     private to that org (RLS).
--
-- Each connection carries the LLM provider config: protocol (openai |
-- anthropic), flavor (generic | webhub), base_url, default_model, the
-- allowed model list, key_mode (shared | per_org), timeout, max_tokens.
-- The key is encrypted at rest (AES-256-GCM) in model_connection_keys;
-- the API never returns it.
--
-- The Chat MVP (internal/llm) and the future agent runtime (A3) both
-- resolve the effective connection in this order:
--   1. org's is_default connection;
--   2. platform's is_default connection;
--   3. env PORTAL_DEFAULT_LLM_* (kept as a seed/fallback for dev).

CREATE TABLE IF NOT EXISTS model_connections (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id          UUID NULL REFERENCES orgs(id) ON DELETE CASCADE, -- NULL = platform
    name            TEXT NOT NULL,
    protocol        TEXT NOT NULL CHECK (protocol IN ('openai','anthropic')),
    flavor          TEXT NOT NULL DEFAULT 'generic' CHECK (flavor IN ('generic','webhub')),
    base_url        TEXT NOT NULL,
    default_model   TEXT NOT NULL,
    models          JSONB NOT NULL DEFAULT '[]',  -- [{id,context_window,max_output,supports_tools}]
    key_mode        TEXT NOT NULL DEFAULT 'shared' CHECK (key_mode IN ('shared','per_org')),
    timeout_s       INT NOT NULL DEFAULT 60 CHECK (timeout_s > 0 AND timeout_s <= 600),
    max_tokens      INT NOT NULL DEFAULT 4096 CHECK (max_tokens > 0 AND max_tokens <= 32768),
    enabled         BOOLEAN NOT NULL DEFAULT true,
    is_default      BOOLEAN NOT NULL DEFAULT false,
    created_by      UUID NULL REFERENCES users(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- One default per (org_id, enabled). Platform default and org default
-- are independent (the org's default wins when present).
CREATE UNIQUE INDEX IF NOT EXISTS model_connections_one_default_per_org
    ON model_connections (org_id) WHERE is_default;
-- Org connections are scoped by org_id; platform connections have
-- org_id IS NULL so the policy below lets every org read them.
ALTER TABLE model_connections ENABLE ROW LEVEL SECURITY;
CREATE POLICY model_connections_tenant ON model_connections FOR ALL
    USING (
        org_id IS NULL  -- platform: every org sees the metadata
        OR org_id = NULLIF(current_setting('app.org_id', true), '')::uuid
    );

-- Per-org keys for key_mode = per_org connections (WebHub). Each org
-- gets its own wh_ key; the key is AES-GCM encrypted with the master
-- PORTAL_SECRETS_KEY. The (connection_id, org_id) is unique so an org
-- has at most one key per connection.
CREATE TABLE IF NOT EXISTS model_connection_keys (
    connection_id   UUID NOT NULL REFERENCES model_connections(id) ON DELETE CASCADE,
    org_id          UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    api_key_enc     BYTEA NOT NULL,  -- AES-256-GCM(key, plaintext)
    key_label       TEXT NOT NULL DEFAULT '',  -- last 4 chars for UI hint
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    rotated_at      TIMESTAMPTZ NULL,
    PRIMARY KEY (connection_id, org_id)
);
ALTER TABLE model_connection_keys ENABLE ROW LEVEL SECURITY;
CREATE POLICY model_connection_keys_tenant ON model_connection_keys FOR ALL
    USING (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON model_connections TO portal_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON model_connection_keys TO portal_app;
