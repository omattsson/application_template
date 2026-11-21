.PHONY: dev-mail test-e2e test-e2e-auth test-coverage test-backend-repeat seed loadtest loadtest-stress check-make-targets gosec lint-backend lint-frontend lint-docs check-workflows check-env-docs check-agents check-dockerfiles check-trivyignore check-domain-terms shellcheck check-prod-credentials test-prod-credentials test-backend-unit dev dev-backend dev-frontend dev-otel mysql mysql-start mysql-stop mysql-logs mysql-shell build clean test install docs fmt lint

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

# Development mode with the observability setup (OpenTelemetry Collector,
# Prometheus, Tempo, Grafana on http://localhost:3001). Development only: see
# docker-compose.otel.yml and "Observability setup" in docs/claude/operations.md.
dev-otel:
	$(DEV_ENV) docker compose -f docker-compose.yml -f docker-compose.otel.yml up --build

# Development mode with a local mail catcher (Mailpit, web UI on
# http://localhost:8025). Development only: see docker-compose.mail.yml and
# "Mail and password reset" in docs/claude/operations.md. Set AUTH_ENABLED=true
# to turn the password reset on.
dev-mail:
	$(DEV_ENV) docker compose -f docker-compose.yml -f docker-compose.mail.yml up --build

# The production targets refuse to start with the placeholder database
# credentials of docker-compose.yml (set MYSQL_PASSWORD and MYSQL_ROOT_PASSWORD
# in the environment or in .env, or ALLOW_DEFAULT_CREDENTIALS=1 knowingly).
# Production mode for both services
prod: check-prod-credentials
	$(PROD_ENV) docker compose up --build

# Production mode for backend only
prod-backend: check-prod-credentials
	$(PROD_ENV) docker compose up --build backend

# Production mode for frontend only (also starts the backend it depends on)
prod-frontend: check-prod-credentials
	$(PROD_ENV) docker compose up --build frontend

# Refuse the placeholder database credentials. See scripts/check-prod-credentials.sh.
check-prod-credentials:
	scripts/check-prod-credentials.sh

# The fixture tests of that guard (part of lint; CI runs them too).
test-prod-credentials:
	scripts/check-prod-credentials.sh --self-test

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

# Backend unit tests with coverage and a gate: every package of
# backend/coverage-packages.txt needs COVERAGE_MIN percent (default 80) on its
# own. Reports: backend/coverage/coverage.html and coverage-summary.txt. See
# backend/Makefile and scripts/coverage-gate.sh.
test-coverage:
	$(MAKE) -C backend test-coverage $(if $(COVERAGE_MIN),COVERAGE_MIN=$(COVERAGE_MIN))

