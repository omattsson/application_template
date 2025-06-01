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
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

# Clean up
clean:
	rm -rf bin coverage.out coverage.html

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
