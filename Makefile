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
.PHONY: up
up: ## Bring up the full local stack (detached).
	@$(COMPOSE) up -d
	@echo ""
	@echo "Stack is starting. Run 'make status' to see health, 'make logs' to tail."

.PHONY: up-fg
up-fg: ## Bring up the full local stack in the foreground (Ctrl-C to stop).
	$(COMPOSE) up

.PHONY: down
down: ## Stop the stack but keep volumes.
	@$(COMPOSE) down

.PHONY: reset
reset: ## Stop the stack AND drop all volumes (destructive).
	@$(COMPOSE) down -v --remove-orphans
	@echo "Volumes dropped. Run 'make up' to start fresh."

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
	@cat <<-'EOF'
	Kong proxy        http://localhost:8000
	Kong admin        http://localhost:8001
	Keycloak          http://localhost:8080  (admin/admin)
	Vault             http://localhost:8200  (token: dev-root-token)
	RabbitMQ          http://localhost:15672 (guva/guva)
	MinIO console     http://localhost:9001  (guva/guvaguva)
	Apicurio          http://localhost:8081
	Prometheus        http://localhost:9090
	Grafana           http://localhost:3000  (admin/admin)
	Jaeger            http://localhost:16686
	Postgres          postgres://guva:guva@localhost:5432/guva
	Redis             redis://localhost:6379
	Kafka             localhost:9094
	Reference svc     http://localhost:7070  (run: make run-reference)
	EOF

.PHONY: clean
clean: ## Remove build artifacts.
	@rm -rf bin dist coverage.txt coverage.html
