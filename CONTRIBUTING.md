# Contributing to 4AiCoding

4AiCoding aims to stay a **small, well-tested sandbox lifecycle control plane**.
Work follows the milestones in [PLAN.md](PLAN.md); each milestone lands as a
pull request with green CI.

## Layout

```
control-plane/   Go control plane (cmd/sandboxd, internal/*, migrations/)
image/           sandbox base image
traefik/         edge router config
scripts/e2e.sh   lifecycle e2e against a real Docker daemon
docs/            OpenAPI contract and operator notes
```

## Before you open a PR

- `make lint` and `make test` pass (Go 1.26, `go test -race`).
- `make e2e` passes if you changed lifecycle behaviour (needs a local Docker daemon).
- API changes update [docs/openapi.yaml](docs/openapi.yaml); the contract test
  fails when routes and the spec drift apart.
- Schema changes add a new numbered file in `control-plane/migrations/`; never
  edit an applied migration.
- Security-relevant changes reference the matching row of PLAN.md §8.
- Add a line to [CHANGELOG.md](CHANGELOG.md) under **Unreleased**.
