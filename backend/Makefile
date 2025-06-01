.PHONY: test test-coverage build run clean lint generate-mocks docs check-health

# Build the application
build:
	go build -o bin/api ./api/main.go

# Run the application
run:
	go run ./api/main.go

# Run tests
test:
	go test -v ./...

# Run tests with coverage
test-coverage:
	@mkdir -p coverage
	@echo "Running tests with coverage..."
	@go test -coverprofile=coverage/coverage.out -covermode=atomic ./... | tee coverage/test-output.txt
	@echo "\nGenerating coverage reports..."
	@go tool cover -html=coverage/coverage.out -o coverage/coverage.html
	@go tool cover -func=coverage/coverage.out | tee coverage/coverage-summary.txt
	@echo "\nChecking coverage threshold (minimum 80%)..."
	@coverage_value=$$(go tool cover -func=coverage/coverage.out | grep total: | grep -Eo '[0-9]+\.[0-9]+' || echo "0.0"); \
	if [ $$(echo "$$coverage_value < 80.0" | bc -l) -eq 1 ]; then \
		echo "\033[0;31mTest coverage ($$coverage_value%) is below threshold (80%)\033[0m"; \
		exit 1; \
	else \
		echo "\033[0;32mTest coverage ($$coverage_value%) meets threshold\033[0m"; \
	fi

# Run tests with coverage and open the report
test-coverage-html: test-coverage
	@echo "Opening coverage report..."
	@open coverage/coverage.html

# Clean up
clean:
	rm -rf bin coverage/

# Run linter
lint:
	if ! command -v golangci-lint &> /dev/null; then \
		curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $$(go env GOPATH)/bin; \
	fi
	golangci-lint run

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
