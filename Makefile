# Root Makefile for cortex monorepo
# Orchestrates linting and formatting across all sub-projects

.PHONY: lint fmt pre-commit build-proxy-init pricing-table abctl authbridge-proxy dev-install help

BIN_DIR := $(CURDIR)/bin

# Where dev-install puts the binaries. Hardcoded to match install.sh, which writes
# the same two names to the same directory — a dev install that landed somewhere else
# would leave two copies on PATH and no way to tell which one the service supervises.
DEV_BIN_DIR := $(HOME)/.local/bin

help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Code Quality

lint: ## Run all linters (pre-commit hooks)
	pre-commit run --all-files

fmt: ## Run formatters across all sub-projects
	cd core && go fmt ./...
	cd cmd/abctl && go fmt ./...
	cd cmd/authbridge-proxy && go fmt ./...
	cd cmd/authbridge-envoy && go fmt ./...
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
		go run ./pricing/internal/gen -commit $(COMMIT) -dir ./pricing
	cd core && go test ./pricing/ -run TestBundled

##@ Binary Targets

# authbridge-proxy's plugin set is resolved from a named profile via
# scripts/profile-tags — the same helper CI uses — so the tag
# list stays in sync without hand-maintenance. abctl links no plugins.

abctl: ## Build abctl to ./bin/abctl
	@mkdir -p $(BIN_DIR)
	@echo "→ building abctl"
	@cd cmd/abctl && GOWORK=off go build -o $(BIN_DIR)/abctl .

authbridge-proxy: ## Build authbridge-proxy to ./bin/authbridge-proxy (PROFILE=full|lite|local, default full)
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
		echo "→ building authbridge-proxy (profile $(or $(PROFILE),full)): $$TAGS" && \
		cd cmd/authbridge-proxy && \
		GOWORK=off go build -tags "$$TAGS" -o $(BIN_DIR)/authbridge-proxy .

##@ Local Dev

# install.sh installs Cortex from a RELEASE: it downloads prebuilt binaries, verifies
# their checksums, and starts the service. There was no equivalent for the tree you are
# sitting in — `make authbridge-proxy` built to ./bin and stopped, leaving four manual
# steps between a build and a running proxy. This is that bridge, and nothing else here
# is a substitute for it.

dev-install: authbridge-proxy abctl ## Build from this tree, install to ~/.local/bin, restart the service (PROFILE=full|lite|local)
	@mkdir -p $(DEV_BIN_DIR)
	@# Copy to a sibling name and rename, rather than writing over the target.
	@# Replacing a RUNNING executable in place fails with ETXTBSY on macOS, and both
	@# of these are usually running: the proxy under the supervisor, abctl in a TUI.
	@# rename swaps the directory entry and leaves the live process on its own inode.
	@# The .new file is removed on any failure: this directory is meant to be on PATH,
	@# so a half-copied executable left behind is worse than the failure itself.
	@for b in authbridge-proxy abctl; do \
		cp $(BIN_DIR)/$$b $(DEV_BIN_DIR)/$$b.new && \
		mv -f $(DEV_BIN_DIR)/$$b.new $(DEV_BIN_DIR)/$$b || \
		{ rm -f $(DEV_BIN_DIR)/$$b.new; exit 1; }; \
	done
	@echo "→ installed to $(DEV_BIN_DIR)"
	@# Everything below runs by absolute path, so a missing PATH entry does not fail
	@# this target — it fails the NEXT thing the developer types. install.sh checks the
	@# same thing and offers to fix the shell profile; a build target should not edit
	@# dotfiles, so it says so and stops there.
	@case ":$$PATH:" in \
		*":$(DEV_BIN_DIR):"*) ;; \
		*) echo >&2; echo "!  $(DEV_BIN_DIR) is not on PATH — \`abctl\` will not resolve until you add it" >&2; echo >&2;; \
	esac
	@# A machine that has never run Cortex has no config, and `service install` refuses
	@# without one. Minting it here is what makes this work on a clean checkout rather
	@# than only as an upgrade.
	@if [ ! -f "$(HOME)/.cortex/config.yaml" ]; then \
		echo "→ no config at ~/.cortex/config.yaml; writing the built-in one"; \
		$(DEV_BIN_DIR)/authbridge-proxy --local --write-config || exit 1; \
	fi
	@# Absolute path, not bare `abctl`: PATH may resolve to a different copy, and the
	@# unit records which abctl wrote it. --yes because a build command that stops to
	@# ask is not a one-liner; --restart because install is otherwise free to re-run and
	@# would skip the restart whenever the rebuild happened to be byte-identical.
	@# Quiet: make would otherwise echo an absolute path and a flag list immediately
	@# after the previous line's output, which read as one run-together sentence.
	@#
	@# "installing and starting" rather than "restarting": serviceInstall is also the
	@# first-install path, where there is nothing to restart and no captured history to
	@# clear. Both consequences are abctl's to report — it can tell whether anything was
	@# running, and a Makefile echo cannot — so the session-store line moved there and
	@# this says only what is true on both paths.
	@echo "→ installing and starting the service"
	@$(DEV_BIN_DIR)/abctl service install --yes --restart
	@echo
	@$(DEV_BIN_DIR)/abctl service status
