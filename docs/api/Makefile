SHELL := /bin/bash

GREEN = \033[0;32m
BLUE = \033[0;34m
RED = \033[0;31m
NC = \033[0m

SCHEMA = tsp-output/schema/openapi.yaml

.PHONY: all install format compile clean mock preview lint

all: format compile
	@echo -e "==> $(BLUE)All tasks completed successfully$(NC)"

## install: fetch the TypeSpec compiler and emitters.
install:
	@echo -e ":: $(GREEN)Installing dependencies...$(NC)"
	@npm install \
		|| (echo -e "==> $(RED)Install failed$(NC)" && exit 1)

## format: check that every .tsp file is formatted.
format:
	@echo -e ":: $(GREEN)Checking TypeSpec format...$(NC)"
	@npx tsp format --check "**/*.tsp" \
		&& echo -e "==> $(BLUE)Format check completed$(NC)" \
		|| (echo -e "==> $(RED)Format check failed, run 'npx tsp format \"**/*.tsp\"'$(NC)" && exit 1)

## compile: emit the OpenAPI document.
compile:
	@echo -e ":: $(GREEN)Compiling TypeSpec...$(NC)"
	@npx tsp compile . \
		&& echo -e "==> $(BLUE)Compiled to $(SCHEMA)$(NC)" \
		|| (echo -e "==> $(RED)Compilation failed$(NC)" && exit 1)

## lint: run Redocly's spec linter over the emitted document.
lint: compile
	@echo -e ":: $(GREEN)Linting OpenAPI...$(NC)"
	@npx @redocly/cli@latest lint $(SCHEMA)

## preview: serve Swagger UI over the emitted document.
## Port 8090, deliberately not the backend's 8080.
preview: compile
	@cp swagger-ui.html tsp-output/schema/index.html
	@echo -e ":: $(GREEN)Swagger UI on http://localhost:8090 (Ctrl-C to stop)$(NC)"
	@cd tsp-output/schema && python3 -m http.server 8090

## mock: serve a mock implementation of the spec on :4010.
mock: compile
	@docker run --init --rm \
		-v $(PWD):/api \
		-p 4010:4010 stoplight/prism:4 \
		mock -h 0.0.0.0 "/api/$(SCHEMA)"

clean:
	@echo -e ":: $(GREEN)Cleaning build artifacts...$(NC)"
	@rm -rf tsp-output \
		&& echo -e "==> $(BLUE)Cleanup completed$(NC)"
