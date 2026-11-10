# 4AiCoding — control plane (control-plane/, Go).
.DEFAULT_GOAL := help
.PHONY: help test lint fmt build e2e e2e-auth e2e-fleet e2e-fleet-exec e2e-k3s helm-lint preview console console-dev up down logs clean portal-test portal-lint portal-build portal-console portal-dev portal-up portal-down

CP := control-plane
PORTAL := portal

help: ## Show this help
        @grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
          awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

test: ## Go unit tests (with the race detector)
        cd $(CP) && go test -race -count=1 ./...

lint: ## gofmt + go vet
        cd $(CP) && test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
        cd $(CP) && go vet ./...

fmt: ## Format Go code
        cd $(CP) && gofmt -w .

build: ## Build the sandboxd binary into ./bin
        cd $(CP) && CGO_ENABLED=1 go build -o ../bin/sandboxd ./cmd/sandboxd

e2e: build ## Lifecycle e2e against the local Docker daemon
        docker build -t sandboxd-e2e:local scripts/e2e
        sudo SANDBOXD_IMAGE=sandboxd-e2e:local SANDBOXD_BIN=$(CURDIR)/bin/sandboxd bash scripts/e2e.sh

e2e-auth: build ## Access-control e2e (auth on) against the local Docker daemon
        docker build -t sandboxd-e2e:local scripts/e2e
        sudo SANDBOXD_IMAGE=sandboxd-e2e:local SANDBOXD_BIN=$(CURDIR)/bin/sandboxd bash scripts/e2e-auth.sh

e2e-fleet: build ## Fleet e2e: 2 workers register, drain, offline (M6)
        docker build -t sandboxd-e2e:local scripts/e2e
        sudo SANDBOXD_IMAGE=sandboxd-e2e:local SANDBOXD_BIN=$(CURDIR)/bin/sandboxd bash scripts/e2e-fleet.sh

e2e-fleet-exec: build ## Sandboxes run on an executor worker through the control plane (M7b)
        docker build -t sandboxd-e2e:local scripts/e2e
        sudo SANDBOXD_IMAGE=sandboxd-e2e:local SANDBOXD_BIN=$(CURDIR)/bin/sandboxd bash scripts/e2e-fleet-exec.sh

e2e-k3s: build ## k3s e2e: the Helm chart worker joins the fleet (needs k3s + helm) (M7a)
        docker build -t sandboxd-e2e:local scripts/e2e
        docker build -t sandboxd:e2e control-plane
        docker save sandboxd:e2e | sudo k3s ctr images import -
        sudo env "PATH=$$PATH" SANDBOXD_IMAGE=sandboxd-e2e:local SANDBOXD_BIN=$(CURDIR)/bin/sandboxd WORKER_IMAGE=sandboxd:e2e bash scripts/e2e-k3s.sh

helm-lint: ## Lint the worker + portal Helm charts
        helm lint deploy/helm/sandboxd-worker --set controlPlane.url=http://cp:9000 --set workerToken.value=t
        helm lint deploy/helm/portal --set database.url=postgres://portal:portal@localhost:5432/portal

preview: ## Full compose stack + browser smoke of the console (screenshots in preview-shots/)
        docker build -t sandboxd-e2e:local scripts/e2e
        cd scripts/preview && npm install --no-audit --no-fund
        sudo --preserve-env=PATH SANDBOXD_IMAGE=sandboxd-e2e:local bash scripts/preview.sh

console: ## Typecheck + build the web console (console/dist)
        cd console && pnpm install --frozen-lockfile && pnpm build

console-dev: ## Console dev server, proxying /v1 to 127.0.0.1:9090
        cd console && pnpm install && pnpm dev

portal-test: ## Portal Go tests (PORTAL_TEST_DATABASE_URL adds the migration tests)
        cd $(PORTAL) && go test -race -count=1 ./...

portal-lint: ## Portal gofmt + go vet
        cd $(PORTAL) && test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
        cd $(PORTAL) && go vet ./...

portal-build: ## Build the portal binary into ./bin
        cd $(PORTAL) && CGO_ENABLED=0 go build -o ../bin/portal ./cmd/portal

portal-console: ## Typecheck + build the portal SPA (portal/web)
        cd portal/web && pnpm install --frozen-lockfile && pnpm build

portal-dev: ## Portal SPA dev server, proxying /api to 127.0.0.1:8080
        cd portal/web && pnpm install && pnpm dev

portal-up: ## Bring up postgres + portal (docker-compose.portal.yml)
        docker compose -f docker-compose.portal.yml up -d --build

portal-down: ## Tear the portal stack down
        docker compose -f docker-compose.portal.yml down

up: ## Bring up control plane + edge + console
        docker compose up -d --build

down: ## Tear it down
        docker compose down

logs: ## Tail the control plane
        docker compose logs -f sandboxd

clean: ## Remove local build artifacts
        rm -rf bin console/dist portal/web/dist
