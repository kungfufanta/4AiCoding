-- A3.1 (docs/AGENT_PLAN_A3.md §4): agent runs are durable Postgres
-- rows claimed by Portal workers with FOR UPDATE SKIP LOCKED + a
-- lease, so a reload, a lost connection or a pod restart never loses
-- one. agent_events is the append-only SSE log (replay always reads
-- the table; NOTIFY only wakes listeners); agent_messages keeps the
-- full LLM history (including tool turns) so a later run in the same
-- chat can continue where this one stopped.
--
-- RLS: a run is an org resource (the sandbox belongs to the org), so
-- every policy pins app.org_id — same shape as 0006. Workers run
-- outside requests and scope every statement with db.WithOrg(run.OrgID);
-- the ONLY cross-org path is the SECURITY DEFINER pair
-- agent_claim_next / agent_sweep_lost (the queue must be visible to
-- every worker) — both have dedicated tests.

CREATE TABLE IF NOT EXISTS agent_runs (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id           UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    sandbox_id       TEXT NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
    conversation_id  UUID NULL REFERENCES chat_conversations(id) ON DELETE SET NULL,
    created_by_user  UUID NULL REFERENCES users(id),
    created_by_key   TEXT NULL,                       -- api key_id when created via /v1
    prompt           TEXT NOT NULL CHECK (length(prompt) <= 32768),
    connection_id    UUID NULL REFERENCES model_connections(id) ON DELETE SET NULL,
    model            TEXT NOT NULL,
    max_steps        INT  NOT NULL DEFAULT 30  CHECK (max_steps BETWEEN 1 AND 50),
    timeout_s        INT  NOT NULL DEFAULT 900 CHECK (timeout_s BETWEEN 30 AND 1800),
    max_tokens       INT  NOT NULL DEFAULT 300000,
    status           TEXT NOT NULL DEFAULT 'queued'
                     CHECK (status IN ('queued','running','succeeded','failed','cancelled')),
    error_code       TEXT NULL,   -- worker_lost|worker_shutdown|timeout|max_steps|token_budget|llm_error|sandbox_unavailable|image_missing_tools|internal
    checkpoint_ref   TEXT NULL,   -- refs/4aicoding/checkpoints/<run_id>, NULL when the image has no git (A3.2 fills this)
    result           JSONB NULL,  -- {summary, files_changed[], build_status, preview_ok, steps}
    steps            INT    NOT NULL DEFAULT 0,
    input_tokens     BIGINT NOT NULL DEFAULT 0,
    output_tokens    BIGINT NOT NULL DEFAULT 0,
    reverted_at      TIMESTAMPTZ NULL,
    cancel_requested BOOLEAN NOT NULL DEFAULT false,
    lease_owner      TEXT NULL,
    lease_until      TIMESTAMPTZ NULL,
    idempotency_key  TEXT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at       TIMESTAMPTZ NULL,
    finished_at      TIMESTAMPTZ NULL
);

-- One active run per sandbox (plan §3 rule 4): the partial unique
-- index turns a double-create into a 409 even across racing inserts.
CREATE UNIQUE INDEX IF NOT EXISTS agent_runs_one_active_per_sandbox
    ON agent_runs (sandbox_id) WHERE status IN ('queued','running');
CREATE INDEX IF NOT EXISTS agent_runs_queue ON agent_runs (created_at) WHERE status = 'queued';
CREATE INDEX IF NOT EXISTS agent_runs_sandbox_recent ON agent_runs (sandbox_id, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS agent_runs_idem ON agent_runs (org_id, idempotency_key) WHERE idempotency_key IS NOT NULL;

CREATE TABLE IF NOT EXISTS agent_events (
    run_id     UUID NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
    seq        INT  NOT NULL,
    org_id     UUID NOT NULL,
    type       TEXT NOT NULL,   -- status|message|tool_call|tool_result|build|done|reverted
    data       JSONB NOT NULL,  -- capped at 256 KiB before INSERT (store.go)
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, seq)
);

CREATE TABLE IF NOT EXISTS agent_messages (       -- full LLM history for "continue in the same chat"
    run_id  UUID NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
    idx     INT  NOT NULL,
    org_id  UUID NOT NULL,
    role    TEXT NOT NULL CHECK (role IN ('system','user','assistant','tool')),
    content JSONB NOT NULL,      -- the llm.Msg JSON (role/content/tool_calls/tool_call_id)
    PRIMARY KEY (run_id, idx)
);

-- Chat integration (plan §9.1): a conversation pins one sandbox;
-- the assistant summary of a run lands in chat_messages so the chat
-- page shows what the agent did.
ALTER TABLE chat_conversations ADD COLUMN IF NOT EXISTS sandbox_id TEXT NULL REFERENCES sandboxes(id) ON DELETE SET NULL;
ALTER TABLE chat_messages ADD COLUMN IF NOT EXISTS agent_run_id UUID NULL REFERENCES agent_runs(id) ON DELETE SET NULL;

