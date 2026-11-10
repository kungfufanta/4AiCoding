-- 0033 (M7b): sandboxes executed on workers.
--
-- nodes.api_token_enc: the credential the control plane uses to call a
-- worker's API (the worker generates it, sends it once on register; it is
-- stored encrypted with the instance secrets key). Empty = the node runs no
-- sandboxes for the control plane (an M6 heartbeat-only worker).
--
-- remote_sandboxes: sandboxes the control plane created on a worker. The
-- sandbox row itself lives in the worker's store; this row is how the
-- control plane routes /v1/sandboxes/{id}/... to the right node.
ALTER TABLE nodes ADD COLUMN api_token_enc TEXT NOT NULL DEFAULT '';

CREATE TABLE remote_sandboxes (
    sandbox_id TEXT PRIMARY KEY,
    node_id    TEXT NOT NULL,
    created_by TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);
CREATE INDEX remote_sandboxes_node_idx ON remote_sandboxes(node_id);
