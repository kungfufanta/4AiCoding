-- S1 (PORTAL_PLAN 6/S1): tenant tables + Row Level Security.
--
-- Isolation model (plan 1 #3, two layers): the Portal checks
-- ownership in Go, and Postgres enforces it again with RLS. Every
-- request runs inside a transaction that sets
--   app.org_id  — the org the request acts on (set after a
--                 membership check), and
--   app.user_id — the authenticated user.
-- A row is visible only when its org matches app.org_id OR the
-- app.user_id is a member of the row's org. A repository query that
-- forgets `WHERE org_id` therefore still cannot leak org B rows to
-- an org A user.

CREATE TABLE IF NOT EXISTS users (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    oidc_sub   TEXT NOT NULL UNIQUE,
    email      CITEXT NOT NULL UNIQUE,
    name       TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS orgs (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug       TEXT NOT NULL UNIQUE,
    name       TEXT NOT NULL,
    plan_id    TEXT NOT NULL DEFAULT 'free',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS memberships (
    org_id     UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role       TEXT NOT NULL CHECK (role IN ('owner', 'admin', 'member', 'billing')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, user_id)
);

CREATE TABLE IF NOT EXISTS invites (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    email      CITEXT NOT NULL,
    role       TEXT NOT NULL CHECK (role IN ('admin', 'member', 'billing')),
    token_hash TEXT NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    accepted_at TIMESTAMPTZ,
    created_by UUID NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, email)
);

CREATE TABLE IF NOT EXISTS projects (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    slug       TEXT NOT NULL,
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, slug)
);

CREATE TABLE IF NOT EXISTS audit_events (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    actor_type  TEXT NOT NULL,
    actor_id    TEXT NOT NULL DEFAULT '',
    action      TEXT NOT NULL,
    target      TEXT NOT NULL DEFAULT '',
    metadata    JSONB NOT NULL DEFAULT '{}',
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS audit_events_org_recent
    ON audit_events (org_id, occurred_at DESC);

CREATE TABLE IF NOT EXISTS web_sessions (
    id_hash    TEXT PRIMARY KEY, -- sha256 hex of the cookie token
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    csrf_hash  TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS web_sessions_user_idx ON web_sessions (user_id);

-- --- Row Level Security -------------------------------------------------
--
-- Policies reference memberships; memberships itself is scoped by
-- user_id (the session's own rows) or org_id (the org under review),
-- so the EXISTS inside the other policies always resolves without
-- recursion.

ALTER TABLE orgs ENABLE ROW LEVEL SECURITY;
CREATE POLICY orgs_tenant ON orgs FOR ALL
    USING (
        id = NULLIF(current_setting('app.org_id', true), '')::uuid
        OR EXISTS (
            SELECT 1 FROM memberships m
            WHERE m.org_id = orgs.id
              AND m.user_id = NULLIF(current_setting('app.user_id', true), '')::uuid
        )
    );

ALTER TABLE memberships ENABLE ROW LEVEL SECURITY;
CREATE POLICY memberships_tenant ON memberships FOR ALL
    USING (
        user_id = NULLIF(current_setting('app.user_id', true), '')::uuid
        OR org_id = NULLIF(current_setting('app.org_id', true), '')::uuid
    );

ALTER TABLE invites ENABLE ROW LEVEL SECURITY;
CREATE POLICY invites_tenant ON invites FOR ALL
    USING (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);

ALTER TABLE projects ENABLE ROW LEVEL SECURITY;
CREATE POLICY projects_tenant ON projects FOR ALL
    USING (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);

ALTER TABLE audit_events ENABLE ROW LEVEL SECURITY;
CREATE POLICY audit_tenant ON audit_events FOR ALL
    USING (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);

-- users + web_sessions carry no org_id: the global directory and the
-- session store are scoped by user, not org. They are NOT RLS-
-- protected; access only happens behind the authenticated BFF.

-- --- App database role ---------------------------------------------------
--
-- RLS is bypassed for superusers and table owners, so the portal
-- connects and queries as the non-owner role `portal_app`
-- (PORTAL_PLAN 3.4: "Role DB của app không phải owner bảng").
-- The migration runner's login receives membership of the role so
-- `SET ROLE portal_app` works when migration user = app user (the
-- default in docker-compose and the Helm chart).

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'portal_app') THEN
        CREATE ROLE portal_app NOLOGIN;
    END IF;
END
$$;

GRANT USAGE ON SCHEMA public TO portal_app;
GRANT SELECT, INSERT, UPDATE, DELETE
    ON users, orgs, memberships, invites, projects, audit_events, web_sessions
    TO portal_app;
-- Skip the grant when the login can already SET ROLE portal_app:
-- roles are cluster-wide, so portal_app may have been created by
-- another login (another portal database on the same server, or a
-- DBA), and re-granting it then fails with 42501 although nothing is
-- missing. PostgreSQL 16+ tracks the SET option separately (a
-- CREATEROLE creator holds ADMIN but not SET), older versions do not.
DO $$
BEGIN
    IF NOT pg_has_role(current_user, 'portal_app',
            CASE WHEN current_setting('server_version_num')::int >= 160000
                 THEN 'SET' ELSE 'MEMBER' END) THEN
        BEGIN
            EXECUTE format('GRANT portal_app TO %I', current_user);
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE EXCEPTION 'login % cannot grant itself role portal_app', current_user
                USING HINT = format('As a superuser run: GRANT portal_app TO %I;', current_user);
        END;
    END IF;
END
$$;
