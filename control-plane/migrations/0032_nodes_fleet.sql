-- 0032 (M6): worker fleet + placements (PLAN §5, §9 M6).
--
-- nodes: one row per fleet member. The control plane itself registers a
-- role='local' row at boot (it executes sandboxes in the all-in-one
-- topology); remote workers upsert themselves through the /internal
-- register + heartbeat endpoints using the worker token. drain_state is
-- the admin-controlled admission switch: a draining node must not
-- receive new sandboxes.
--
-- sandbox_placements: the node a sandbox is assigned to, written in the
-- same transaction that inserts the sandbox row. The FK cascade keeps
-- placement lifecycle equal to the sandbox lifecycle (DELETE sandbox ->
-- placement gone).
CREATE TABLE nodes (
    node_id       TEXT PRIMARY KEY,
    role          TEXT NOT NULL DEFAULT 'worker' CHECK (role IN ('local', 'worker')),
    address       TEXT NOT NULL DEFAULT '',  -- worker-reachable base URL (informational)
    version       TEXT NOT NULL DEFAULT '',
    capacity_json TEXT NOT NULL DEFAULT '{}', -- {"cpus":n,"memory_mb":n,"disk_mb":n}
    used_json     TEXT NOT NULL DEFAULT '{}', -- worker-reported usage; empty = derive from placements
    drain_state   TEXT NOT NULL DEFAULT 'active' CHECK (drain_state IN ('active', 'drain')),
    registered_at INTEGER NOT NULL,
    heartbeat_at  INTEGER NOT NULL
);

CREATE INDEX nodes_heartbeat_idx ON nodes(heartbeat_at);

CREATE TABLE sandbox_placements (
    sandbox_id  TEXT PRIMARY KEY REFERENCES sandbox(id) ON DELETE CASCADE,
    node_id     TEXT NOT NULL,
    state       TEXT NOT NULL DEFAULT 'placed' CHECK (state IN ('placed', 'draining', 'evicted')),
    reserved_at INTEGER NOT NULL
);

CREATE INDEX sandbox_placements_node_idx ON sandbox_placements(node_id);
