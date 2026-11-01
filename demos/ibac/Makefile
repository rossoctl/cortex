# IBAC demo Makefile — the email agent with and without IBAC, side by side.
#
# End-to-end:
#
#   make demo-ibac          # build + load demo images, deploy both agents, patch
#   make show-result        # agentop on the cluster (one terminal per agent)
#   make chat               # the attack, IBAC on: blocked
#   make chat-unprotected   # the attack, IBAC off: inbox exfiltrated
#   make undeploy           # remove the demo's resources from team1
#
# IBAC ships in the default authbridge image pulled by the rossoctl chart,
# so this demo only needs to enable the plugin in the rendered ConfigMap.
#
# Prerequisites:
#   - rossoctl installed on a kind cluster (this is the operator-flow
#     demo; for plain-kind testing of the IBAC plugin, see commit history
#     for the standalone variant)
#   - ollama on the host with llama3.2:3b pulled
#   - kubectl, kind, podman/docker, python3 + PyYAML

.PHONY: help build-images build-agentop load-images deploy \
        patch-config demo-ibac show-result undeploy restart \
        chat chat-unprotected \
        logs-agent logs-evil preflight wait-pods

# `make` with no target prints help — easier discoverability than the
# walls-of-comments at the top.
.DEFAULT_GOAL := help

help:  ## Show this menu (default)
	@printf "\nIBAC demo — operator-injected agent in rossoctl's team1 namespace.\n\n"
	@printf "Typical run:\n"
	@printf "  \033[1mmake demo-ibac\033[0m          deploy both agents\n"
	@printf "  \033[1mmake show-result\033[0m        agentop (run it in two terminals, one per agent)\n"
	@printf "  \033[1mmake chat\033[0m               the attack, IBAC on: blocked\n"
	@printf "  \033[1mmake chat-unprotected\033[0m   the attack, IBAC off: inbox exfiltrated\n"
	@printf "  \033[1mmake logs-evil\033[0m          what reached the attacker\n"
	@printf "  \033[1mmake undeploy\033[0m\n\n"
	@printf "Targets:\n"
	@awk 'BEGIN { FS = ":.*## " } \
	     /^[a-zA-Z_-]+:.*## / { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 }' \
	     $(MAKEFILE_LIST)
	@printf "\nVariables:\n"
	@printf "  \033[36m%-18s\033[0m %s\n" "KIND_CLUSTER_NAME" "kind cluster to load images into (default: rossoctl)"
	@printf "  \033[36m%-18s\033[0m %s\n" "NAMESPACE"         "agent namespace (default: team1)"
	@printf "  \033[36m%-18s\033[0m %s\n" "AGENT_NAME"        "deployment name (default: email-agent)"
	@printf "  \033[36m%-18s\033[0m %s\n" "CONTAINER_RUNTIME" "podman or docker (auto-detected)"
	@printf "\nPrerequisites: rossoctl install on a kind cluster, ollama on the host\n"
	@printf "with llama3.2:3b, kubectl/kind/podman, python3 + PyYAML.\n\n"

KIND_CLUSTER_NAME ?= rossoctl
KIND_NODE         ?= $(KIND_CLUSTER_NAME)-control-plane
NAMESPACE         ?= team1
AGENT_NAME        ?= email-agent
# The unprotected twin: same agent and sidecar, never patched with IBAC.
TWIN_NAME         := email-agent-unprotected

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

