.PHONY: test-e2e test-e2e-auth gosec lint-backend lint-frontend lint-docs check-workflows check-env-docs check-agents test-backend-unit dev dev-backend dev-frontend mysql mysql-start mysql-stop mysql-logs mysql-shell build clean test install docs fmt lint

# MySQL service management
mysql-start:
	@echo "Starting MySQL container..."
	GO_ENV=development docker compose up -d db
	@echo "Waiting for MySQL to be ready..."
	@until docker compose exec db mysqladmin ping -h localhost -u$${MYSQL_USER:-appuser} -p$${MYSQL_PASSWORD:-apppass} --silent; do \
		echo "Waiting for MySQL..."; \
		sleep 2; \
	done
	@echo "MySQL is ready!"

mysql-stop:
	@echo "Stopping MySQL container..."
	docker compose stop db

mysql-logs:
	docker compose logs -f db

mysql-shell:
	docker compose exec db mysql -u$${MYSQL_USER:-appuser} -p$${MYSQL_PASSWORD:-apppass} $${MYSQL_DATABASE:-app}

# Host ports: the web UI on FRONTEND_PORT and the API on BACKEND_PORT (for
# direct calls: the browser reaches the API through the proxy of the web UI).
# APP_DEBUG drives the Gin mode (debug in
# development, release in production); GIN_MODE, when set, wins.
FRONTEND_PORT_DEV ?= 3000
BACKEND_PORT_DEV ?= 8081
FRONTEND_PORT_PROD ?= 80
BACKEND_PORT_PROD ?= 8080
DEV_ENV = GO_ENV=development NODE_ENV=development FRONTEND_PORT=$(FRONTEND_PORT_DEV) FRONTEND_CONTAINER_PORT=3000 BACKEND_PORT=$(BACKEND_PORT_DEV) APP_DEBUG=true ENABLE_SWAGGER=true
PROD_ENV = GO_ENV=production NODE_ENV=production FRONTEND_PORT=$(FRONTEND_PORT_PROD) FRONTEND_CONTAINER_PORT=8080 BACKEND_PORT=$(BACKEND_PORT_PROD) APP_DEBUG=false ENABLE_SWAGGER=false

# Development mode for both services
dev:
	$(DEV_ENV) docker compose up --build

# Development mode for backend only
dev-backend:
	$(DEV_ENV) docker compose up --build backend

# Development mode for frontend only (also starts the backend it depends on)
dev-frontend:
	$(DEV_ENV) docker compose up --build frontend

# Production mode for both services
prod:
	$(PROD_ENV) docker compose up --build

# Production mode for backend only
prod-backend:
	$(PROD_ENV) docker compose up --build backend

# Production mode for frontend only (also starts the backend it depends on)
prod-frontend:
	$(PROD_ENV) docker compose up --build frontend

# Stop and remove all containers, networks, and volumes
clean:
	docker compose down -v

# Remove all unused Docker resources
prune:
	docker system prune -f

# Run all tests
test: test-backend test-frontend

# Run backend unit tests only (no external dependencies needed)
test-backend:
	cd backend && go test ./... -v

# Backend unit tests with the race detector, as CI runs them (no database)
test-backend-unit:
	cd backend && go test -race -short ./...

# Start infrastructure for integration tests
integration-infra-start:
	@echo "Starting integration test infrastructure..."
	GO_ENV=test docker compose up -d db
	@echo "Waiting for MySQL to be ready..."
	@until docker compose exec db mysqladmin ping -h localhost -u$${MYSQL_USER:-appuser} -p$${MYSQL_PASSWORD:-apppass} --silent 2>/dev/null; do \
		echo "Waiting for MySQL..."; \
		sleep 2; \
	done
	@echo "Integration infrastructure is ready!"

# Stop infrastructure for integration tests
integration-infra-stop:
	@echo "Stopping integration test infrastructure..."
	GO_ENV=test docker compose stop db

# Run backend integration tests (starts/stops infra automatically)
test-backend-integration: integration-infra-start
	cd backend && go test -tags integration -p 1 ./... -v
	$(MAKE) integration-infra-stop

# Run all backend tests (unit + integration)
test-backend-all: integration-infra-start
	cd backend && go test -tags integration -p 1 ./... -v
	$(MAKE) integration-infra-stop

test-frontend:
	cd frontend && npm test

