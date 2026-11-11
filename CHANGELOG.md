# Changelog

All notable changes to 4AiCoding. The project started from
[tastyeffectco/sandboxd](https://github.com/tastyeffectco/sandboxd) at commit
`92f4ffd`; see that repository for the history before the fork.

## Unreleased

### Portal P0: no default admin password
- The bootstrap password login no longer defaults to `admin/admin123`.
  It is off unless `PORTAL_ADMIN_PASSWORD` is set (min 12 characters);
  a shorter value fails at startup.
- With the login off, the portal removes stored password logins and
  their sessions at boot, so the old default cannot be used again.
- Changing `PORTAL_ADMIN_PASSWORD` signs out the admin's existing
  sessions.
- k3s deploy: optional `PORTAL_ADMIN_PASSWORD` secret on the k3s-1
  environment, passed to the pod through the `portal-database` Secret.

### Portal S1 fixes
- Org slugs: creating an org whose slug another tenant already uses no
  longer fails with a 500. RLS hid the other org, so the "is it free"
  check always said yes; the insert now probes the unique index
  (`ON CONFLICT (slug) DO NOTHING`) and retries with a suffix.
- First sign-in creates the user and personal org in one transaction,
  and any signed-in user with no org gets a personal org, so an earlier
  half-finished bootstrap no longer leaves a user orgless.
- Roles: only an owner can grant `owner`, and nobody can change their
  own role (an admin could promote themselves to owner).
- Invites can only be accepted by the email they were sent to
  (`403 invite_email_mismatch`).
- Signing in with a second identity provider account that has an
  email already in use answers `409 email_in_use` instead of a 500.
  Accounts are not linked automatically. An ID token with
  `email_verified: false` is refused (`403 email_unverified`).
- The session cookie lifetime follows `PORTAL_SESSION_TTL` instead of
  a fixed 12h.

### Portal S1 — login and tenants (docs/PORTAL_PLAN.md 6/S1)
- Migration `0002_tenants_rls.sql`: `users`, `orgs`, `memberships`,
  `invites`, `projects`, `audit_events`, `web_sessions` + Row Level
  Security (org visibility by `app.org_id` or membership in the row's
  org) and the non-owner `portal_app` role the serving pool assumes
  (`PORTAL_DB_ROLE`, `-` to disable) — superusers/owners bypass RLS.
- SSO login (plan 3.1): OIDC Authorization Code + PKCE
  (`internal/oidcauth`, works with Zitadel/Keycloak/Auth0), BFF
  callbacks, first-login personal org bootstrap, `web_sessions` with
  sha256-hashed tokens, HttpOnly `SameSite=Lax` cookies (Secure on
  https) and double-submit CSRF bound to the session hash. Logout,
  session sweep.
- Dashboard BFF: `/api/me`, org create, projects create/list,
  members list, role change (admin/owner only, owner protected),
  invites (token shown once, sha256 stored, single-use accept via
  link carrying the org id), audit trail. Foreign orgs answer 404,
  never 403.
- Dashboard SPA (`portal/web`): sign-in screen, org switcher +
  creation, Projects / Members / Audit tabs, invite flow with the
  one-time link, role selects.
- RLS proof test (`rls_test.go`): WHERE-less SELECT/UPDATE/DELETE
  from an org-A session cannot read or write org B, INSERTs into a
  foreign org are rejected by `WITH CHECK`; invite accept is the one
  sanctioned cross-org path and the token is single-use.
- Tests: hermetic cookie-flag + CSRF middleware tests; full BFF e2e
  against Postgres (CI `postgres:16` service; `PORTAL_TEST_DATABASE_URL`).

### Portal S0 — project skeleton (docs/PORTAL_PLAN.md 6/S0)
- New `portal/` Go module (`cmd/portal`): reads `PORTAL_*` env config
  (`PORTAL_ADDR`, required `PORTAL_DATABASE_URL`, `PORTAL_DEBUG`,
  `PORTAL_PUBLIC_URL`, `PORTAL_SESSION_TTL`, OIDC placeholders for S1),
  connects to Postgres via pgx, applies its own embedded migrations
  (`portal/migrations/NNNN_*.sql`, advisory-locked, idempotent) and
  serves `/healthz`, `/readyz` (Postgres ping) plus the embedded SPA
  with CSP / `X-Frame-Options: DENY` headers. JSON logging with
  request ids mirrors control-plane/internal/logging.
- `portal/web/`: Vite + React 19 + TypeScript SPA skeleton (health
  cards); embedded into the binary through `internal/webfs` so the
  dashboard shares one origin with the future BFF (plan 3.1).
- `docker-compose.portal.yml` for dev: Postgres 16 + Portal, Zitadel
  behind the `idp` profile.
- `deploy/helm/portal`: Deployment + Service + optional Ingress,
  DSN via `database.url` (chart Secret) or `database.existingSecret`,
  non-root distroless pod, /healthz + /readyz probes.
- CI: `portal-unit` (gofmt/vet/build/test against a postgres:16
  service + helm lint/template) and `portal-console` (SPA build).

### M7b — sandboxes run on workers
- Executor mode: a sandboxd with `SANDBOXD_CONTROL_PLANE_URL` (+
  `SANDBOXD_WORKER_TOKEN`, `SANDBOXD_NODE_ID`, `SANDBOXD_NODE_ADDRESS`) runs
  sandboxes for a control plane. It generates its own API token
  (`<data>/state/node-api-token`, 0600), registers with it, reports usage,
  applies the drain state and the control plane's policy from each
  heartbeat, and accepts no other API credential.
- Control plane: stores each node's token encrypted (migration `0033`,
  `nodes.api_token_enc`) and binds the node id to it (a re-register with
  another token is 409). `POST /v1/sandboxes` places the sandbox on the
  schedulable node with the most free memory; `remote_sandboxes` records
  worker placements; every `/v1/sandboxes/{id}/...` route is forwarded to
  the owning worker (streaming included); `GET /v1/sandboxes` merges all
  nodes and reports `unreachable_nodes`. Approval gates run on the control
  plane and the cleared action is passed on with `X-Sandboxd-Fleet-Grant`,
  honoured only from the control plane's credential. Forwarded writes are
  audited as `sandbox.forward`.
