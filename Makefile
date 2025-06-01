.PHONY: dev dev-backend dev-frontend build clean test install docs fmt lint

# Development mode for both services
dev:
	NODE_ENV=development PORT=3000 BACKEND_PORT=8081 GIN_MODE=debug docker compose up --build

# Development mode for backend only
dev-backend:
	NODE_ENV=development BACKEND_PORT=8081 GIN_MODE=debug docker compose up --build backend

# Development mode for frontend only
dev-frontend:
	NODE_ENV=development PORT=3000 docker compose up --build frontend

# Production mode for both services
prod:
	NODE_ENV=production PORT=80 BACKEND_PORT=8080 GIN_MODE=release docker compose up --build

# Production mode for backend only
prod-backend:
	NODE_ENV=production BACKEND_PORT=8080 GIN_MODE=release docker compose up --build backend

# Production mode for frontend only
prod-frontend:
	NODE_ENV=production PORT=80 docker compose up --build frontend

# Stop and remove all containers, networks, and volumes
clean:
	docker compose down -v

# Remove all unused Docker resources
prune:
	docker system prune -f

# Run all tests
test: test-backend test-frontend

test-backend:
	cd backend && go test ./... -v

test-frontend:
	cd frontend && npm test

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

lint:
	cd backend && go vet ./...
	cd frontend && npm run lint