# Frontend e2e tests (Playwright). scripts/e2e.sh starts MySQL in Docker under
# its own Compose project (E2E_COMPOSE_PROJECT, host port E2E_MYSQL_PORT), builds
# and starts the backend natively (E2E_BACKEND_PORT), runs Playwright (which
# starts the Vite dev server on E2E_FRONTEND_PORT) and removes the database and
# the backend afterwards, also on a failure. A port in use is an error.
E2E_COMPOSE_PROJECT ?= app-template-e2e
E2E_MYSQL_PORT ?= 13306
E2E_BACKEND_PORT ?= 8081
E2E_FRONTEND_PORT ?= 3000
E2E_RUN_ENV = E2E_COMPOSE_PROJECT=$(E2E_COMPOSE_PROJECT) E2E_MYSQL_PORT=$(E2E_MYSQL_PORT) \
	E2E_BACKEND_PORT=$(E2E_BACKEND_PORT) E2E_FRONTEND_PORT=$(E2E_FRONTEND_PORT)

# Auth run: TEST VALUES ONLY, for the throwaway e2e backend. They are not
# defaults of the application (it has none for JWT_SECRET and ADMIN_PASSWORD)
# and must never be used anywhere else.
E2E_JWT_SECRET ?= e2e-only-jwt-secret-not-for-any-real-deployment
E2E_ADMIN_USERNAME ?= admin
E2E_ADMIN_PASSWORD ?= e2e-only-admin-password
# false: the refresh cookie is refresh_token without Secure. true: the
# production setting, __Host-refresh_token with Secure (Chromium accepts it on
# http://localhost too).
E2E_SECURE_COOKIES ?= false
E2E_AUTH_SPECS ?= e2e/auth.spec.ts e2e/accessibility.spec.ts

# Auth off: every spec. auth.spec.ts skips itself (it needs AUTH_ENABLED=true),
# and the accessibility spec skips the Login and Admin Users pages.
test-e2e:
	$(E2E_RUN_ENV) scripts/e2e.sh off

# Auth on (AUTH_ENABLED=true, test JWT_SECRET and ADMIN_PASSWORD): the sign-in
# specs and the accessibility spec in strict mode (A11Y_REQUIRE_AUTH=1), so the
# Login and Admin Users pages are really checked.
test-e2e-auth:
	$(E2E_RUN_ENV) E2E_JWT_SECRET=$(E2E_JWT_SECRET) E2E_ADMIN_USERNAME=$(E2E_ADMIN_USERNAME) \
		E2E_ADMIN_PASSWORD=$(E2E_ADMIN_PASSWORD) E2E_SECURE_COOKIES=$(E2E_SECURE_COOKIES) \
		scripts/e2e.sh auth $(E2E_AUTH_SPECS)

# Install dependencies
install:
	cd backend && go mod download
	cd frontend && npm install

# Generate API documentation
docs:
	cd backend && swag init -g api/main.go

# Format and lint code
fmt:
	cd backend && go fmt ./...
	cd frontend && npm run format

lint: lint-backend lint-frontend check-workflows check-env-docs check-agents

lint-backend:
	cd backend && go vet ./...

lint-frontend:
	cd frontend && npm run lint

# Workflow hygiene (pinned actions, permissions, timeouts, main trigger), with
# the checker's own fixture tests first. CI runs it too.
check-workflows:
	scripts/check-workflows.sh --self-test
	scripts/check-workflows.sh

# Every variable the config package reads is documented in backend/.env.example
# (both copies) and docs/claude/operations.md, and nothing else is documented,
# with the checker's own fixture tests first. CI runs it too.
check-env-docs:
	scripts/check-env-docs.sh --self-test
	scripts/check-env-docs.sh

# The Claude Code agents, the ship-issue skill and .claude/settings.json are
# well formed and consistent (frontmatter, no forbidden tools or agents, nothing
# that publishes on the allow list), with the checker's own fixture tests
# first. CI runs it too.
check-agents:
	scripts/check-agents.sh --self-test
	scripts/check-agents.sh

# Markdown lint (rules: .markdownlint.yml, ignores: .markdownlint-cli2.jsonc;
# markdownlint-cli2 does not read .markdownlintignore).
# The version is pinned for reproducible runs; bump it on purpose.
MARKDOWNLINT_CLI2_VERSION ?= 0.24.1
lint-docs:
	npx --yes markdownlint-cli2@$(MARKDOWNLINT_CLI2_VERSION) "**/*.md"

# gosec security scan of the backend (pinned gosec built with the local Go).
gosec:
	scripts/gosec.sh