- New `DELETE /v1/nodes/{id}` (admin): forget a worker; refused while it
  still runs sandboxes.
- Helm chart: `executor.enabled` runs sandboxd on the node's Docker
  (host network, Docker socket, host data dir, root with only CHOWN /
  DAC_OVERRIDE / FOWNER).
- CI: `scripts/e2e-fleet-exec.sh` (control plane + executor: placement,
  exec/logs/list through the control plane, policy sync, approval for a
  remote delete, drain) and an executor step in the k3s e2e.

### M7a — k3s worker: Helm chart, worker pod joins the fleet
- New Helm chart `deploy/helm/sandboxd-worker`: runs `sandboxd worker` as a
  DaemonSet (node id = Kubernetes node name, stable across restarts) or as a
  single-replica Deployment with a fixed `nodeId`. The worker token comes from
  a chart-made Secret or `workerToken.existingSecret`; capacity, heartbeat
  interval, image and scheduling are values. The template fails without
  `controlPlane.url`, a token, or (Deployment) a `nodeId`.
- Hardened pod: non-root (65532), read-only root filesystem, no
  capabilities, no privilege escalation, seccomp RuntimeDefault, no service
  account token, no host mounts.
- CI: new `k3s` job installs k3s, imports the image built from
  `control-plane/Dockerfile`, lints and installs the chart and runs
  `scripts/e2e-k3s.sh`: the pod registers under the node name with the
  configured capacity, learns drain/undrain, keeps its id across a rollout
  restart and goes offline after uninstall.
- Makefile: `e2e-k3s`, `helm-lint`.

### M6 — worker fleet: registration, placement, drain
- `sandboxd worker` subcommand: the same binary joins the control plane as a
  fleet member — `--control-plane-url`, `--worker-token`, `--node-id`,
  `--address`, `--capacity-{cpus,memory-mb,disk-mb}`, `--heartbeat-interval-sec`
  (env fallbacks: `SANDBOXD_CONTROL_PLANE_URL`, `SANDBOXD_WORKER_TOKEN`, …).
  It registers with backoff, heartbeats, re-registers when the control plane
  forgets it and logs the drain state it learns from each heartbeat.
