# Changelog

All notable changes to 4AiCoding. The project started from
[tastyeffectco/sandboxd](https://github.com/tastyeffectco/sandboxd) at commit
`92f4ffd`; see that repository for the history before the fork.

## Unreleased

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
