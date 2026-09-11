# Root Makefile for cortex monorepo
# Orchestrates linting and formatting across all sub-projects

.PHONY: lint fmt pre-commit build-proxy-init pricing-table abctl authbridge-proxy help

BIN_DIR := $(CURDIR)/bin

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

pricing-table: ## Regenerate the bundled price table (COMMIT=<sha> [NO_PROXY_FOR_GEN=1])
ifndef COMMIT
	$(error COMMIT is required. Find the latest with: curl -sS 'https://api.github.com/repos/BerriAI/litellm/commits?path=model_prices_and_context_window.json&per_page=1' | jq -r '.[0].sha')
endif
	@# NO_PROXY_FOR_GEN=1 clears the proxy variables for this fetch. Matched against
	@# exactly "1", so NO_PROXY_FOR_GEN=0 means off rather than the surprising opposite.
	@# Off by default:
	@# "github.com is behind a TLS-intercepting proxy" was true on one developer's
	@# machine, not a property of this repo, and hardcoding it broke the target for
	@# anyone whose proxy is the only route out.
	cd authbridge/authlib && $(if $(filter 1,$(NO_PROXY_FOR_GEN)),HTTPS_PROXY= HTTP_PROXY= ALL_PROXY=,) \
		go run ./pricing/internal/gen -commit $(COMMIT) -dir ./pricing
	cd authbridge/authlib && go test ./pricing/ -run TestBundled

##@ Binary Targets

# authbridge-proxy's plugin set is resolved from a named profile via
# authbridge/scripts/profile-tags — the same helper CI uses — so the tag
# list stays in sync without hand-maintenance. abctl links no plugins.

abctl: ## Build abctl to ./bin/abctl
	@mkdir -p $(BIN_DIR)
	cd authbridge/cmd/abctl && go build -o $(BIN_DIR)/abctl .

authbridge-proxy: ## Build authbridge-proxy to ./bin/authbridge-proxy (PROFILE=full|lite|local, default full)
	@mkdir -p $(BIN_DIR)
	@# Resolve the profile's tag list at build time so the plugin set stays
	@# in sync with authbridge/scripts/profile-tags without hand-maintenance.
	@# An unknown profile exits non-zero — the $$(...) captures that and
	@# the target fails.
	TAGS=$$(go -C authbridge/scripts/profile-tags run . $(or $(PROFILE),full)) && \
		cd authbridge/cmd/authbridge-proxy && \
		go build -tags "$$TAGS" -o $(BIN_DIR)/authbridge-proxy .
