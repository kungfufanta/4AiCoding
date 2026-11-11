# 4AiCoding

4AiCoding is a **sandbox lifecycle control plane** that you drive entirely through an HTTP API. It creates isolated Docker containers ("sandboxes"), gives each one a persistent workspace and an optional preview URL, stops them when idle, and wakes them on the next request.

It began as a fork of [tastyeffectco/sandboxd](https://github.com/tastyeffectco/sandboxd) (MIT). The coding-agent layer was removed: there is no in-sandbox supervisor, no task API, no model providers, no auth proxy, no desktop app and no agent console. What remains is the control plane. Anything that runs inside a sandbox, including a future AI agent, is treated as **untrusted** and goes through the same API as everyone else.

See [PLAN.md](PLAN.md) for the roadmap and the security baseline.

## What it does today

| Area | Status |
|---|---|
| Lifecycle | Create, list, get, start, stop, restart, exec, logs (SSE follow) and delete over `/v1/sandboxes` |
| Spec | Per-sandbox image (allowlisted), command, env, CPU/memory limits (clamped); stored, so a rebuilt container is identical |
| Network | Preview ports managed over `/v1/sandboxes/{id}/network/ports`; the API refuses requests from the sandbox network |
| Observability | Per-sandbox metrics (one sample or an SSE stream) and mounted volumes |
| Isolation | Non-root (`1000:1000`), read-only rootfs, `--cap-drop=ALL`, `no-new-privileges`, pids/memory limits, tmpfs `/tmp`, only the workspace mounted; optional gVisor (`SANDBOXD_RUNTIME=runsc`) |
| Resources | Idle reaper, memory-pressure reaper, wake-on-request through Traefik, keepalive |
| Workspaces | Persistent bind-mounted workspace per sandbox; snapshots; clone a public git repo at create time |
| Access | Console users with roles (admin, operator, viewer), session login with CSRF protection, API keys with role, scopes, TTL and optional binding to one sandbox |
| Fleet | Worker mode (`sandboxd worker`): workers register with a worker token, heartbeat, and appear in `/v1/nodes` + `/v1/capacity`; drain/undrain per node; every sandbox records its placement |
| Policy | One instance policy (`/v1/policy`) checked for every create, port, egress and lifecycle change; requests are clamped or refused |
| Egress | Sandboxes have no route out; the only exit is an HTTP/CONNECT proxy that allows each sandbox's allowlist only and always blocks metadata/loopback |
| Approvals | Risky actions (delete a keep-alive sandbox, change policy, egress outside policy, public preview) can require a console admin's single-use approval |
| Audit | Hash-chained audit log with `GET /v1/audit/verify` |
| Domain | `/v1/domain` stores domain and TLS mode, checks DNS, installs custom wildcard certificates for Traefik |
| Preview | Per-port Traefik routes: `http://s-<id>-<port>.preview.<domain>` |

| Console | Web UI at `http://console.<domain>`: sandboxes, logs, exec, metrics, network, policy, approvals, nodes, users, API keys, audit, domain |

Planned next (see PLAN.md): a Kubernetes runtime adapter (M8).

## Quickstart (single host)

Requirements: Docker with the Compose plugin.

```bash
cp .env.example .env
# Build the sandbox base image (or point SANDBOXD_IMAGE at your own image).
docker build -f image/Dockerfile -t sandboxd-base:0.3.0 .
docker compose up -d --build
```

The API listens on `127.0.0.1:9090`. Auth is on by default, so set a token in `.env` (`SANDBOXD_API_TOKENS=default=<secret>`) and send it as `Authorization: Bearer <secret>`. Set `SANDBOXD_BOOTSTRAP_ADMIN=admin:<password>` to create the first console user.

Sandboxes start with **no internet access**. To let one reach, say, GitHub, allow it in the instance policy and then on the sandbox:

```bash
curl -s -H "$AUTH" -X PUT $API/v1/policy/egress -d '{"mode":"strict","allow":["github.com","*.npmjs.org"]}'
curl -s -H "$AUTH" -X PUT $API/v1/sandboxes/$ID/network/egress -d '{"allow":["github.com"]}'
```

Tools inside the sandbox pick up the proxy from `HTTP_PROXY`/`HTTPS_PROXY`.

```bash
API=http://127.0.0.1:9090
AUTH="Authorization: Bearer $TOKEN"

# create
curl -s -H "$AUTH" -X POST $API/v1/sandboxes -d '{"ports":[3000]}'
# list / get
curl -s -H "$AUTH" $API/v1/sandboxes
curl -s -H "$AUTH" $API/v1/sandboxes/$ID
# exec
curl -s -H "$AUTH" -X POST $API/v1/sandboxes/$ID/exec -d '{"cmd":["uname","-a"]}'
# logs (add &follow=true for a Server-Sent Events stream)
curl -s -H "$AUTH" "$API/v1/sandboxes/$ID/logs?tail=100"
# stop / start / restart
curl -s -H "$AUTH" -X POST $API/v1/sandboxes/$ID/stop
curl -s -H "$AUTH" -X POST $API/v1/sandboxes/$ID/start
curl -s -H "$AUTH" -X POST $API/v1/sandboxes/$ID/restart
# destroy (container + workspace + row)
curl -s -H "$AUTH" -X DELETE $API/v1/sandboxes/$ID
```

### Web console

`docker compose up` also starts the console. Open `http://console.localhost` (add `:$HTTP_PORT` if you changed it). The first visit asks you to create the admin account, unless `SANDBOXD_BOOTSTRAP_ADMIN` already did. Each screen shows the equivalent `curl` command. Viewers can only look, operators can manage sandboxes, and admins also manage policy, users, keys, approvals, the audit log and the domain.

The full contract is [docs/openapi.yaml](docs/openapi.yaml). The older `/sandbox` routes still work but will be deprecated.

### Worker fleet (M6)

The control plane is itself a node (`local`). Remote machines join the fleet with the **same binary** in worker mode:

```bash
# on the control plane host: choose a shared secret
echo 'SANDBOXD_WORKER_TOKEN=change-me' >> .env && docker compose up -d

# on each worker machine:
sandboxd worker \
  --control-plane-url http://<control-plane-host>:9090 \
  --worker-token change-me \
  --node-id worker-1 \
  --address http://worker-1.example:9000 \
  --capacity-cpus 8 --capacity-memory-mb 16384
```

Workers register, heartbeat every 10 s and drop to `offline` in the fleet view after ~90 s of silence. The operator API:

```bash
curl -s -H "$AUTH" $API/v1/nodes            # membership: role, status, drain state, capacity, usage
curl -s -H "$AUTH" $API/v1/capacity         # fleet totals + per-node breakdown
curl -s -H "$AUTH" $API/v1/nodes/$ID/capacity
curl -s -H "$AUTH" -X POST $API/v1/nodes/$ID/drain    # admin: refuse NEW sandboxes there
curl -s -H "$AUTH" -X POST $API/v1/nodes/$ID/undrain  # admin: back to the pool
```

Every sandbox response carries its `node` (its placement). Draining the `local` node blocks sandbox creation on the control plane itself; draining a worker is preparation for moving work onto it — sandboxes run on workers in executor mode (below). The console shows the fleet in **Admin → Nodes**, refreshed live.

Every node also reports its host's real CPU, memory, disk and network with its heartbeat (from the worker image itself, so it works on a VPS, under Docker or in k3s). The console's **Nodes** page shows it with an hour of history, and admins can propose a new advertised capacity there: that opens a pull request editing the node's Helm values file, which the deploy workflow applies once merged. See [docs/NODE_RESOURCES.md](docs/NODE_RESOURCES.md).

### Workers on Kubernetes / k3s (M7a)

`deploy/helm/sandboxd-worker` runs `sandboxd worker` as a DaemonSet: every Kubernetes node joins the fleet under its node name, so the id survives pod restarts. Build the image from `control-plane/` (it is the control plane image; the chart starts it with `worker`), make it available to the cluster, then:

```bash
docker build -t sandboxd:0.1.0 control-plane
docker save sandboxd:0.1.0 | sudo k3s ctr images import -   # or push to your registry

helm install sandboxd-worker deploy/helm/sandboxd-worker \
  --namespace sandboxd --create-namespace \
  --set controlPlane.url=http://<control-plane>:9000 \
  --set workerToken.value="$SANDBOXD_WORKER_TOKEN"   # or workerToken.existingSecret
```

By default the pod runs as a non-root user with a read-only root filesystem, no capabilities and no service account token, and mounts nothing from the host: it only registers, heartbeats and honours drain. `kind=Deployment` with a fixed `nodeId` runs a single worker instead; see `values.yaml` for capacity, scheduling and image options.

### Running sandboxes on workers (M7b)

A worker in **executor mode** is a full sandboxd on its own machine that runs sandboxes for the control plane. Start any sandboxd with `SANDBOXD_CONTROL_PLANE_URL`, `SANDBOXD_WORKER_TOKEN`, `SANDBOXD_NODE_ID` and `SANDBOXD_NODE_ADDRESS` (see `.env.example`), or install the chart with `--set executor.enabled=true --set executor.sandboxImage=<image>`: each pod then drives its node's Docker (host network, Docker socket, a host data dir, root with only `CHOWN`/`DAC_OVERRIDE`/`FOWNER`), so keep it to nodes dedicated to sandboxes.

- The executor registers with its own API token. The control plane stores it encrypted and is the only caller the executor's API accepts. A node id stays bound to that token; `DELETE /v1/nodes/{id}` (admin) resets a node that lost its state.
- `POST /v1/sandboxes` on the control plane places the sandbox on the schedulable node with the most free memory (the control plane itself wins ties; drained and offline nodes are skipped). The response's `node` names it.
- Every `/v1/sandboxes/{id}/...` call (exec, logs, metrics, ports, lifecycle, delete, ...) is routed to that node, and `GET /v1/sandboxes` merges all nodes (`unreachable_nodes` lists any that did not answer). Consoles, CLIs and API keys keep talking to the control plane only.
- Approvals stay on the control plane: a gated request (public preview, deleting a keep-alive sandbox, egress outside the policy) is parked and approved there, then forwarded. The control plane's policy is pushed to every executor on each heartbeat.
- Limits today: snapshots and `from_snapshot` creates stay on the control plane, and each executor serves previews through its own edge (set its `PREVIEW_DOMAIN`). Private preview auth across nodes is not wired yet.

## Repository layout

```
control-plane/      Go control plane (cmd/sandboxd, internal/*, migrations)
console/            Web console (React 19 + TypeScript, Vite; served by nginx)
image/              Sandbox base image (toolchains, no supervisor)
traefik/            Edge router config (preview routes, wake, forward-auth)
scripts/e2e.sh      Lifecycle e2e against a real Docker daemon
deploy/helm/        Helm chart for k3s / Kubernetes workers
docs/               OpenAPI contract and operator notes
PLAN.md             Roadmap and security baseline
```

## Development

```bash
make test   # go test -race ./...
make lint   # gofmt + go vet
make e2e    # build sandboxd + a tiny test image, then run scripts/e2e.sh
make e2e-auth     # same, with auth on: login, roles, keys, keep-alive, approvals, audit
make e2e-fleet    # same, plus 2 workers registering, draining and going offline
make e2e-fleet-exec  # control plane + executor worker: sandboxes run on the worker
make e2e-k3s      # Helm chart worker on a local k3s joins the fleet and runs a sandbox (needs k3s + helm)
make preview      # full docker compose stack + browser smoke of the console
make console      # typecheck + build the console
make console-dev  # console dev server against a local sandboxd on 127.0.0.1:9090
```

CI (`.github/workflows/ci.yml`) runs the Go unit job, the console build and the Docker integration jobs (lifecycle, access-control and fleet e2e) and a k3s job that installs the worker chart on a real k3s cluster on every push and pull request. The `preview` workflow builds the whole compose stack on each pull request, drives the console in a browser through Traefik, and attaches screenshots of every screen to the run.

## License

MIT. See [LICENSE](LICENSE). Original work © sandboxd contributors.
