-- 0036: host telemetry per fleet member.
--
-- telemetry_json is the latest hosttelemetry.Snapshot a node sent with its
-- heartbeat (CPU, memory, disk, network of the machine it runs on). It is
-- written by every member, local node included, so node metrics work on a
-- plain VPS or Docker host as well as in k3s. '{}' = never reported.
ALTER TABLE nodes ADD COLUMN telemetry_json TEXT NOT NULL DEFAULT '{}';
