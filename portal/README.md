# Portal

The SaaS layer in front of [sandboxd](../control-plane): organisations,
projects, API keys + agent tokens, the public `/v1` API and the
dashboard. **This is the contract** for what gets built here and in
which order: [`docs/PORTAL_PLAN.md`](../docs/PORTAL_PLAN.md).

## Layout (S0 + S1)

| Path | What |
|---|---|
| `cmd/portal` | The single binary: env config → migrate (owner pool) → serve (app role pool) |
| `internal/config` | `PORTAL_*` env parsing |
| `internal/db` | pgx pools + embedded migration runner + RLS tx helpers (`WithTx` / `WithOrg` / `BootstrapTx`) |
| `internal/tenants` | Tenant repositories: users, orgs, memberships, invites, projects, audit |
| `internal/session` | `web_sessions`: hashed tokens, HttpOnly cookie, double-submit CSRF |
| `internal/oidcauth` | OIDC Authorization Code + PKCE client (works with any provider) |
| `internal/passwordauth` | Local username/password login: bootstrap admin (`password_logins`, bcrypt) |
| `internal/httpapi` | `/healthz`, `/readyz`, the BFF (`/auth/*`, `/api/*`) |
| `internal/webfs` | The embedded SPA (`web/dist`); same origin as the BFF (plan 3.1) |
| `internal/logging` | slog JSON + request-id middleware (mirrors control-plane) |
| `migrations/` | Plain SQL, `NNNN_name.sql`, applied in order |
| `web/` | Vite + React 19 + TypeScript SPA |

## Tenancy & RLS (S1)

Migration `0002_tenants_rls.sql` creates the tenant tables and turns
on Row Level Security. Every request transaction pins two settings
(`app.org_id`, `app.user_id`); the policies make a row visible only to
its own org or to a member of the row's org. Queries may "forget"
`WHERE org_id` — RLS is the last line of defence, and `rls_test.go`
proves a WHERE-less UPDATE/DELETE/SELECT cannot cross orgs.

The serving pool assumes the non-owner role `portal_app` (created by
the migration; `PORTAL_DB_ROLE` overrides, `-` disables) because
superusers and table owners bypass RLS. The default compose stack and
the CI service container satisfy this automatically.

## Run it (dev)

```sh
docker compose -f docker-compose.portal.yml up --build
# portal  → http://localhost:8080  (/healthz, /readyz, SPA)
# postgres → localhost:5433 (portal/portal)

# The local admin login is off unless you give it a password
# (min 12 chars), e.g.:
#   PORTAL_ADMIN_PASSWORD=local-dev-password docker compose -f docker-compose.portal.yml up --build
# then sign in at http://localhost:8080 as admin (see PORTAL_ADMIN_* below).

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
| `PORTAL_PUBLIC_URL` | — | External base URL (cookies/OIDC redirect) |
| `PORTAL_SESSION_TTL` | `12h` | Web-session lifetime |
| `PORTAL_OIDC_ISSUER` / `_CLIENT_ID` / `_CLIENT_SECRET` | — | OIDC login (empty = login disabled, `/auth/login` answers 503) |
| `PORTAL_ADMIN_USERNAME` | `admin` | Local password-login username (bootstrap admin) |
| `PORTAL_ADMIN_PASSWORD` | — (login off) | Local password-login password, min 12 characters. Unset or `-`: the password login is off, and stored password logins plus their sessions are removed at boot. When set, the stored bcrypt hash converges to this value on every boot (that is the rotation path), and a change signs out the account's existing sessions |
| `PORTAL_ADMIN_EMAIL` | `admin@portal.local` | Directory email of the bootstrap admin (gets a personal org like any user) |
| `PORTAL_DB_ROLE` | `portal_app` | Non-owner role for RLS; `-` disables the `SET ROLE` |

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
