# Portal

The SaaS layer in front of [sandboxd](../control-plane): organisations,
projects, API keys + agent tokens, the public `/v1` API and the
dashboard. **This is the contract** for what gets built here and in
which order: [`docs/PORTAL_PLAN.md`](../docs/PORTAL_PLAN.md).

## Layout (S0)

| Path | What |
|---|---|
| `cmd/portal` | The single binary: env config → Postgres pool → migrations → HTTP |
| `internal/config` | `PORTAL_*` env parsing |
| `internal/db` | pgx pool + embedded migration runner (advisory-locked, idempotent) |
| `internal/httpapi` | `/healthz`, `/readyz` (Postgres ping); S1 mounts the BFF here |
| `internal/webfs` | The embedded SPA (`web/dist`); same origin as the BFF (plan 3.1) |
| `internal/logging` | slog JSON + request-id middleware (mirrors control-plane) |
| `migrations/` | Plain SQL, `NNNN_name.sql`, applied in order |
| `web/` | Vite + React 19 + TypeScript SPA |

## Run it (dev)

```sh
docker compose -f docker-compose.portal.yml up --build
# portal  → http://localhost:8080  (/healthz, /readyz, SPA)
# postgres → localhost:5433 (portal/portal)

# with the identity provider (S1 login):
docker compose -f docker-compose.portal.yml --profile idp up
# zitadel → http://localhost:8081
```

Or without Docker: point `PORTAL_DATABASE_URL` at any Postgres 16+ and
`go run ./cmd/portal` (from `portal/`).

## Configuration

| Env | Default | Meaning |
|---|---|---|
| `PORTAL_DATABASE_URL` | — (required) | Postgres DSN of the portal's own DB |
| `PORTAL_ADDR` | `:8080` | HTTP listen address |
| `PORTAL_DEBUG` | off | Debug-level JSON logs |
| `PORTAL_PUBLIC_URL` | — | External base URL (S1 cookies/OIDC) |
| `PORTAL_SESSION_TTL` | `12h` | Web-session lifetime (S1) |
| `PORTAL_OIDC_ISSUER` / `_CLIENT_ID` / `_CLIENT_SECRET` | — | OIDC login (S1) |

## Tests

```sh
cd portal
go test -race -count=1 ./...
# migration tests need a real Postgres; CI runs one as a service:
PORTAL_TEST_DATABASE_URL=postgres://portal:portal@127.0.0.1:5432/portal_test go test ./internal/db/...
```

SPA: `cd web && pnpm install && pnpm build`.

## Deploy

Helm chart `deploy/helm/portal` — give it a DSN:

```sh
helm template portal deploy/helm/portal \
  --set database.url=postgres://portal:portal@portal-postgres:5432/portal
```

The binary migrates its own database at boot; no init job needed.
`/readyz` gates traffic and checks Postgres.
