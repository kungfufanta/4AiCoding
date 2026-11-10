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
| Policy | One instance policy (`/v1/policy`) checked for every create, port, egress and lifecycle change; requests are clamped or refused |
| Egress | Sandboxes have no route out; the only exit is an HTTP/CONNECT proxy that allows each sandbox's allowlist only and always blocks metadata/loopback |
| Approvals | Risky actions (delete a keep-alive sandbox, change policy, egress outside policy, public preview) can require a console admin's single-use approval |
| Audit | Hash-chained audit log with `GET /v1/audit/verify` |
| Domain | `/v1/domain` stores domain and TLS mode, checks DNS, installs custom wildcard certificates for Traefik |
| Preview | Per-port Traefik routes: `http://s-<id>-<port>.preview.<domain>` |

| Console | Web UI at `http://console.<domain>`: sandboxes, logs, exec, metrics, network, policy, approvals, users, API keys, audit, domain |

Planned next (see PLAN.md): multi-node workers and a Kubernetes runtime.

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

## Repository layout

```
control-plane/      Go control plane (cmd/sandboxd, internal/*, migrations)
console/            Web console (React 19 + TypeScript, Vite; served by nginx)
image/              Sandbox base image (toolchains, no supervisor)
traefik/            Edge router config (preview routes, wake, forward-auth)
scripts/e2e.sh      Lifecycle e2e against a real Docker daemon
docs/               OpenAPI contract and operator notes
PLAN.md             Roadmap and security baseline
```

## Development

```bash
make test   # go test -race ./...
make lint   # gofmt + go vet
make e2e    # build sandboxd + a tiny test image, then run scripts/e2e.sh
make e2e-auth     # same, with auth on: login, roles, keys, keep-alive, approvals, audit
make preview      # full docker compose stack + browser smoke of the console
make console      # typecheck + build the console
make console-dev  # console dev server against a local sandboxd on 127.0.0.1:9090
```

CI (`.github/workflows/ci.yml`) runs the Go unit job, the console build and the Docker integration jobs (lifecycle and access-control e2e) on every push and pull request. The `preview` workflow builds the whole compose stack on each pull request, drives the console in a browser through Traefik, and attaches screenshots of every screen to the run.

## License

MIT. See [LICENSE](LICENSE). Original work © sandboxd contributors.
