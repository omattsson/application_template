.PHONY: dev dev-backend dev-frontend mysql mysql-start mysql-stop mysql-logs mysql-shell build clean test install docs fmt lint

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


