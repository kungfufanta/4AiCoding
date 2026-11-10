-- 0030 (M3b): instance policy, per-sandbox lifecycle and egress.

-- Generic instance configuration (key -> JSON). "policy" holds the admin
-- policy enforced by the policy decision point (internal/policy).
CREATE TABLE instance_config (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);

-- Per-sandbox idle timeout (NULL = the instance threshold) and egress
-- allowlist (JSON array of hostnames; NULL/empty = no egress).
ALTER TABLE sandbox_spec ADD COLUMN idle_timeout_secs INTEGER;
ALTER TABLE sandbox_spec ADD COLUMN egress_json TEXT;
