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
	@printf "  \033[36m%-18s\033[0m %s\n" "REBUILD=1"         "force rebuild even when images are present locally"
	@printf "  \033[36m%-18s\033[0m %s\n" "RELOAD=1"          "force kind-load all demo images (default: only rebuilt ones)"
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

# Marker file: build-images truncates this at the start, appends an image
# name when it actually builds. load-images and deploy read it to know
# whether to (re)load and rollout-restart. Removed at end of deploy.
# Demo runs are scoped per (cluster, namespace, agent) to keep parallel
# runs from stomping each other.
CHANGED_MARK := /tmp/.ibac-demo-changed-$(KIND_CLUSTER_NAME)-$(NAMESPACE)-$(AGENT_NAME)

# Image-spec list reused by build-images. Format is `<service-dir>=<image-tag>`.
# Uses `=` as separator because the image-tag itself contains `:`.
DEMO_IMAGE_SPECS := agent=$(AGENT_IMAGE) email-server=$(EMAIL_IMAGE) evil-server=$(EVIL_IMAGE)

build-images:  ## Build demo images (skip if present locally; REBUILD=1 forces)
	@: > $(CHANGED_MARK)
	@for spec in $(DEMO_IMAGE_SPECS); do \
	  svc=$${spec%%=*}; img=$${spec#*=}; \
	  if [ -z "$(REBUILD)" ] && $(CONTAINER_RUNTIME) image inspect "$$img" >/dev/null 2>&1; then \
	    echo "[*] $$img already built — skip (REBUILD=1 to force)"; \
	    continue; \
	  fi; \
	  echo "[*] Building $$img ..."; \
	  $(CONTAINER_RUNTIME) build -f $$svc/Dockerfile -t "$$img" . || exit 1; \
	  echo "$$img" >>$(CHANGED_MARK); \
	done

build-abctl:  ## Build abctl into /tmp (cached, used by show-result)
	@if [ -x /tmp/abctl-ibac-demo ]; then \
	  echo "[*] /tmp/abctl-ibac-demo already built; skipping. \`rm /tmp/abctl-ibac-demo\` to force rebuild."; \
	else \
	  echo "[*] Building abctl into /tmp/abctl-ibac-demo ..."; \
	  cd ../../cmd/abctl && go build -o /tmp/abctl-ibac-demo . ; \
	fi

# Resolve the list of images to (re)load:
#   - If RELOAD=1 was set, force all three.
#   - Else if the marker file from build-images exists, load only what
#     it lists (empty list = nothing rebuilt = nothing to load).
#   - Else (user invoked load-images directly, no preceding build), load
#     all three to preserve the pre-refactor behavior.
LOAD_IMAGES = $(shell \
  if [ -n "$(RELOAD)" ]; then \
    echo "$(AGENT_IMAGE) $(EMAIL_IMAGE) $(EVIL_IMAGE)"; \
  elif [ -f $(CHANGED_MARK) ]; then \
    cat $(CHANGED_MARK); \
  else \
    echo "$(AGENT_IMAGE) $(EMAIL_IMAGE) $(EVIL_IMAGE)"; \
  fi)

load-images:  ## Load demo images into kind (skip when build was a no-op; RELOAD=1 forces all)
	@imgs="$(LOAD_IMAGES)"; \
	if [ -z "$$imgs" ]; then \
	  echo "[*] No images need (re)loading — skipping kind load."; \
	  exit 0; \
	fi; \
	for img in $$imgs; do \
	  echo "[*] Loading $$img into kind"; \
	  kind load docker-image "$$img" --name $(KIND_CLUSTER_NAME); \
	  echo "$$img" >>$(CHANGED_MARK); \
	done
ifeq ($(CONTAINER_RUNTIME),podman)
	@imgs="$(LOAD_IMAGES)"; \
	for img in $$imgs; do \
	  $(CONTAINER_RUNTIME) exec $(KIND_NODE) ctr --namespace=k8s.io images tag localhost/$$img docker.io/library/$$img 2>/dev/null || true; \
	done
endif

# ---------- Deploy + patch ----------

deploy:  ## Apply manifests; rollout-restart only if images changed this run
	kubectl apply -f k8s/email-server.yaml
	kubectl apply -f k8s/evil-server.yaml
	kubectl apply -f k8s/agent.yaml
	@# Restart only when build-images / load-images actually did work.
	@# The marker file is truncated by build-images and appended-to when
	@# images are rebuilt or reloaded. If it's missing entirely, we
	@# assume a standalone `make deploy` invocation and play it safe by
	@# restarting (preserves pre-idempotency-refactor behavior).
	@if [ ! -f $(CHANGED_MARK) ]; then \
	  echo "[*] Marker file absent — assuming standalone deploy; rolling restart"; \
	  kubectl -n $(NAMESPACE) rollout restart deploy/ibac-email-server deploy/ibac-evil-server deploy/email-agent; \
	elif [ -s $(CHANGED_MARK) ]; then \
	  echo "[*] Images rebuilt/reloaded this run — rolling restart"; \
	  kubectl -n $(NAMESPACE) rollout restart deploy/ibac-email-server deploy/ibac-evil-server deploy/email-agent; \
	else \
	  echo "[*] No image changes this run — skip rollout restart."; \
	fi
	@rm -f $(CHANGED_MARK)
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
	@rm -f $(CHANGED_MARK)
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
