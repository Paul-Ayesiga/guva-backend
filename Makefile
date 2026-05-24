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
# `make up` brings up every service except APISIX, waits for Keycloak to
# be healthy, then starts APISIX. APISIX validates bearer tokens via OIDC
# discovery against Keycloak (no pinned keys, no rendered config).

BASE_SERVICES := postgres redis kafka apicurio rabbitmq keycloak vault minio jaeger otel-collector prometheus grafana

.PHONY: up
up: ## Bring up the full local stack (detached).
	@$(COMPOSE) up -d --wait $(BASE_SERVICES)
	@bash tools/scripts/seed-vault.sh
	@$(COMPOSE) up -d apisix
	@echo ""
	@echo "Stack is up. Run 'make status' to see health, 'make logs' to tail, 'make urls' for endpoints."

.PHONY: seed-vault
seed-vault: ## Re-seed Vault with the dev secrets every service needs at startup.
	@bash tools/scripts/seed-vault.sh

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
KEYCLOAK_URL ?= https://auth.guva.localhost
CADDY_ROOT_CA = .cache/caddy-root.crt

.PHONY: token
token: ## Print an OAuth client-credentials access token for guva-reference.
	@response=$$(curl -fsS -X POST $(KEYCLOAK_URL)/realms/guva/protocol/openid-connect/token \
	  -H 'Content-Type: application/x-www-form-urlencoded' \
	  -d 'grant_type=client_credentials' \
	  -d 'client_id=guva-reference' \
	  -d 'client_secret=reference-dev-secret' 2>&1) || { \
	    echo "$$response" | grep -q "certificate" && { \
	      echo "TLS verification failed. Run 'make trust-ca' once to install Caddy's local root CA." >&2; \
	      exit 1; \
	    }; \
	    echo "$$response" >&2; exit 1; \
	  }; \
	echo "$$response" | python3 -c "import sys,json;print(json.load(sys.stdin)['access_token'])"

.PHONY: trust-ca
trust-ca: ## Install Caddy's local root CA into your system trust store (one-time).
	@mkdir -p .cache
	@docker compose exec -T caddy cat /data/caddy/pki/authorities/local/root.crt > $(CADDY_ROOT_CA) 2>/dev/null \
	  || { echo "Caddy isn't running yet — bring the stack up with 'make up' first."; exit 1; }
	@echo "==> Extracted Caddy root CA to $(CADDY_ROOT_CA)"
	@uname=$$(uname -s); \
	case $$uname in \
	  Darwin) \
	    echo "==> Installing into macOS System keychain (will prompt for sudo)"; \
	    sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain $(CADDY_ROOT_CA); \
	    echo "==> Done. Try: curl https://auth.guva.localhost/realms/guva | head"; ;; \
	  Linux) \
	    echo "==> Installing into /usr/local/share/ca-certificates/ (will prompt for sudo)"; \
	    sudo cp $(CADDY_ROOT_CA) /usr/local/share/ca-certificates/caddy-guva-local-root.crt; \
	    sudo update-ca-certificates; \
	    echo "==> Done. Try: curl https://auth.guva.localhost/realms/guva | head"; ;; \
	  *) \
	    echo "Unknown OS '$$uname'. Manually add $(CADDY_ROOT_CA) to your system trust store."; \
	    exit 1; ;; \
	esac

.PHONY: untrust-ca
untrust-ca: ## Remove Caddy's local root CA from your system trust store.
	@uname=$$(uname -s); \
	case $$uname in \
	  Darwin) \
	    echo "==> Removing from macOS System keychain (will prompt for sudo)"; \
	    sudo security delete-certificate -c "Caddy Local Authority - 2025 ECC Root" /Library/Keychains/System.keychain 2>/dev/null \
	      || echo "    (no matching cert found — already removed?)" ;; \
	  Linux) \
	    sudo rm -f /usr/local/share/ca-certificates/caddy-guva-local-root.crt; \
	    sudo update-ca-certificates --fresh; ;; \
	  *) \
	    echo "Unknown OS '$$uname'. Manually remove $(CADDY_ROOT_CA) from your trust store."; ;; \
	esac

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
migrate: ## Run migrations for every service that has actual .sql files.
	@for s in $(SERVICES); do \
	  if ls $(SERVICES_DIR)/$$s/migrations/*.sql >/dev/null 2>&1; then \
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
	  "Keycloak (TLS)    https://auth.guva.localhost  (admin/admin) — run 'make trust-ca' once" \
	  "Keycloak (raw)    http://localhost:8080  (bypasses Caddy; debugging only)" \
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
	  "Reference svc     http://localhost:7070  (run: make run-reference)" \
	  "Identity svc      http://localhost:7071  (run: make run-identity; through gateway: /v1/identity/*)"

.PHONY: clean
clean: ## Remove build artifacts.
	@rm -rf bin dist coverage.txt coverage.html
