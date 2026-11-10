# sandboxd — control plane

The Go control plane for [4AiCoding](../README.md). A single binary that drives
the Docker daemon (via the `docker` CLI), stores state in SQLite, runs the idle
and pressure reapers, and serves the HTTP API and the wake path.

See [`../PLAN.md`](../PLAN.md) for the roadmap and security baseline.

## Build & test

```bash
go build ./...
go test ./...
go vet ./...
```

`sandboxd` uses cgo (mattn/go-sqlite3); the container build (`Dockerfile`) sets
`CGO_ENABLED=1`.

## Packages

| Package | Responsibility |
|---|---|
| `cmd/sandboxd` | daemon entrypoint, env wiring, background goroutines |
| `internal/docker` | thin typed wrapper over the `docker` CLI |
| `internal/loopback` | per-sandbox workspace storage (directory-backed) |
| `internal/traefik` | preview-route label generation |
| `internal/reaper` | idle (stop-on-idle) + host-memory pressure reapers |
| `internal/wake` | wake-on-request handler + warming page |
| `internal/reconcile` | boot-time convergence of Docker → SQLite |
| `internal/store` | SQLite access + numbered migrations |
| `internal/api` | HTTP handlers (`/sandbox*`, `/v1/*`, wake, forward-auth) |
| `internal/auth` | service-token, API-key and preview-token auth |
| `internal/console` | bcrypt passwords + session/API-key token hashing |
| `internal/snapshot` | workspace snapshot capture/restore |
| `internal/gitimport` | host-side clone of a public repo into a workspace |
| `internal/detect`, `manifest`, `recipes`, `preset` | framework detection data |
| `internal/audit`, `internal/events` | audit log + activity timeline |

## Configuration

All runtime configuration is via environment variables, set by the compose file
from `../.env`. The ones the OSS build adds or changes:

| Variable | Default | Purpose |
|---|---|---|
| `PREVIEW_DOMAIN` | `localhost` | domain preview URLs hang off |
| `PREVIEW_ENTRYPOINT` | `web` | Traefik entrypoint on preview routers |
| `PREVIEW_TLS` | `false` | emit `tls=true` on preview routers |
| `SANDBOXD_NETWORK` | `sandboxd_net` | docker network sandboxes join |
| `SANDBOXD_USERNS` | `host` | `--userns` for sandboxes + the seed container |
| `SANDBOXD_DATA_DIR` | `/var/lib/sandboxd` | workspaces + SQLite + logs |
| `SANDBOXD_SET_MEMORY_HIGH` | `false` | write cgroup `memory.high` (needs host cgroup access) |
| `SANDBOXD_IMAGE` | `sandboxd-base:0.3.0` | per-sandbox base image |
| `SANDBOXD_API_AUTH_DISABLED` | `false` | `true` opens the whole API (local experiments only) |
| `SANDBOXD_API_TOKENS` | — | `name=secret` pairs (comma-separated) for bearer auth |
| `SANDBOXD_IDLE_THRESHOLD_SECONDS` | `2100` | idle window before `docker stop` |

## API sketch

The public contract is [`../docs/openapi.yaml`](../docs/openapi.yaml).

```
GET    /v1/sandboxes                 list (?status=, ?external_user_id=, ?external_project_id=)
POST   /v1/sandboxes                 create
GET    /v1/sandboxes/{id}            get
POST   /v1/sandboxes/{id}/start      start (wake)
POST   /v1/sandboxes/{id}/stop       stop
POST   /v1/sandboxes/{id}/restart    stop + start
POST   /v1/sandboxes/{id}/exec       one-shot command
GET    /v1/sandboxes/{id}/logs       container logs (?tail=, ?follow=true for SSE)
DELETE /v1/sandboxes/{id}            destroy container + workspace + row
GET    /healthz  GET /readyz         liveness / readiness
GET    /version                      build version + commit (JSON, no auth)
```

The legacy `/sandbox*` routes remain for now and will be deprecated.
