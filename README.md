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
| Access | API keys and console sessions (bcrypt), audit log |
| Preview | Per-port Traefik routes: `http://s-<id>-<port>.preview.<domain>` |

Planned next (see PLAN.md): policy engine and egress policy, session login, domain manager, approvals, a new web console, and multi-node workers.

## Quickstart (single host)

Requirements: Docker with the Compose plugin.

```bash
cp .env.example .env
# Build the sandbox base image (or point SANDBOXD_IMAGE at your own image).
docker build -f image/Dockerfile -t sandboxd-base:0.3.0 .
docker compose up -d --build
```

The API listens on `127.0.0.1:9090`. Auth is on by default, so set a token in `.env` (`SANDBOXD_API_TOKENS=default=<secret>`) and send it as `Authorization: Bearer <secret>`.

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

The full contract is [docs/openapi.yaml](docs/openapi.yaml). The older `/sandbox` routes still work but will be deprecated.

## Repository layout

```
control-plane/      Go control plane (cmd/sandboxd, internal/*, migrations)
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
```

CI (`.github/workflows/ci.yml`) runs the unit job and the Docker integration job on every push and pull request.

## License

MIT. See [LICENSE](LICENSE). Original work © sandboxd contributors.
