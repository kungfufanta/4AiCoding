-- A2 review fix. 0008's single FOR ALL policy used
-- `org_id IS NULL OR org_id = app.org_id` for both USING and the
-- implied WITH CHECK, so any org context (every user owns a personal
-- org) could UPDATE or DELETE the platform rows (org_id NULL): repoint
-- the platform connection's base_url at its own server and receive
-- the shared platform key on the next Test or chat call.
--
-- Reads stay as before (every org sees the platform metadata, never
-- the key, which the API does not return). Writes to a platform row
-- now need an empty app.org_id, which only the platform-admin path
-- (no org scope) has; writes to an org row need that org's context.

DROP POLICY IF EXISTS model_connections_tenant ON model_connections;

CREATE POLICY model_connections_read ON model_connections FOR SELECT
    USING (
        org_id IS NULL
        OR org_id = NULLIF(current_setting('app.org_id', true), '')::uuid
    );

CREATE POLICY model_connections_insert ON model_connections FOR INSERT
    WITH CHECK (
        (org_id IS NULL AND NULLIF(current_setting('app.org_id', true), '') IS NULL)
        OR org_id = NULLIF(current_setting('app.org_id', true), '')::uuid
    );

CREATE POLICY model_connections_update ON model_connections FOR UPDATE
    USING (
        (org_id IS NULL AND NULLIF(current_setting('app.org_id', true), '') IS NULL)
        OR org_id = NULLIF(current_setting('app.org_id', true), '')::uuid
    )
    WITH CHECK (
        (org_id IS NULL AND NULLIF(current_setting('app.org_id', true), '') IS NULL)
        OR org_id = NULLIF(current_setting('app.org_id', true), '')::uuid
    );

CREATE POLICY model_connections_delete ON model_connections FOR DELETE
    USING (
        (org_id IS NULL AND NULLIF(current_setting('app.org_id', true), '') IS NULL)
        OR org_id = NULLIF(current_setting('app.org_id', true), '')::uuid
    );

-- 0008's one-default index treats NULLs as distinct, so it never
-- limited the platform layer (org_id NULL) to one default.
DROP INDEX IF EXISTS model_connections_one_default_per_org;
CREATE UNIQUE INDEX IF NOT EXISTS model_connections_one_default_per_org
    ON model_connections (org_id) NULLS NOT DISTINCT WHERE is_default;
