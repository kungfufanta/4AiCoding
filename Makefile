# 4AiCoding — control plane (control-plane/, Go).
.DEFAULT_GOAL := help
.PHONY: help test lint fmt build e2e e2e-auth e2e-fleet preview console console-dev up down logs clean

CP := control-plane

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

preview: ## Full compose stack + browser smoke of the console (screenshots in preview-shots/)
	docker build -t sandboxd-e2e:local scripts/e2e
	cd scripts/preview && npm install --no-audit --no-fund
	sudo --preserve-env=PATH SANDBOXD_IMAGE=sandboxd-e2e:local bash scripts/preview.sh

console: ## Typecheck + build the web console (console/dist)
	cd console && pnpm install --frozen-lockfile && pnpm build

console-dev: ## Console dev server, proxying /v1 to 127.0.0.1:9090
	cd console && pnpm install && pnpm dev

up: ## Bring up control plane + edge + console
	docker compose up -d --build

down: ## Tear it down
	docker compose down

logs: ## Tail the control plane
	docker compose logs -f sandboxd

clean: ## Remove local build artifacts
	rm -rf bin console/dist
