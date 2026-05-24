# =============================================================================
# GUVA Backend — Makefile
# =============================================================================
# Common workflows for the local development environment. Run `make help` for
# a complete list. CI executes the same targets to keep developer-local and
# CI behaviour aligned.
# =============================================================================

SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

# ---- Configuration ---------------------------------------------------------
PROJECT       ?= guva
COMPOSE       ?= docker compose
GO            ?= go
SERVICES_DIR  := services
SERVICES      := $(notdir $(wildcard $(SERVICES_DIR)/*))
ENV_FILE      := .env

# ---- Help -----------------------------------------------------------------
.PHONY: help
help: ## Show this help.
	@printf "GUVA Backend — local development\n\n"
	@printf "Usage: make \033[36m<target>\033[0m\n\n"
	@awk 'BEGIN {FS = ":.*?## "} \
	     /^[a-zA-Z0-9_.-]+:.*?## / { \
	       printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 \
	     }' $(MAKEFILE_LIST)
	@printf "\nDiscovered services: $(SERVICES)\n"

# ---- Bootstrap ------------------------------------------------------------
.PHONY: bootstrap
bootstrap: $(ENV_FILE) ## One-time setup: copy env, pull images, validate Docker.
	@command -v docker >/dev/null || (echo "docker not found"; exit 1)
	@$(COMPOSE) version >/dev/null || (echo "docker compose plugin not found"; exit 1)
	@$(COMPOSE) pull --quiet
	@bash tools/scripts/bootstrap.sh

$(ENV_FILE):
	@cp .env.example $(ENV_FILE)
	@echo "Created $(ENV_FILE) from .env.example"

# ---- Compose stack lifecycle ----------------------------------------------
#
# `make up` runs in two phases so APISIX's declarative routes file can be
# rendered from Keycloak's realm public key (which only exists once
# Keycloak is up):
#   1. Bring up every service except APISIX; wait for Keycloak to be
#      healthy.
#   2. Render deploy/compose/apisix/apisix.yaml from apisix.yaml.tmpl,
#      then start APISIX.
# The render step is idempotent — it skips re-rendering when the output
# already exists. Use `make refresh-keys` to force after a Keycloak
# rotation; APISIX hot-reloads the file automatically.

GATEWAY_RENDERED := deploy/compose/apisix/apisix.yaml
BASE_SERVICES := postgres redis kafka apicurio rabbitmq keycloak vault minio jaeger otel-collector prometheus grafana

.PHONY: up
up: ## Bring up the full local stack (detached).
	@$(COMPOSE) up -d --wait $(BASE_SERVICES)
	@bash tools/scripts/render-gateway-config.sh
	@$(COMPOSE) up -d apisix
	@echo ""
	@echo "Stack is up. Run 'make status' to see health, 'make logs' to tail, 'make urls' for endpoints."

.PHONY: refresh-keys
refresh-keys: ## Re-render APISIX config from Keycloak's current realm key.
	@bash tools/scripts/render-gateway-config.sh --force
	@echo "APISIX standalone hot-reloads apisix.yaml within ~1s — no restart needed."

.PHONY: up-fg
up-fg: ## Bring up the full local stack in the foreground (Ctrl-C to stop).
	@bash tools/scripts/render-gateway-config.sh || true
	$(COMPOSE) up

.PHONY: down
down: ## Stop the stack but keep volumes.
	@$(COMPOSE) down

.PHONY: reset
reset: ## Stop the stack AND drop all volumes (destructive).
	@$(COMPOSE) down -v --remove-orphans
	@rm -f $(GATEWAY_RENDERED)
	@echo "Volumes dropped and rendered gateway config removed. Run 'make up' to start fresh."

.PHONY: restart
restart: down up ## Down then up.

.PHONY: status
status: ## Show service health.
	@$(COMPOSE) ps

.PHONY: logs
logs: ## Tail logs for all services (Ctrl-C to exit).
	@$(COMPOSE) logs -f --tail=200

.PHONY: logs-%
logs-%: ## Tail logs for a single service, e.g. `make logs-kafka`.
	@$(COMPOSE) logs -f --tail=200 $*

# ---- Database -------------------------------------------------------------
.PHONY: token
token: ## Print an OAuth client-credentials access token for guva-reference.
	@curl -fsS -X POST http://localhost:8080/realms/guva/protocol/openid-connect/token \
	  -H 'Content-Type: application/x-www-form-urlencoded' \
	  -d 'grant_type=client_credentials' \
	  -d 'client_id=guva-reference' \
	  -d 'client_secret=reference-dev-secret' \
	  | python3 -c "import sys,json;print(json.load(sys.stdin)['access_token'])"

.PHONY: ping
ping: ## Call /v1/reference/ping through Kong with a fresh token.
	@TOKEN=$$($(MAKE) -s token) && \
	 curl -fsS -H "Authorization: Bearer $$TOKEN" \
	   http://localhost:8000/v1/reference/ping \
	   | python3 -m json.tool

.PHONY: psql
psql: ## Open psql on the postgres container.
	@$(COMPOSE) exec -it postgres psql -U $${POSTGRES_USER:-guva} -d $${POSTGRES_DB:-guva}

.PHONY: redis-cli
redis-cli: ## Open redis-cli on the redis container.
	@$(COMPOSE) exec -it redis redis-cli

.PHONY: migrate
migrate: ## Run migrations for all services (no-op until services own migrations).
	@for s in $(SERVICES); do \
	  if [ -d $(SERVICES_DIR)/$$s/migrations ]; then \
	    echo "==> migrate $$s"; \
	    bash tools/scripts/db-migrate.sh $$s up; \
	  fi; \
	done

# ---- Go: lint / format / test ---------------------------------------------
.PHONY: fmt
fmt: ## Format all Go code (gofumpt + goimports).
	@command -v gofumpt >/dev/null || (echo "gofumpt not installed; run: go install mvdan.cc/gofumpt@latest"; exit 1)
	@command -v goimports >/dev/null || (echo "goimports not installed; run: go install golang.org/x/tools/cmd/goimports@latest"; exit 1)
	@gofumpt -w $(SERVICES_DIR) pkg
	@goimports -w -local github.com/guva-ug $(SERVICES_DIR) pkg

.PHONY: lint
lint: ## Run golangci-lint over the workspace.
	@command -v golangci-lint >/dev/null || (echo "golangci-lint not installed; see https://golangci-lint.run/usage/install/"; exit 1)
	@golangci-lint run ./...

.PHONY: test
test: ## Run unit tests for every service.
	@$(GO) test -race -count=1 -coverprofile=coverage.txt ./...

.PHONY: tidy
tidy: ## Run `go mod tidy` for every service.
	@for s in $(SERVICES); do \
	  echo "==> tidy $$s"; \
	  (cd $(SERVICES_DIR)/$$s && $(GO) mod tidy); \
	done

.PHONY: build
build: ## Build all service binaries into ./bin.
	@mkdir -p bin
	@for s in $(SERVICES); do \
	  echo "==> build $$s"; \
	  (cd $(SERVICES_DIR)/$$s && $(GO) build -o ../../bin/$$s ./cmd/server); \
	done

# ---- Run a service against the local stack --------------------------------
.PHONY: run-%
run-%: ## Run a service in the foreground, e.g. `make run-reference`.
	@cd $(SERVICES_DIR)/$* && $(GO) run ./cmd/server

# ---- Convenience ----------------------------------------------------------
.PHONY: urls
urls: ## Print local service URLs.
	@printf '%s\n' \
	  "APISIX proxy      http://localhost:8000  (public gateway)" \
	  "APISIX metrics    http://localhost:9091/apisix/prometheus/metrics" \
	  "Keycloak          http://localhost:8080  (admin/admin)" \
	  "Vault             http://localhost:8200  (token: dev-root-token)" \
	  "RabbitMQ          http://localhost:15672 (guva/guva)" \
	  "MinIO console     http://localhost:9001  (guva/guvaguva)" \
	  "Apicurio          http://localhost:8081" \
	  "Prometheus        http://localhost:9090" \
	  "Grafana           http://localhost:3000  (admin/admin)" \
	  "Jaeger            http://localhost:16686" \
	  "Postgres          postgres://guva:guva@localhost:5432/guva" \
	  "Redis             redis://localhost:6379" \
	  "Kafka             localhost:9094" \
	  "Reference svc     http://localhost:7070  (run: make run-reference)"

.PHONY: clean
clean: ## Remove build artifacts.
	@rm -rf bin dist coverage.txt coverage.html
