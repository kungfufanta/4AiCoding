-- 0034: per-sandbox network mode and block list.
--   egress_mode       none | allowlist | open; NULL = allowlist (the
--                     behaviour of every sandbox created before this)
--   egress_deny_json  JSON array of hosts / IPs / CIDRs that are always
--                     refused, in every mode
ALTER TABLE sandbox_spec ADD COLUMN egress_mode TEXT;
ALTER TABLE sandbox_spec ADD COLUMN egress_deny_json TEXT;
