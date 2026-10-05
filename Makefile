# Root Makefile for cortex monorepo
# Orchestrates linting and formatting across all sub-projects

.PHONY: lint fmt pre-commit build-proxy-init pricing-table agentop cortex dev-install help

BIN_DIR := $(CURDIR)/bin

help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Code Quality

lint: ## Run all linters (pre-commit hooks)
	pre-commit run --all-files

fmt: ## Run formatters across all sub-projects
	cd core && go fmt ./...
	@# Its own module, so the `cd core` above does not reach it: Go excludes nested
	@# modules from ./..., and moving it under core/ made it only look covered.
	cd core/storage/redis && go fmt ./...
	cd cmd/agentop && go fmt ./...
	cd cmd/cortex && go fmt ./...
	cd cmd/cortex-envoy && go fmt ./...
	@# Scope matches the ruff hooks in .pre-commit-config.yaml. Both skip the root
	@# tests/ tree, which `authbridge/` never covered and which does not format clean.
	@# Must be `--exclude ./tests`, root-anchored like the hook's `^tests/`: plain
	@# `--exclude tests` also drops deploy/sparc-service/tests, and `--exclude /tests`
	@# stops excluding root tests/ entirely.
	ruff format . --exclude ./tests

pre-commit: ## Install pre-commit hooks (including commit-msg)
	pre-commit install --hook-type pre-commit --hook-type commit-msg

##@ Sub-project Targets

build-proxy-init: ## Build the proxy-init iptables init container
	cd deploy/proxy-init && make docker-build-init

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
	cd core && $(if $(filter 1,$(NO_PROXY_FOR_GEN)),HTTPS_PROXY= HTTP_PROXY= ALL_PROXY=,) \
		go run ./cost/pricing/internal/gen -commit $(COMMIT) -dir ./cost/pricing
	cd core && go test ./cost/pricing/ -run TestBundled

##@ Binary Targets

# cortex's plugin set is resolved from a named profile via
# scripts/profile-tags — the same helper CI uses — so the tag
# list stays in sync without hand-maintenance. agentop links no plugins.

agentop: ## Build agentop to ./bin/agentop
	@mkdir -p $(BIN_DIR)
	@echo "→ building agentop"
	@cd cmd/agentop && GOWORK=off go build -o $(BIN_DIR)/agentop .

cortex: ## Build cortex to ./bin/cortex (PROFILE=full|lite|local, default full)
	@mkdir -p $(BIN_DIR)
	@# Restrict PROFILE to the plugin sets this binary ships.
	@if [ -n "$(PROFILE)" ] && [ -z "$(filter full lite local,$(PROFILE))" ]; then \
		echo "PROFILE=$(PROFILE) is not one of: full lite local" >&2; \
		exit 1; \
	fi
	@# GOWORK=off matches CI and the Dockerfile — workspace mode can select a
	@# higher third-party version than this binary's own go.mod requires.
	@# The resolved tags, not just the profile name: "why is plugin X missing from my
	@# binary" is answered by the tag list, and quieting the command removed the only
	@# place it appeared.
	@TAGS=$$(go -C scripts/profile-tags run . $(or $(PROFILE),full)) && \
		echo "→ building cortex (profile $(or $(PROFILE),full)): $$TAGS" && \
		cd cmd/cortex && \
		GOWORK=off go build -tags "$$TAGS" -o $(BIN_DIR)/cortex .

##@ Local Dev

# install.sh installs Cortex from a RELEASE: it downloads prebuilt binaries, verifies
# their checksums, stages them, and hands off to `agentop setup --from <stage>`. This
# is the same handoff for the tree you are sitting in: build both binaries to ./bin,
# then let the agentop just built install them. Setup copies them to ~/.local/bin, the
# directory a release install uses too, so there is never a second copy on PATH. It
# also writes the config on a clean machine, starts or restarts the service, and
# removes the pre-rename abctl and authbridge-proxy. One code path, so a dev install
# cannot drift from a release install.

dev-install: cortex agentop ## Build from this tree, install it with agentop setup, and restart the proxy (PROFILE=full|lite|local)
	@# BIN_DIR's agentop, not the installed one: the agentop just built is the one that
	@# knows how this tree installs. Setup leaves BIN_DIR in place; it deletes only a
	@# stage the installer made, and only when the installer says so. --yes, because a
	@# build command that stops to ask is not a one-liner. --no-modify-path, because a
	@# build target should not edit dotfiles; setup says so when ~/.local/bin is not
	@# on PATH. --restart, because setup otherwise skips the restart when the rebuild
	@# is byte-identical.
	@#
	@# No @ on the line itself, so make prints the command before running it: the
	@# restart is this line's doing and nothing else on screen says so, and a setup
	@# that refuses names this same command as the one to re-run.
	$(BIN_DIR)/agentop setup --from $(BIN_DIR) --yes --no-modify-path --restart