- New internal fleet API, reachable ONLY with the `SANDBOXD_WORKER_TOKEN`
  bearer (any other credential — or no credential — gets 404; a worker token
  gets 403 on every non-internal route): `POST /internal/nodes/register` and
  `POST /internal/nodes/heartbeat` (response carries the node's desired
  `drain_state`).
- New operator API (PLAN §4 "Node / cluster"): `GET /v1/nodes`,
  `GET /v1/nodes/{id}/capacity`, `GET /v1/capacity` and the admin-only
  `POST /v1/nodes/{id}/drain` / `undrain`. Node status is derived from the
  heartbeat lease (`SANDBOXD_NODE_LEASE_SECONDS`, default 90). Drain/undrain
  are audited (`node.register`, `node.drain`, `node.undrain`).
- Migration `0032`: `nodes` (role local/worker, capacity JSON, drain state,
  heartbeat) and `sandbox_placements` (sandbox → node, FK cascade on delete).
  The control plane registers itself as the role=`local` node at boot.
- Placement: every sandbox create picks the executor node via
  `fleet.Executor` and stores the placement in the same transaction as the
  sandbox row; `GET /v1/sandboxes/{id}` reports it as `node`. In M6 the only
  executor is the `local` node — draining it refuses new sandboxes with a 503
  (Retry-After: 30). Workers already drain for membership; executing on them
  lands in M7 (k3s).
- Console: new **Admin → Nodes** tab (live refresh, every 10 s) showing each
  node's role, status, drain state, capacity vs. used and heartbeat, with
  admin-only Drain/Undrain actions.
- CI: new `fleet e2e` integration step (`scripts/e2e-fleet.sh`, `make
  e2e-fleet`) — two real worker processes register, drain, learn the drain
  from their heartbeat, go offline after the lease, and the local node
  blocks/unblocks creation around it.

### M5 — full CI and PR preview
- `scripts/e2e-auth.sh` (CI integration job, `make e2e-auth`): the access
  layer end to end with auth on — login, CSRF, roles, disabled users, API key
  role/scope/TTL/binding/revocation, keep-alive policy, the approvals flow and
  the audit chain.
- `scripts/preview.sh` + `.github/workflows/preview.yml` (`make preview`):
  builds the whole compose stack, checks the Traefik routes and console
  headers, drives the console with Playwright and uploads screenshots.
- Fix: the console's security headers (CSP, X-Frame-Options, …) were dropped
  on pages and assets because nginx does not inherit `add_header` into
  locations that set their own; they now live in an included snippet.
- `SANDBOXD_SANDBOX_NOFILE` sets the sandbox open-files limit (default 65536;
  lower it on nested Docker hosts whose own limit is smaller).

### M4 — web console
- New `console/`: React 19 + TypeScript SPA built with Vite, no runtime
  dependencies besides React. Screens: login / first-run setup, sandbox list,
  sandbox detail (logs with SSE follow, exec, live metrics, keep-alive,
  delete), new sandbox (every spec field, with the matching curl), network
  (preview ports, egress allowlist) and admin (policy, approvals, users,
  API keys, audit log with chain verification, domain and certificates).
- Every unsafe request sends `X-Sandboxd-CSRF`; actions that need approval
  show the approval id and accept it on retry.
- Served by an unprivileged, read-only nginx container that proxies `/v1` to
  sandboxd on the same origin; Traefik routes `console.<domain>` to it.
- CI builds the console (`console` job); `make console` / `make console-dev`.

### M3b/M3c — users, policy, egress, approvals, domain
- Console users with roles `admin`, `operator` and `viewer` (migration `0029`
  turns the old single password into user `admin`). `SANDBOXD_BOOTSTRAP_ADMIN`
  creates the first admin. New routes: `GET /v1/auth/me`, `/v1/users` CRUD.
  Cookie-authenticated unsafe requests need the `X-Sandboxd-CSRF` header.
