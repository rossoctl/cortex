# IBAC demo Makefile — kagenti-UI flow.
#
# End-to-end:
#
#   make demo-ibac     # build + load demo images, deploy, patch, ready-to-chat banner
#   make show-result   # render pipeline forensic of last session
#   make undeploy      # remove the demo's resources from team1
#
# IBAC ships in the default authbridge image pulled by the kagenti chart,
# so this demo only needs to enable the plugin in the rendered ConfigMap.
#
# Prerequisites:
#   - kagenti installed on a kind cluster (this is the operator-flow
#     demo; for plain-kind testing of the IBAC plugin, see commit history
#     for the standalone variant)
#   - ollama on the host with llama3.2:3b pulled
#   - kubectl, kind, podman/docker, python3 + PyYAML

.PHONY: help build-images build-abctl load-images deploy \
        patch-config demo-ibac show-result undeploy \
        logs-agent logs-evil preflight wait-pods

# `make` with no target prints help — easier discoverability than the
# walls-of-comments at the top.
.DEFAULT_GOAL := help

help:  ## Show this menu (default)
	@printf "\nIBAC demo — operator-injected agent in kagenti's team1 namespace.\n\n"
	@printf "Typical run:\n"
	@printf "  \033[1mmake demo-ibac\033[0m\n"
	@printf "  ... chat with email-agent in the kagenti UI ...\n"
	@printf "  \033[1mmake show-result\033[0m\n"
	@printf "  \033[1mmake undeploy\033[0m\n\n"
	@printf "Targets:\n"
	@awk 'BEGIN { FS = ":.*## " } \
	     /^[a-zA-Z_-]+:.*## / { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 }' \
	     $(MAKEFILE_LIST)
	@printf "\nVariables:\n"
	@printf "  \033[36m%-18s\033[0m %s\n" "KIND_CLUSTER_NAME" "kind cluster to load images into (default: kagenti)"
	@printf "  \033[36m%-18s\033[0m %s\n" "NAMESPACE"         "agent namespace (default: team1)"
	@printf "  \033[36m%-18s\033[0m %s\n" "AGENT_NAME"        "deployment name (default: email-agent)"
	@printf "  \033[36m%-18s\033[0m %s\n" "CONTAINER_RUNTIME" "podman or docker (auto-detected)"
	@printf "\nPrerequisites: kagenti install on a kind cluster, ollama on the host\n"
	@printf "with llama3.2:3b, kubectl/kind/podman, python3 + PyYAML.\n\n"

KIND_CLUSTER_NAME ?= kagenti
KIND_NODE         ?= $(KIND_CLUSTER_NAME)-control-plane
NAMESPACE         ?= team1
AGENT_NAME        ?= email-agent

# Container runtime auto-detect.
CONTAINER_RUNTIME ?= $(shell command -v podman >/dev/null 2>&1 && echo podman || echo docker)

# Demo-image tags (local-only, not pushed).
AGENT_IMAGE       := email-agent:latest
EMAIL_IMAGE       := ibac-email-server:latest
EVIL_IMAGE        := ibac-evil-server:latest

# Per-checkout build/load bookkeeping. `.stamps/<svc>.built` is a make-tracked
# stamp file: its mtime is compared against source files so build-images
# rebuilds only when something actually changed. `.stamps/<svc>.loaded`
# tracks the last successful kind-load — managed manually because make can't
# stat images inside a kind node. Wiped by `undeploy`.
STAMP_DIR   := .stamps
LOAD_MARKER := $(STAMP_DIR)/.loaded-this-run

# ---------- Pre-flight ----------

preflight:  ## Verify kagenti is installed + tools are available

	@kubectl get ns kagenti-system >/dev/null 2>&1 || { \
	  echo "ERROR: namespace 'kagenti-system' not found — kagenti doesn't appear to be installed."; \
	  echo "       Install kagenti first (see https://github.com/kagenti/kagenti/blob/main/docs/install.md)"; \
	  exit 1; \
	}
	@kubectl get ns $(NAMESPACE) >/dev/null 2>&1 || { \
	  echo "ERROR: namespace '$(NAMESPACE)' not found — kagenti's agent namespace is missing."; \
	  echo "       Did the kagenti install complete? Try: kubectl get ns | grep team"; \
	  exit 1; \
	}
	@command -v kind >/dev/null 2>&1 || { echo "ERROR: kind not installed"; exit 1; }
	@python3 -c 'import yaml' 2>/dev/null || { \
	  echo "ERROR: python3-yaml (PyYAML) is required."; \
	  echo "       Install: pip3 install --user pyyaml"; \
	  exit 1; \
	}

# ---------- Build ----------

