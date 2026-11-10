# Portal image (docs/PORTAL_PLAN.md 6/S0): build the SPA, build the Go
# binary, ship one static binary. The SPA is embedded via
# internal/webfs, so dashboard + API share one origin (BFF, plan 3.1).
FROM node:22-alpine AS web
WORKDIR /src
RUN corepack enable
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm build

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/dist/ ./internal/webfs/dist/
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/portal ./cmd/portal

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/portal /portal
# Files live in the container image; sessions/orgs live in Postgres.
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/portal"]
