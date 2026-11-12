-- A3.3 (docs/AGENT_PLAN_A3.md §11 "A3.3 — Revert + continue + usage"):
-- the agent worker records every terminal run into the usage_events
-- ledger (P5) so the A5 monthly org quota can sum tokens the same way
-- it sums sandbox_seconds today. Two rows per run, idempotent via the
-- source_event_id UNIQUE (a worker that retries after a crash between
-- the terminal write and the ledger insert cannot double-apply):
--
--   agent_tokens  quantity = input_tokens + output_tokens, unit tokens
--   agent_steps   quantity = steps,                         unit steps
--
-- Migration 0007 pinned kind to the sandbox kinds; extend the CHECK.
ALTER TABLE usage_events DROP CONSTRAINT IF EXISTS usage_events_kind_check;
ALTER TABLE usage_events ADD CONSTRAINT usage_events_kind_check
    CHECK (kind IN ('sandbox_seconds','vcpu_seconds','exec_seconds',
                    'agent_tokens','agent_steps'));
