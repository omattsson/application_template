.PHONY: test test-coverage test-coverage-html build run clean lint generate-mocks docs check-health

# Build the application
build:
	go build -o bin/api ./api

# Run the application
run:
	go run ./api

# Run tests
test:
	go test -v ./...

# Run the unit tests with coverage and gate it: every package of
# COVERAGE_PACKAGES must reach COVERAGE_MIN percent on its own (not an average).
# -coverpkg makes every test binary count toward the listed packages, so the
# api main package is credited for what its bootstrap tests run (its main()
# cannot be tested; the rest of the package can). The gate, the list and the
# reasons for what is not listed: ../scripts/coverage-gate.sh and
# coverage-packages.txt. -short is what CI runs (no database needed).
COVERAGE_PACKAGES ?= coverage-packages.txt
COVERAGE_MIN ?= 80
test-coverage:
	@mkdir -p coverage
	@echo "Running tests with coverage..."
	@pkgs=$$(sed -e 's/#.*//' -e 's/[[:space:]]//g' $(COVERAGE_PACKAGES) | grep . | paste -sd, -); \
	[ -n "$$pkgs" ] || { echo "ERROR: $(COVERAGE_PACKAGES) lists no package"; exit 1; }; \
	go test -short -covermode=atomic -coverprofile=coverage/coverage.out -coverpkg="$$pkgs" ./... > coverage/test-output.txt 2>&1 \
		|| { cat coverage/test-output.txt; echo "ERROR: the tests failed"; exit 1; }
	@echo "Generating coverage reports (coverage/coverage.html, coverage/coverage-summary.txt)..."
	@go tool cover -html=coverage/coverage.out -o coverage/coverage.html
	@go tool cover -func=coverage/coverage.out > coverage/coverage-summary.txt
	@echo "Checking coverage per package (minimum $(COVERAGE_MIN)%)..."
	@../scripts/coverage-gate.sh coverage/coverage.out $(COVERAGE_PACKAGES) $(COVERAGE_MIN)

# Run tests with coverage and open the report
test-coverage-html: test-coverage
	@echo "Opening coverage report..."
	@open coverage/coverage.html

# Clean up
clean:
	rm -rf bin coverage/

# Run linter
lint:
	golangci-lint -c .golangci.yml run

# Generate mocks (if needed)
generate-mocks:
	go generate ./...

# Generate Swagger documentation
docs:
	$(shell go env GOPATH)/bin/swag init -g api/main.go

# Check service health
check-health:
	@echo "Checking liveness..."
	@curl -v http://localhost:8080/health/live || true
	@echo "\nChecking readiness..."
	@curl -v http://localhost:8080/health/ready || true

# Run all development setup
dev: docs build run