# Repeat (-race, -short, -count=REPEAT_COUNT) the tests of the backend packages
# that changed against REPEAT_BASE, to catch a flaky test before CI does.
# Changed = .go files that differ from the merge base of REPEAT_BASE and HEAD
# (working tree included), plus untracked ones. REPEAT_BASE defaults to
# origin/main; when that ref does not exist the local main is used, and when
# neither exists the target stops with an error. No changed package is not an
# error: there is nothing to repeat. -race needs cgo.
# The variables are exported and read in the recipe, never pasted into the shell text.
REPEAT_BASE ?= origin/main
REPEAT_COUNT ?= 3
export REPEAT_BASE REPEAT_COUNT
test-backend-repeat:
	@case "$$REPEAT_COUNT" in ''|*[!0-9]*|0|0[0-9]*) echo "ERROR: REPEAT_COUNT must be a positive integer, got '$$REPEAT_COUNT'"; exit 1;; esac; \
	ref="$$REPEAT_BASE"; \
	if ! git rev-parse --verify --quiet "$$ref^{commit}" >/dev/null; then \
		if [ "$$ref" = origin/main ] && git rev-parse --verify --quiet "main^{commit}" >/dev/null; then \
			echo "NOTICE: origin/main does not exist; using the local main as REPEAT_BASE"; ref=main; \
		else \
			echo "ERROR: REPEAT_BASE=$$ref is not a known ref. Fetch it, or set REPEAT_BASE (for example REPEAT_BASE=main)."; exit 1; \
		fi; \
	fi; \
	base=$$(git merge-base "$$ref" HEAD) || { echo "ERROR: no merge base of $$ref and HEAD"; exit 1; }; \
	tracked=$$(git diff --name-only "$$base" -- backend) || { echo "ERROR: git diff failed"; exit 1; }; \
	untracked=$$(git ls-files --others --exclude-standard -- backend) || { echo "ERROR: git ls-files failed"; exit 1; }; \
	files=$$(printf '%s\n%s\n' "$$tracked" "$$untracked" | grep '\.go$$' | sort -u || true); \
	pkgs=""; \
	for f in $$files; do \
		d=$$(dirname "$${f#backend/}"); \
		[ -d "backend/$$d" ] || continue; \
		ls backend/$$d/*.go >/dev/null 2>&1 || continue; \
		[ "$$d" = "." ] || d="./$$d"; \
		pkgs="$$pkgs $$d"; \
	done; \
	pkgs=$$(printf '%s\n' $$pkgs | sort -u | tr '\n' ' '); \
	if [ -z "$$(echo $$pkgs)" ]; then echo "No changed backend Go packages vs $$ref; nothing to repeat."; exit 0; fi; \
	echo "Repeating tests ($$REPEAT_COUNT x, -race, -short) for: $$pkgs"; \
	cd backend && go test -short -race -count="$$REPEAT_COUNT" $$pkgs

# Create sample Items through the API of a running backend (make dev or
# make dev-backend). Needs API_URL (default http://localhost:8081); with
# AUTH_ENABLED=true also API_TOKEN or ADMIN_PASSWORD (and ADMIN_USERNAME).
# SEED_COUNT (default 10) items named "Sample Item NN"; a second run creates
# nothing. See scripts/seed-dev-data.sh.
seed:
	scripts/seed-dev-data.sh

# k6 load tests of the Items API and /ws against a RUNNING backend: a local k6
# binary (https://grafana.com/docs/k6/latest/set-up/install-k6/, the load tests
# use only the standard k6 modules) runs loadtest/k6-api.js and loadtest/k6-websocket.js.
# k6 is looked up only when the recipe runs, so make -n works without it. K6
# is the binary to run. The target must be local; LOADTEST_ALLOW_REMOTE=1 allows
# another one. A crossed threshold (error rate, p95/p99 latency) makes k6, and
# so make, fail. Start the backend with a high RATE_LIMIT (the default of 100
# requests per minute per IP stops the test at once), for example:
#   RATE_LIMIT=1000000 make dev-backend
# Credentials of a backend with AUTH_ENABLED=true: API_TOKEN, API_KEY or
# ADMIN_PASSWORD (ADMIN_USERNAME), from the environment, never on a command
# line. See "Load tests" in docs/claude/operations.md.
API_URL ?= http://localhost:8081
K6 ?= k6
LOADTEST_VUS ?= 10
LOADTEST_DURATION ?= 30s
LOADTEST_STRESS_VUS ?= 100
LOADTEST_STRESS_STEP ?= 30s
LOADTEST_ENV = API_URL=$(API_URL) LOADTEST_VUS=$(LOADTEST_VUS) LOADTEST_DURATION=$(LOADTEST_DURATION)
define require_k6
	@command -v $(K6) >/dev/null 2>&1 || { echo "ERROR: k6 not found ($(K6)). Install it (brew install k6, or https://grafana.com/docs/k6/latest/set-up/install-k6/) or set K6=/path/to/k6."; exit 1; }
endef

loadtest:
	$(require_k6)
	$(LOADTEST_ENV) $(K6) run loadtest/k6-api.js
	$(LOADTEST_ENV) $(K6) run loadtest/k6-websocket.js

# Opt-in stress profile: ramps up to LOADTEST_STRESS_VUS users, in steps of
# LOADTEST_STRESS_STEP. Never part of make loadtest. Same safety rules.
loadtest-stress:
	$(require_k6)
	$(LOADTEST_ENV) LOADTEST_STRESS_VUS=$(LOADTEST_STRESS_VUS) LOADTEST_STRESS_STEP=$(LOADTEST_STRESS_STEP) $(K6) run loadtest/k6-stress.js

test-frontend:
	cd frontend && npm test

# Frontend e2e tests (Playwright). scripts/e2e.sh starts MySQL in Docker under
# its own Compose project (E2E_COMPOSE_PROJECT, host port E2E_MYSQL_PORT), builds
# and starts the backend natively (E2E_BACKEND_PORT), runs Playwright (which
# starts the Vite dev server on E2E_FRONTEND_PORT) and removes the database and
# the backend afterwards, also on a failure. A port in use is an error.
# test-e2e-auth also starts Mailpit (docker-compose.mail.yml, SMTP on
# E2E_MAILPIT_SMTP_PORT, web UI and API on E2E_MAILPIT_UI_PORT) for the password
# reset specs; test-e2e starts none.
E2E_COMPOSE_PROJECT ?= app-template-e2e
E2E_MYSQL_PORT ?= 13306
E2E_BACKEND_PORT ?= 8081
E2E_FRONTEND_PORT ?= 3000
E2E_MAILPIT_SMTP_PORT ?= 11025
E2E_MAILPIT_UI_PORT ?= 18025
E2E_RUN_ENV = E2E_COMPOSE_PROJECT=$(E2E_COMPOSE_PROJECT) E2E_MYSQL_PORT=$(E2E_MYSQL_PORT) \
	E2E_BACKEND_PORT=$(E2E_BACKEND_PORT) E2E_FRONTEND_PORT=$(E2E_FRONTEND_PORT) \
	E2E_MAILPIT_SMTP_PORT=$(E2E_MAILPIT_SMTP_PORT) E2E_MAILPIT_UI_PORT=$(E2E_MAILPIT_UI_PORT)

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
# e2e/password-reset, e2e/api-keys and e2e/audit-logs match the password reset, API keys and audit log specs as patterns.
E2E_AUTH_SPECS ?= e2e/auth.spec.ts e2e/accessibility.spec.ts e2e/password-reset e2e/api-keys e2e/audit-logs e2e/account.spec.ts

# Auth off: every spec. auth.spec.ts skips itself (it needs AUTH_ENABLED=true),
# and the accessibility spec skips the Login and Admin Users pages.
test-e2e:
	$(E2E_RUN_ENV) scripts/e2e.sh off

# Auth on (AUTH_ENABLED=true, test JWT_SECRET and ADMIN_PASSWORD): the sign-in
# specs and the accessibility spec in strict mode (A11Y_REQUIRE_AUTH=1), so the
# Login and Admin Users pages are really checked, and the password reset spec
# (Mailpit, the module on, the seeded user e2e-reset-user).
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

# Everything the "Repository checks", "Backend" (vet) and "Frontend" (lint)
# jobs of .github/workflows/pull-request.yml run, so a clean local lint means
# a clean CI lint. (Markdown: make lint-docs. gosec: make gosec.)
lint: lint-backend lint-frontend check-workflows check-dockerfiles check-trivyignore check-env-docs check-agents check-make-targets check-domain-terms test-prod-credentials shellcheck

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

# The ops targets (test-coverage, test-backend-repeat, seed, loadtest,
# loadtest-stress) exist, are .PHONY and can be planned with make -n (also
# without k6), with the checker's own fixture tests first. CI runs it too.
check-make-targets:
	scripts/check-make-targets.sh --self-test
	scripts/check-make-targets.sh

# Dockerfile hardening rules, with the checker's own fixture tests first. CI
# runs it too.
check-dockerfiles:
	scripts/check-dockerfiles.sh --self-test
	scripts/check-dockerfiles.sh

# The .trivyignore entries are well formed and not expired, with the checker's
# own fixture tests first. CI runs it too.
check-trivyignore:
	scripts/check-trivyignore.sh --self-test
	scripts/check-trivyignore.sh

# No project-specific terms in tracked files (git add new files first: the
# scan reads git-tracked files only), with the checker's own tests first. CI
# runs it too.
check-domain-terms:
	scripts/check-domain-terms.test.sh
	scripts/check-domain-terms.sh

# shellcheck of every script. CI installs it; locally a missing shellcheck is
# a warning (brew install shellcheck, apt install shellcheck), CI still fails.
shellcheck:
	@if command -v shellcheck >/dev/null 2>&1; then shellcheck scripts/*.sh; \
	elif [ -n "$$CI" ]; then echo "ERROR: shellcheck is not installed"; exit 1; \
	else echo "WARNING: shellcheck is not installed, skipping (CI runs it)"; fi

# Markdown lint (rules: .markdownlint.yml, ignores: .markdownlint-cli2.jsonc;
# markdownlint-cli2 does not read .markdownlintignore).
# The version is pinned for reproducible runs; bump it on purpose.
MARKDOWNLINT_CLI2_VERSION ?= 0.24.1
lint-docs:
	npx --yes markdownlint-cli2@$(MARKDOWNLINT_CLI2_VERSION) "**/*.md"

# gosec security scan of the backend (pinned gosec built with the local Go).
gosec:
	scripts/gosec.sh


