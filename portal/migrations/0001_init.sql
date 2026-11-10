-- S0 baseline (PORTAL_PLAN 6/S0): extensions the later milestones rely on.
-- No tenant tables yet: S1 adds users / orgs / memberships / projects /
-- api_keys / audit_events with Row Level Security.

-- gen_random_uuid() for primary keys, digest()/hmac() for API key
-- secret hashing and webhook signature checks.
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- Case-insensitive emails for users and invites (S1 login + invites).
CREATE EXTENSION IF NOT EXISTS citext;