- API keys take `role` (not above the creator's), more scopes, `ttl_seconds`
  (capped by `SANDBOXD_API_KEY_MAX_TTL_SECONDS`) and `sandbox_id`.
- Audit log rows are hash-chained; `GET /v1/audit` and `GET /v1/audit/verify`.
- Instance policy (`GET|PUT /v1/policy`, `/v1/policy/{section}`): allowed
  images, port ranges, resource caps, egress mode and allowlist, keep-alive
  and idle limits, and which actions need approval. Every create, port,
  egress and lifecycle change goes through `policy.Decide`. Denials are 403
  `policy_denied`; disallowed images are now refused with "not allowed by policy".
- Per-sandbox lifecycle: `GET|PATCH /v1/sandboxes/{id}/lifecycle`,
  `POST|DELETE /v1/sandboxes/{id}/keep-alive`. Keep-alive is off unless the
  policy allows it.
- Egress deny-by-default: the compose sandbox network is `internal`, and
  sandboxd runs an HTTP/CONNECT proxy (`SANDBOXD_EGRESS_PROXY_ADDR`) injected
  as `HTTP(S)_PROXY`. `GET|PUT /v1/sandboxes/{id}/network/egress` manages each
  sandbox's allowlist. Metadata, loopback and unlisted private IPs are blocked.
- Approvals: `GET /v1/approvals[/{id}]`, `POST /v1/approvals/{id}/approve|deny`.
  A gated request returns 202 with an `approval_id`; repeat it with
  `X-Sandboxd-Approval` once a console admin approves. Single use, payload-bound.
- Domain manager: `GET|PUT /v1/domain` (applied at the next start),
  `POST /v1/domain/verify`, `GET|POST /v1/domain/certificates`.

### M3a — per-sandbox spec, ports, observability, isolation
- `POST /v1/sandboxes` accepts `image` (instance image or one on
  `SANDBOXD_IMAGE_ALLOWLIST`), `command`, `resources.cpus`/`resources.memory_mb`
  (clamped to the instance maximums) and `network.ports`. The spec is stored
  (migration `0028`), so a recreated container is identical, env included.
- New routes: `GET|PUT|POST /v1/sandboxes/{id}/network/ports`,
  `DELETE /v1/sandboxes/{id}/network/ports/{port}`, `GET /v1/sandboxes/{id}/metrics`,
  `GET /v1/sandboxes/{id}/metrics/stream` (SSE) and `GET /v1/sandboxes/{id}/volumes`.
- Sandboxes always run as `1000:1000` (`SANDBOXD_SANDBOX_USER`); older containers
  are recreated as that user on their next start.
- The API refuses requests from the sandbox network (source guard), and
  docker-compose reaches the control plane over a separate `sandboxd_ctl` network.
- Create and recreate share one container builder (`sandboxspec.Build`).

### M2 — lifecycle API and cleanup
- New `/v1` lifecycle routes: `GET /v1/sandboxes`, `POST /v1/sandboxes/{id}/restart`,
  `POST /v1/sandboxes/{id}/exec`, `GET /v1/sandboxes/{id}/logs` (SSE with `follow=true`).
- `POST /v1/sandboxes` takes `ports`, `env`, `visibility`, `idle_policy`, `external`,
  `from_snapshot` and `workspace.repo_url`; unknown fields are rejected. The
  `project`, `template` and `runtime_preset` fields are gone.
- The v1 sandbox object now carries `image`, `ports`, `visibility`, `idle_policy`,
  `external`, `last_active_at` and `keepalive_until`; `preview` appears only
  when the sandbox exposes ports.
- Migration `0027` drops the app, task, agent, git-credential and model-provider
  tables and their columns.
- The `upgrade` API-key scope is replaced by `health`.
- Removed the old agent console, installers, release workflow and agent docs.

### M1 — strip to a lifecycle-only control plane
- Removed `runtimed`, the coding-agent stack, telemetry, in-place upgrades,
  the desktop app and app templates.
- New CI: unit tests with the race detector and a Docker lifecycle e2e.
