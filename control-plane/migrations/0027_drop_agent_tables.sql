-- 0027: 4AiCoding is a lifecycle-only control plane. Drop the tables and
-- columns that only served the removed coding-agent / app layer.
DROP TABLE IF EXISTS app_config;
DROP TABLE IF EXISTS task;
DROP TABLE IF EXISTS agent_instance;
DROP TABLE IF EXISTS app;
DROP TABLE IF EXISTS git_credential;
DROP TABLE IF EXISTS model_provider;

DROP INDEX IF EXISTS idx_sandbox_app_id;
ALTER TABLE sandbox DROP COLUMN app_id;

DROP INDEX IF EXISTS idx_snapshot_app;
ALTER TABLE snapshot DROP COLUMN source_app_id;

ALTER TABLE instance_settings DROP COLUMN agent_default_models;

-- app_events stays as the sandbox/snapshot activity timeline.
DROP INDEX IF EXISTS idx_app_events_app;
DROP INDEX IF EXISTS idx_app_events_task;
ALTER TABLE app_events DROP COLUMN app_id;
ALTER TABLE app_events DROP COLUMN task_id;
CREATE INDEX IF NOT EXISTS idx_app_events_sandbox ON app_events(owner_token, sandbox_id, id);
