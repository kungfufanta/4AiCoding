-- 0031 (M3c): human-in-the-loop approvals (PLAN §8 #11). A risky request is
-- parked here; an admin with a console session approves or denies it; the
-- approval is valid ONCE, for the exact payload (payload_hash), until it
-- expires.
CREATE TABLE approval (
    id           TEXT PRIMARY KEY,
    action       TEXT NOT NULL,
    target       TEXT NOT NULL,
    payload      TEXT NOT NULL,  -- canonical JSON of what was requested
    payload_hash TEXT NOT NULL,  -- sha256(action, target, payload)
    status       TEXT NOT NULL CHECK (status IN ('pending', 'approved', 'denied', 'used', 'expired')),
    requested_by TEXT NOT NULL,  -- actor kind:user
    decided_by   TEXT,
    reason       TEXT,
    created_at   INTEGER NOT NULL,
    decided_at   INTEGER,
    expires_at   INTEGER NOT NULL
);
CREATE INDEX approval_status_idx ON approval(status, created_at);
