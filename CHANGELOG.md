# Changelog

All notable changes to 4AiCoding. The project started from
[tastyeffectco/sandboxd](https://github.com/tastyeffectco/sandboxd) at commit
`92f4ffd`; see that repository for the history before the fork.

## Unreleased

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
