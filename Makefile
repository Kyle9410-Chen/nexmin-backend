SHELL := /bin/bash

GREEN = \033[0;32m
BLUE = \033[0;34m
RED = \033[0;31m
NC = \033[0m

COMPOSE = docker compose -f ./.deploy/local/compose.yaml

.PHONY: all run build test lint gen config up down clean token

all: build

## config: create config.yaml from the example when it is missing.
## config.yaml is gitignored, so every developer needs their own. Never overwrite an
## existing one -- it usually holds hand-edited local secrets.
config:
	@if [ -f config.yaml ]; then \
		echo -e "  -> config.yaml already exists, leaving it untouched" >&2; \
	else \
		cp config.example.yaml config.yaml \
			&& echo -e "  -> $(GREEN)created config.yaml from config.example.yaml$(NC)" >&2; \
	fi

## up: start local dependencies and wait until they are healthy.
up:
	@echo -e ":: $(GREEN)Starting depending services...$(NC)"
	@$(COMPOSE) up -d --wait \
		|| (echo -e "  -> $(RED)Depending services failed to start$(NC)" && exit 1)

## down: stop local dependencies, keeping their data.
down:
	@echo -e ":: $(GREEN)Stopping depending services...$(NC)"
	@$(COMPOSE) down

## run: bring up the environment and start the backend in debug mode.
run: config
	@echo -e ":: $(GREEN)Starting backend...$(NC)"
	@echo -e "  -> Downloading go dependencies..."
	@go mod download \
		|| (echo -e "  -> $(RED)Failed to download go dependencies$(NC)" && exit 1)
	@$(MAKE) --no-print-directory up
	@$(MAKE) --no-print-directory gen
	@echo -e "  -> Building backend binary..."
	@go build -o bin/backend cmd/backend/main.go \
		|| (echo -e "  -> $(RED)Build failed$(NC)" && exit 1)
	@echo -e "  -> Starting backend..."
	@DEBUG=true ./bin/backend \
		&& echo -e "==> $(BLUE)Successfully shutdown backend$(NC)" \
		|| (echo -e "==> $(RED)Backend exited with an error$(NC)" && exit 1)

build: gen
	@echo -e ":: $(GREEN)Building backend...$(NC)"
	@go build -o bin/backend cmd/backend/main.go \
		&& echo -e "==> $(BLUE)Build completed successfully$(NC)" \
		|| (echo -e "==> $(RED)Build failed$(NC)" && exit 1)

test: gen
	@echo -e ":: $(GREEN)Running tests...$(NC)"
	@go test -cover ./... \
		&& echo -e "==> $(BLUE)All tests passed$(NC)" \
		|| (echo -e "==> $(RED)Tests failed$(NC)" && exit 1)

lint:
	@echo -e ":: $(GREEN)Linting...$(NC)"
	@golangci-lint run ./... \
		&& echo -e "==> $(BLUE)No lint issues$(NC)" \
		|| (echo -e "==> $(RED)Lint issues found$(NC)" && exit 1)

## gen: merge per-package schema.sql files and regenerate sqlc code.
## sqlc.yaml and internal/database/full_schema.sql are generated artifacts -- edit the
## per-package schema.sql/queries.sql instead.
gen:
	@echo -e ":: $(GREEN)Generating schema and code...$(NC)"
	@echo -e "  -> Merging schemas..."
	@./scripts/create_full_schema.sh \
		|| (echo -e "  -> $(RED)Schema merge failed$(NC)" && exit 1)
	@echo -e "  -> Generating sqlc code..."
	@sqlc generate \
		|| (echo -e "  -> $(RED)sqlc generation failed$(NC)" && exit 1)
	@echo -e "==> $(BLUE)Generation completed$(NC)"

## token: mint a JWT for testing protected endpoints, signed with config.yaml's secret.
## Prints only the token, so it can be captured directly: TOKEN=$$(make -s token)
token: config
	@go run ./scripts/token $(ARGS)

clean:
	@echo -e ":: $(GREEN)Cleaning up...$(NC)"
	@echo -e "  -> Removing depending services and their data..."
	@$(COMPOSE) down -v --remove-orphans \
		|| (echo -e "  -> $(RED)Failed to remove depending services$(NC)" && exit 1)
	@echo -e "  -> Removing backend binary..."
	@rm -rf bin/
	@echo -e "==> $(BLUE)Cleanup completed$(NC)"
