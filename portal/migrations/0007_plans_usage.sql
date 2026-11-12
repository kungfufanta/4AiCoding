-- P5 (docs/PORTAL_PLAN.md 3.4 + 6/S4): the plan catalog and the usage
-- event ledger. `plans` is platform data (seeded, not tenant-owned);
-- `usage_events` is tenant-owned and RLS-scoped, written idempotently
-- from the sandboxd lifecycle webhook (one row per source_event_id).
--
-- Design notes:
--   * `plans` has no RLS: it is a small platform catalog (free / pro /
--     enterprise…). orgs.plan_id (0002_tenants_rls.sql) is a soft FK
--     to plans.id (TEXT) — kept soft so a missing plan row does not
--     break existing orgs during migration.
--   * `usage_events` is INSERT-only from the webhook receiver. The
--     webhook already dedupes by sandboxd event id; the
--     `source_event_id UNIQUE` is the last line of defence against a
--     double-apply (e.g. a replayed webhook after a Portal restart).
--   * `quantity` is the integer count in the named `unit`
--     (sandbox_seconds, vcpu_seconds). The aggregator sums by kind +
--     occurred_at::date; nothing reads individual rows for totals.
--   * Pruning: usage_events are kept for 90 days (plan §S4 keeps the
--     "monthly" rollup indefinitely via aggregate queries on this
--     table; the daily rollup below is a cheap GROUP BY). A prune job
--     (P6 or A5) can DELETE WHERE occurred_at < now() - interval '90
--     days'.

CREATE TABLE IF NOT EXISTS plans (
    id                       TEXT PRIMARY KEY,
    name                     TEXT NOT NULL,
    max_concurrent_sandboxes INT NOT NULL CHECK (max_concurrent_sandboxes >= 0),
    max_vcpu_per_sandbox     DOUBLE PRECISION NOT NULL CHECK (max_vcpu_per_sandbox >= 0),
    max_ttl_seconds          INT NOT NULL CHECK (max_ttl_seconds >= 0),
    monthly_vcpu_seconds     BIGINT NOT NULL CHECK (monthly_vcpu_seconds >= 0),
    egress_mode              TEXT NOT NULL DEFAULT 'allowlist'
                             CHECK (egress_mode IN ('none','allowlist','open')),
    rate_limit_rps           INT NOT NULL DEFAULT 0 CHECK (rate_limit_rps >= 0),
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Seed the two plans every org starts under. orgs.plan_id defaults to
-- 'free' (0002_tenants_rls.sql); pro/enterprise are listed here so the
-- Usage page can show the upgrade ceiling without a code change.
INSERT INTO plans (id, name, max_concurrent_sandboxes, max_vcpu_per_sandbox,
                   max_ttl_seconds, monthly_vcpu_seconds, egress_mode, rate_limit_rps)
VALUES
    ('free',       'Free',       2,  1.0,  1800,  72000,  'allowlist', 2),
    ('pro',        'Pro',        8,  4.0, 21600, 540000,  'allowlist', 20),
    ('enterprise', 'Enterprise', 32, 8.0, 86400, 5400000, 'open',      120)
ON CONFLICT (id) DO NOTHING;

CREATE TABLE IF NOT EXISTS usage_events (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id          UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    project_id      UUID REFERENCES projects(id) ON DELETE SET NULL,
    sandbox_id      TEXT NOT NULL,
    kind            TEXT NOT NULL CHECK (kind IN
                        ('sandbox_seconds','vcpu_seconds','exec_seconds')),
    quantity        BIGINT NOT NULL CHECK (quantity >= 0),
    unit            TEXT NOT NULL DEFAULT 'seconds',
    source_event_id TEXT NOT NULL,
    occurred_at     TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (source_event_id)
);
CREATE INDEX IF NOT EXISTS usage_events_org_day
    ON usage_events (org_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS usage_events_sandbox
    ON usage_events (sandbox_id, occurred_at DESC);

ALTER TABLE usage_events ENABLE ROW LEVEL SECURITY;
CREATE POLICY usage_events_tenant ON usage_events FOR ALL
    USING (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);

GRANT SELECT, INSERT ON usage_events TO portal_app;
GRANT SELECT ON plans TO portal_app;

-- Track the start of the most recent run so the webhook receiver can
-- bill sandbox_seconds on stop/delete. stopped_at already exists
-- (0006); started_at is its mirror for the current run. A sandbox
-- that is restarted bills a fresh window from the new started_at.
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS started_at TIMESTAMPTZ NULL;

