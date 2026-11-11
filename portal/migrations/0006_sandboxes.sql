-- S3 (docs/PORTAL_PLAN.md 3.4, 6/S3): the Portal-side sandbox index and
-- the idempotency store for POST /v1/sandboxes + exec.
--
-- `sandboxes` is the Portal's ownership index ("sandbox id của sandboxd"
-- is the primary key): it answers "does this sandbox belong to this org"
-- without a round trip to sandboxd, and it carries the tags the gateway
-- filters on. sandboxd stays the source of truth for status; the
-- lifecycle webhook (4.4) and read-through sync keep the row fresh.

CREATE TABLE IF NOT EXISTS sandboxes (
    id              TEXT PRIMARY KEY, -- sandboxd sandbox id (ULID)
    org_id          UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    project_id      UUID REFERENCES projects(id) ON DELETE SET NULL,
    created_by_key  TEXT,             -- public key_id when created via /v1
    created_by_user UUID REFERENCES users(id), -- when created via the BFF
    agent_run_id    TEXT,
    status          TEXT NOT NULL DEFAULT 'pending',
    image           TEXT NOT NULL DEFAULT '',
    vcpu            DOUBLE PRECISION NOT NULL DEFAULT 0,
    memory_mb       INT NOT NULL DEFAULT 0,
    ports           INT[] NOT NULL DEFAULT '{}',
    error_message   TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    stopped_at      TIMESTAMPTZ NULL
);
CREATE INDEX IF NOT EXISTS sandboxes_org_recent ON sandboxes (org_id, created_at DESC);
CREATE INDEX IF NOT EXISTS sandboxes_project ON sandboxes (project_id);

ALTER TABLE sandboxes ENABLE ROW LEVEL SECURITY;
CREATE POLICY sandboxes_tenant ON sandboxes FOR ALL
    USING (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON sandboxes TO portal_app;

-- Idempotency-Key replay store (plan 5: "Idempotency-Key bắt buộc hỗ trợ
-- trên POST /v1/sandboxes và exec; lưu 24 giờ"). The handler hashes the
-- request body into request_hash; a mismatched replay is a 422 so a
-- reused key cannot return the wrong payload. Rows older than 24h are
-- pruned lazily on insert.
CREATE TABLE IF NOT EXISTS idempotency_keys (
    org_id          UUID NOT NULL,
    key             TEXT NOT NULL,
    request_hash    TEXT NOT NULL,
    response_status INT,
    response_body   BYTEA,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, key)
);

GRANT SELECT, INSERT, UPDATE, DELETE ON idempotency_keys TO portal_app;

-- Webhook replay guard (plan 4.4 via PORTAL_PLAN_V2 P4): sandboxd signs
-- every lifecycle event with an event id; the receiver records the ids it
-- has processed and skips duplicates. Rows older than 25h are pruned
-- lazily (sandboxd retries at most 24h).
CREATE TABLE IF NOT EXISTS webhook_events (
    event_id   TEXT PRIMARY KEY,
    event      TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

GRANT SELECT, INSERT, DELETE ON webhook_events TO portal_app;
