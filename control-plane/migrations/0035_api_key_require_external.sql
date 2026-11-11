-- 0035 (Portal S3a): the require_external flag on API keys. A key created
-- for the Portal carries it: any sandbox it creates must tag
-- external.workspace_id (the org), else the create is a 400. Existing keys
-- keep today's behaviour (0 = not required).
ALTER TABLE api_key ADD COLUMN require_external INTEGER NOT NULL DEFAULT 0;
