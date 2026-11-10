# Changelog

All notable changes to 4AiCoding. The project started from
[tastyeffectco/sandboxd](https://github.com/tastyeffectco/sandboxd) at commit
`92f4ffd`; see that repository for the history before the fork.

## Unreleased

### M8 — kubernetes runtime adapter: sandbox = Pod (k3s B2)
- `SANDBOXD_EXECUTOR_ENGINE=kubernetes` on an executor worker runs every
  sandbox in the cluster as a **Pod** (hardened: non-root uid 1000, read-only
  rootfs, ALL capabilities dropped, no privilege escalation, RuntimeDefault
  seccomp, no service-account token), wrapped in a **Service** (its preview
  ports), a **NetworkPolicy** (egress deny-by-default: only cluster DNS plus
  optional `SANDBOXD_K8S_PROXY_SELECTOR` peers) and — with a preview domain —
  an **Ingress** (one rule per preview port, the same hosts Traefik serves on
  docker). Workspaces stay on the node (hostPath); the one-shot workspace
  seed runs as a Kubernetes Job instead of a docker container.
- New `internal/kube` engine implements the `docker.Engine` seam (PLAN M8)
  on the raw REST API — no client-go: pod lifecycle (Run/Inspect/Start/Stop/
  Remove/Logs/ListByNamePrefix), exec over WebSocket (`v4.channel.k8s.io`,
  hand-rolled client), in-cluster + kubeconfig auth, and RunSpec persistence
  so `Start`/wake recreates a stopped sandbox's Pod. `Stats` returns
  "unsupported" on this engine until a metrics backend is wired.
- Fleet: nodes report their engine (`nodes.engine`, migration `0034`);
  `GET /v1/nodes` + the console Nodes tab show `docker | kubernetes`.
- Helm chart: `executor.engine=kubernetes` renders the least-privilege RBAC
  (pods/log/exec, services, networkpolicies, ingresses, seed Jobs) and drops
  the Docker socket mount.
- CI: the k3s e2e adds an M8 section — a sandbox created on the control
  plane becomes a running Pod with Service/NetworkPolicy/Ingress, exec goes
  through the control plane, delete removes every object. Unit tests run the
  engine against a fake apiserver (pod hardening, lifecycle mapping, exec
  framing, logs).

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
