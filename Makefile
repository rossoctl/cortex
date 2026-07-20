# Root Makefile for rossocortex monorepo
# Orchestrates linting and formatting across all sub-projects

.PHONY: lint fmt pre-commit build-proxy-init help

help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Code Quality

lint: ## Run all linters (pre-commit hooks)
	pre-commit run --all-files

fmt: ## Run formatters across all sub-projects
	cd authbridge/authlib && go fmt ./...
	cd authbridge/cmd/abctl && go fmt ./...
	cd authbridge/cmd/authbridge-proxy && go fmt ./...
	cd authbridge/cmd/authbridge-envoy && go fmt ./...
	ruff format authbridge/

pre-commit: ## Install pre-commit hooks (including commit-msg)
	pre-commit install --hook-type pre-commit --hook-type commit-msg

##@ Sub-project Targets

build-proxy-init: ## Build the proxy-init iptables init container
	cd authbridge/proxy-init && make docker-build-init