ALTER TABLE agent_runs ENABLE ROW LEVEL SECURITY;
CREATE POLICY agent_runs_tenant ON agent_runs FOR ALL
    USING (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);

ALTER TABLE agent_events ENABLE ROW LEVEL SECURITY;
CREATE POLICY agent_events_tenant ON agent_events FOR ALL
    USING (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);

ALTER TABLE agent_messages ENABLE ROW LEVEL SECURITY;
CREATE POLICY agent_messages_tenant ON agent_messages FOR ALL
    USING (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON agent_runs TO portal_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON agent_events TO portal_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON agent_messages TO portal_app;

-- NOTIFY plumbing (decision §2-8): the table carries the truth, NOTIFY
-- only wakes listeners. agent_runs_queued fires when a queued run is
-- inserted (workers wake to claim); agent_events fires per appended
-- event with payload "<run_id>:<seq>" (SSE followers wake to replay
-- from the table). Both are plain functions: pg_notify needs no
-- elevated rights and fires at COMMIT.
CREATE FUNCTION agent_runs_notify() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status = 'queued' THEN
        PERFORM pg_notify('agent_runs_queued', NEW.id::text);
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER agent_runs_notify AFTER INSERT ON agent_runs
    FOR EACH ROW EXECUTE FUNCTION agent_runs_notify();

CREATE FUNCTION agent_events_notify() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('agent_events', NEW.run_id::text || ':' || NEW.seq::text);
    RETURN NEW;
END $$;

CREATE TRIGGER agent_events_notify AFTER INSERT ON agent_events
    FOR EACH ROW EXECUTE FUNCTION agent_events_notify();

-- Claim the oldest queued run for this worker: sets it running with a
-- fresh lease, atomically, skipping rows other workers already hold
-- (FOR UPDATE SKIP LOCKED — plan §12 "Hai worker claim cùng run").
-- SECURITY DEFINER is the single cross-org door: the queue spans all
-- orgs, which RLS-scoped workers cannot see. lease_seconds is a
-- parameter so tests can shrink the lease (prod default 60).
CREATE OR REPLACE FUNCTION agent_claim_next(p_worker TEXT, p_lease_seconds INT)
RETURNS TABLE (
    id UUID, org_id UUID, sandbox_id TEXT, conversation_id UUID,
    created_by_user UUID, created_by_key TEXT, prompt TEXT,
    connection_id UUID, model TEXT, max_steps INT, timeout_s INT, max_tokens INT
) LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    UPDATE agent_runs SET
        status = 'running',
        lease_owner = p_worker,
        lease_until = now() + make_interval(secs => GREATEST(p_lease_seconds, 1)),
        started_at = COALESCE(started_at, now())
    WHERE id = (
        SELECT r.id FROM agent_runs r
        WHERE r.status = 'queued'
        ORDER BY r.created_at
        FOR UPDATE SKIP LOCKED
        LIMIT 1
    )
    RETURNING agent_runs.id, agent_runs.org_id, agent_runs.sandbox_id, agent_runs.conversation_id,
              agent_runs.created_by_user, agent_runs.created_by_key, agent_runs.prompt,
              agent_runs.connection_id, agent_runs.model, agent_runs.max_steps,
              agent_runs.timeout_s, agent_runs.max_tokens;
$$;

GRANT EXECUTE ON FUNCTION agent_claim_next(TEXT, INT) TO portal_app;

-- Reap runs whose worker died mid-flight: anything still 'running'
-- with a lease expired by p_grace_seconds becomes failed/worker_lost
-- with its final done event, in one transaction (plan §7 "Worker mất
-- lease ... chuyển run sang failed/worker_lost và ghi event done.
-- Không resume."). SECURITY DEFINER like the claim: the sweeper sees
-- every org's expired leases. The OUT columns are deliberately named
-- reaped_* — plpgsql treats bare column names that match OUT
-- parameters as ambiguous (org_id hit exactly that).
CREATE OR REPLACE FUNCTION agent_sweep_lost(p_grace_seconds INT)
RETURNS TABLE (reaped_id UUID, reaped_org UUID, reaped_sandbox TEXT)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN
        UPDATE agent_runs SET
            status = 'failed',
            error_code = 'worker_lost',
            finished_at = now()
        WHERE status = 'running'
          AND lease_until < now() - make_interval(secs => GREATEST(p_grace_seconds, 0))
        RETURNING id, org_id, sandbox_id
    LOOP
        INSERT INTO agent_events (run_id, seq, org_id, type, data)
        VALUES (
            r.id,
            (SELECT COALESCE(MAX(seq), 0) + 1 FROM agent_events e WHERE e.run_id = r.id),
            r.org_id,
            'done',
            jsonb_build_object('status', 'failed', 'error_code', 'worker_lost')
        );
        reaped_id := r.id;
        reaped_org := r.org_id;
        reaped_sandbox := r.sandbox_id;
        RETURN NEXT;
    END LOOP;
END $$;

GRANT EXECUTE ON FUNCTION agent_sweep_lost(INT) TO portal_app;