# Per-service stamp rules. Each `<svc>.built` stamp depends on every file
# under that service's source directory plus go.mod (the shared module
# definition every Dockerfile copies in). Make naturally rebuilds only the
# services whose sources actually changed; an unchanged tree turns into
# a millisecond-scale no-op.

$(STAMP_DIR):
	@mkdir -p $@

$(STAMP_DIR)/agent.built: $(shell find agent -type f) go.mod | $(STAMP_DIR)
	@echo "[*] Building $(AGENT_IMAGE) ..."
	$(CONTAINER_RUNTIME) build -f agent/Dockerfile -t $(AGENT_IMAGE) .
	@touch $@

$(STAMP_DIR)/email-server.built: $(shell find email-server -type f) go.mod | $(STAMP_DIR)
	@echo "[*] Building $(EMAIL_IMAGE) ..."
	$(CONTAINER_RUNTIME) build -f email-server/Dockerfile -t $(EMAIL_IMAGE) .
	@touch $@

$(STAMP_DIR)/evil-server.built: $(shell find evil-server -type f) go.mod | $(STAMP_DIR)
	@echo "[*] Building $(EVIL_IMAGE) ..."
	$(CONTAINER_RUNTIME) build -f evil-server/Dockerfile -t $(EVIL_IMAGE) .
	@touch $@

build-images: $(STAMP_DIR)/agent.built $(STAMP_DIR)/email-server.built $(STAMP_DIR)/evil-server.built  ## Build demo images (rebuilds only when sources change)

build-abctl:  ## Build abctl into /tmp (rebuilds when source is newer; used by show-result)
	@ABCTL_SRC_DIR=../../cmd/abctl; \
	if [ -x /tmp/abctl-ibac-demo ]; then \
	  NEWER=$$(find $$ABCTL_SRC_DIR -name '*.go' -newer /tmp/abctl-ibac-demo -print -quit 2>/dev/null); \
	  if [ -z "$$NEWER" ]; then \
	    echo "[*] /tmp/abctl-ibac-demo is up to date with the abctl source; skipping. \`rm /tmp/abctl-ibac-demo\` to force rebuild."; \
	    exit 0; \
	  fi; \
	  echo "[*] abctl source has been modified since /tmp/abctl-ibac-demo was built — rebuilding ..."; \
	else \
	  echo "[*] Building abctl into /tmp/abctl-ibac-demo ..."; \
	fi; \
	cd $$ABCTL_SRC_DIR && go build -o /tmp/abctl-ibac-demo .

