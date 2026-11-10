-- 0028: per-sandbox runtime spec (M3a). One optional row per sandbox. A
-- missing row, or a NULL/empty column, means "use the instance default".
--   image        custom image; NULL = follow SANDBOXD_IMAGE (moves with it)
--   command_json JSON array of strings; NULL = the image's CMD
--   env_json     JSON object of declared env; the ONLY env a sandbox gets
--   cpus         --cpus limit; NULL/0 = no hard limit (cpu-shares only)
--   memory_mb    --memory hard limit; NULL/0 = instance default
CREATE TABLE sandbox_spec (
    sandbox_id   TEXT PRIMARY KEY REFERENCES sandbox(id) ON DELETE CASCADE,
    image        TEXT,
    command_json TEXT,
    env_json     TEXT,
    cpus         REAL,
    memory_mb    INTEGER
);