preflight:  ## Verify rossoctl is installed + tools are available

	@kubectl get ns rossoctl-system >/dev/null 2>&1 || { \
	  echo "ERROR: namespace 'rossoctl-system' not found — rossoctl doesn't appear to be installed."; \
	  echo "       Install rossoctl first (see https://github.com/rossoctl/rossoctl/blob/main/docs/operate/install-kubernetes.md)"; \
	  exit 1; \
	}
	@kubectl get ns $(NAMESPACE) >/dev/null 2>&1 || { \
	  echo "ERROR: namespace '$(NAMESPACE)' not found — rossoctl's agent namespace is missing."; \
	  echo "       Did the rossoctl install complete? Try: kubectl get ns | grep team"; \
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

build-agentop:  ## Build agentop into /tmp (rebuilds when source is newer; used by show-result)
	@AGENTOP_SRC_DIR=../../cmd/agentop; \
	if [ -x /tmp/agentop-ibac-demo ]; then \
	  NEWER=$$(find $$AGENTOP_SRC_DIR -name '*.go' -newer /tmp/agentop-ibac-demo -print -quit 2>/dev/null); \
	  if [ -z "$$NEWER" ]; then \
	    echo "[*] /tmp/agentop-ibac-demo is up to date with the agentop source; skipping. \`rm /tmp/agentop-ibac-demo\` to force rebuild."; \
	    exit 0; \
	  fi; \
	  echo "[*] agentop source has been modified since /tmp/agentop-ibac-demo was built — rebuilding ..."; \
	else \
	  echo "[*] Building agentop into /tmp/agentop-ibac-demo ..."; \
	fi; \
	cd $$AGENTOP_SRC_DIR && go build -o /tmp/agentop-ibac-demo .

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
	kubectl apply -f k8s/agent-unprotected.yaml
	@if [ -f $(LOAD_MARKER) ]; then \
	  echo "[*] Image content changed — rolling restart"; \
	  kubectl -n $(NAMESPACE) rollout restart deploy/ibac-email-server deploy/ibac-evil-server deploy/email-agent deploy/$(TWIN_NAME); \
	  rm -f $(LOAD_MARKER); \
	else \
	  echo "[*] No image changes this run — skip rollout restart."; \
	fi
	$(MAKE) wait-pods

wait-pods:  ## Wait for pods + operator-rendered authbridge ConfigMap
	@echo "[*] Waiting for ibac-email-server and ibac-evil-server ..."
	kubectl -n $(NAMESPACE) wait --for=condition=Available --timeout=120s \
	  deploy/ibac-email-server deploy/ibac-evil-server
	@echo "[*] Waiting for the operator to admit the agent pods and create their authbridge ConfigMaps ..."
	@for agent in $(AGENT_NAME) $(TWIN_NAME); do \
	  kubectl -n $(NAMESPACE) rollout status deploy/$$agent --timeout=180s || exit 1; \
	  for i in $$(seq 1 60); do \
	    if kubectl -n $(NAMESPACE) get configmap authbridge-config-$$agent >/dev/null 2>&1; then \
	      echo "    ✓ authbridge-config-$$agent present"; \
	      break; \
	    fi; \
	    sleep 2; \
	  done; \
	  kubectl -n $(NAMESPACE) get configmap authbridge-config-$$agent >/dev/null 2>&1 || { \
	    echo "ERROR: operator did not create authbridge-config-$$agent within 120s"; \
	    echo "       Check the agent pod state:"; \
	    kubectl -n $(NAMESPACE) get pods -l app.kubernetes.io/name=$$agent; \
	    exit 1; \
	  }; \
	done

# The operator re-renders authbridge-config-<agent> every time it admits the
# agent's pod, so this must run after the last pod (re)start — `make restart`
# does both. The twin gets the same parsers without ibac, so agentop shows
# the two agents' traffic the same way and they differ only where IBAC acts.
patch-config:  ## Add the IBAC pipeline to email-agent and the parsers alone to the twin (waits for hot-reload)
	bash scripts/patch-ibac-config.sh $(NAMESPACE) $(AGENT_NAME)
	bash scripts/patch-ibac-config.sh $(NAMESPACE) $(TWIN_NAME) k8s/parsers-patch.yaml

restart:  ## Restart both agents, then re-patch (after a cluster restart, or when a sidecar hangs)
	kubectl -n $(NAMESPACE) rollout restart deploy/$(AGENT_NAME) deploy/$(TWIN_NAME)
	$(MAKE) wait-pods patch-config

# ---------- Chat ----------

chat:  ## Send "Summarize my emails." to email-agent (IBAC blocks the exfiltration)
	bash scripts/chat.sh $(NAMESPACE) $(AGENT_NAME) with-ibac-$$(date +%H%M%S)

chat-unprotected:  ## Send the same message to email-agent-unprotected (the inbox reaches ibac-evil-server)
	bash scripts/chat.sh $(NAMESPACE) $(TWIN_NAME) no-ibac-$$(date +%H%M%S)

# ---------- Top-level demo + result rendering ----------

demo-ibac: preflight build-images load-images deploy patch-config  ## Build, load, deploy both agents end to end and print the walkthrough
	@echo
	@echo "✅ IBAC demo ready: email-agent (IBAC on) and email-agent-unprotected (IBAC off)."
	@echo
	@echo "  1. Terminal A:  make show-result   → team1 → email-agent-…"
	@echo "     Terminal B:  make show-result   → team1 → email-agent-unprotected-…"
	@echo "  2. Terminal C:  make chat              # blocked: 'Tool call blocked by platform'"
	@echo "                  make chat-unprotected  # an ordinary summary"
	@echo "  3. In A and B, Enter on the new session (with-ibac-… / no-ibac-…):"
	@echo "     the ibac-evil-server row is 'deny ibac 403' in A, '— 200' in B."
	@echo "  4. make logs-evil   # the inbox, staging password included, from B only"
	@echo
	@echo "Walkthrough and troubleshooting: README.md"

show-result: build-agentop  ## Launch agentop on the cluster: pick team1, then an agent pod
	bash scripts/launch-agentop.sh $(NAMESPACE) $(AGENT_NAME)

# ---------- Cleanup ----------

undeploy:  ## Remove demo resources from team1 (rossoctl install untouched)
	-kubectl -n $(NAMESPACE) delete pod ibac-chat --ignore-not-found
	-kubectl -n $(NAMESPACE) delete -f k8s/agent.yaml --ignore-not-found
	-kubectl -n $(NAMESPACE) delete -f k8s/agent-unprotected.yaml --ignore-not-found
	-kubectl -n $(NAMESPACE) delete -f k8s/email-server.yaml --ignore-not-found
	-kubectl -n $(NAMESPACE) delete -f k8s/evil-server.yaml --ignore-not-found
	-kubectl -n $(NAMESPACE) delete configmap authbridge-config-$(AGENT_NAME) --ignore-not-found
	-kubectl -n $(NAMESPACE) delete secret -l rossoctl.io/client-name=$(AGENT_NAME) --ignore-not-found
	-kubectl -n $(NAMESPACE) delete configmap authbridge-config-$(TWIN_NAME) --ignore-not-found
	-kubectl -n $(NAMESPACE) delete secret -l rossoctl.io/client-name=$(TWIN_NAME) --ignore-not-found
	@rm -rf $(STAMP_DIR)
	@echo "[*] Demo resources removed from $(NAMESPACE)."
	@echo "    The rossoctl install + the namespace itself are untouched."

# ---------- Diagnostics ----------

logs-agent:  ## Tail agent + authbridge sidecar logs
	@echo "--- agent ---"
	kubectl -n $(NAMESPACE) logs deploy/$(AGENT_NAME) -c agent --tail=100
	@echo "--- authbridge ---"
	kubectl -n $(NAMESPACE) logs deploy/$(AGENT_NAME) -c authbridge-proxy --tail=100

logs-evil:  ## Tail evil-server logs (empty after email-agent; the inbox after email-agent-unprotected)
	kubectl -n $(NAMESPACE) logs deploy/ibac-evil-server --tail=50