# load-images reloads each image when EITHER:
#   - the kind node doesn't have it under docker.io/library/<name>, OR
#   - the .built stamp is newer than the .loaded stamp (rebuilt this run
#     or in a prior session that didn't get to deploy).
# Both conditions independently fire a reload, which keeps the target
# self-healing across cluster recreation, manual `crictl rmi`, and
# rebuilds — without any flag.
load-images: build-images | $(STAMP_DIR)  ## Load demo images into kind (reloads when node is missing the image or sources were rebuilt)
	@rm -f $(LOAD_MARKER)
	@for spec in agent=$(AGENT_IMAGE) email-server=$(EMAIL_IMAGE) evil-server=$(EVIL_IMAGE); do \
	  svc=$${spec%%=*}; img=$${spec#*=}; \
	  built=$(STAMP_DIR)/$$svc.built; \
	  loaded=$(STAMP_DIR)/$$svc.loaded; \
	  in_node=no; \
	  $(CONTAINER_RUNTIME) exec $(KIND_NODE) ctr -n k8s.io images list -q 2>/dev/null \
	    | grep -qx "docker.io/library/$$img" && in_node=yes; \
	  if [ "$$in_node" = yes ] && [ -f "$$loaded" ] && [ ! "$$loaded" -ot "$$built" ]; then \
	    echo "[*] $$img already in node and up-to-date — skip"; \
	    continue; \
	  fi; \
	  echo "[*] Loading $$img into kind"; \
	  kind load docker-image "$$img" --name $(KIND_CLUSTER_NAME); \
	  $(CONTAINER_RUNTIME) exec $(KIND_NODE) ctr -n k8s.io images tag \
	    localhost/$$img docker.io/library/$$img >/dev/null 2>&1 || true; \
	  touch "$$loaded" $(LOAD_MARKER); \
	done

# ---------- Deploy + patch ----------

deploy:  ## Apply manifests; rollout-restart only if load-images actually loaded something
	kubectl apply -f k8s/email-server.yaml
	kubectl apply -f k8s/evil-server.yaml
	kubectl apply -f k8s/agent.yaml
	@if [ -f $(LOAD_MARKER) ]; then \
	  echo "[*] Image content changed — rolling restart"; \
	  kubectl -n $(NAMESPACE) rollout restart deploy/ibac-email-server deploy/ibac-evil-server deploy/email-agent; \
	  rm -f $(LOAD_MARKER); \
	else \
	  echo "[*] No image changes this run — skip rollout restart."; \
	fi
	$(MAKE) wait-pods

wait-pods:  ## Wait for pods + operator-rendered authbridge ConfigMap
	@echo "[*] Waiting for ibac-email-server and ibac-evil-server ..."
	kubectl -n $(NAMESPACE) wait --for=condition=Available --timeout=120s \
	  deploy/ibac-email-server deploy/ibac-evil-server
	@echo "[*] Waiting for the operator to admit the agent pod and create the authbridge ConfigMap ..."
	@for i in $$(seq 1 60); do \
	  if kubectl -n $(NAMESPACE) get configmap authbridge-config-$(AGENT_NAME) >/dev/null 2>&1; then \
	    echo "    ✓ authbridge-config-$(AGENT_NAME) present"; \
	    break; \
	  fi; \
	  sleep 2; \
	done
	@kubectl -n $(NAMESPACE) get configmap authbridge-config-$(AGENT_NAME) >/dev/null 2>&1 || { \
	  echo "ERROR: operator did not create authbridge-config-$(AGENT_NAME) within 120s"; \
	  echo "       Check the agent pod state:"; \
	  kubectl -n $(NAMESPACE) get pods -l app.kubernetes.io/name=$(AGENT_NAME); \
	  exit 1; \
	}

patch-config:  ## Add a2a-parser + ibac to the rendered authbridge ConfigMap (and wait for hot-reload)
	bash scripts/patch-ibac-config.sh $(NAMESPACE) $(AGENT_NAME)

# ---------- Top-level demo + result rendering ----------

demo-ibac: preflight build-images load-images deploy patch-config  ## Build, load, deploy the demo end-to-end and print next-steps banner
	@echo
	@echo "╔══════════════════════════════════════════════════════════╗"
	@echo "║ ✅ IBAC demo agent ready                                 ║"
	@echo "║                                                          ║"
	@echo "║ Open the kagenti UI:                                     ║"
	@echo "║   http://kagenti-ui.localtest.me:8080                    ║"
	@echo "║                                                          ║"
	@echo "║ Pick email-agent from the Agents list, then chat:        ║"
	@echo "║                                                          ║"
	@echo "║   \"Summarize my emails.\"                                 ║"
	@echo "║                                                          ║"
	@echo "║ The agent will attempt the email-poison attack and IBAC  ║"
	@echo "║ will block the exfiltration. The chat response relays    ║"
	@echo "║ the platform's verbatim 403 body — including the LLM     ║"
	@echo "║ judge's reasoning — without any agent-fabricated framing.║"
	@echo "║                                                          ║"
	@echo "║ Then run:                                                ║"
	@echo "║                                                          ║"
	@echo "║   make show-result                                       ║"
	@echo "║                                                          ║"
	@echo "║ to see the pipeline-level forensic.                      ║"
	@echo "╚══════════════════════════════════════════════════════════╝"

show-result: build-abctl  ## Launch abctl against the agent's session API (interactive TUI)
	bash scripts/launch-abctl.sh $(NAMESPACE) $(AGENT_NAME)

# ---------- Cleanup ----------

undeploy:  ## Remove demo resources from team1 (kagenti install untouched)
	-kubectl -n $(NAMESPACE) delete -f k8s/agent.yaml --ignore-not-found
	-kubectl -n $(NAMESPACE) delete -f k8s/email-server.yaml --ignore-not-found
	-kubectl -n $(NAMESPACE) delete -f k8s/evil-server.yaml --ignore-not-found
	-kubectl -n $(NAMESPACE) delete configmap authbridge-config-$(AGENT_NAME) --ignore-not-found
	-kubectl -n $(NAMESPACE) delete secret -l kagenti.io/client-name=$(AGENT_NAME) --ignore-not-found
	@rm -rf $(STAMP_DIR)
	@echo "[*] Demo resources removed from $(NAMESPACE)."
	@echo "    The kagenti install + the namespace itself are untouched."

# ---------- Diagnostics ----------

logs-agent:  ## Tail agent + authbridge sidecar logs
	@echo "--- agent ---"
	kubectl -n $(NAMESPACE) logs deploy/$(AGENT_NAME) -c agent --tail=100
	@echo "--- authbridge ---"
	kubectl -n $(NAMESPACE) logs deploy/$(AGENT_NAME) -c authbridge-proxy --tail=100

logs-evil:  ## Tail evil-server logs (empty when IBAC is working)
	kubectl -n $(NAMESPACE) logs deploy/ibac-evil-server --tail=50
