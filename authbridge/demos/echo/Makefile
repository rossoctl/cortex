# Echo demo Makefile — credential placeholder-swap, kagenti-UI flow.
#
# End-to-end:
#
#   make demo-echo     # build sidecar + demo images, deploy, keycloak, patch, banner
#   make show-result   # tail upstream logs (ground-truth echoed credential)
#   make undeploy      # remove the demo's resources from team1
#
# Unlike the IBAC demo, this demo needs a freshly-built authbridge sidecar
# image: credential placeholder swap is a new authbridge capability, so we
# build the proxy from the repo root and point the kagenti platform config
# at the local tag before deploying the agent.
#
# Prerequisites:
#   - kagenti installed on a kind cluster (this is the operator-flow demo)
#   - kubectl, kind, podman/docker, python3 + PyYAML
#   - python-keycloak (for setup-keycloak; pip install python-keycloak)

.PHONY: help preflight build-sidecar load-sidecar override-sidecar-image \
        build-images load-images deploy wait-pods setup-keycloak \
        patch-config demo-echo show-result undeploy \
        logs-agent logs-upstream

# `make` with no target prints help — easier discoverability than the
# walls-of-comments at the top.
.DEFAULT_GOAL := help

help:  ## Show this menu (default)
	@printf "\nEcho demo — credential placeholder-swap, operator-injected agent in team1.\n\n"
	@printf "Typical run:\n"
	@printf "  \033[1mmake demo-echo\033[0m\n"
	@printf "  ... chat with echo-agent in the kagenti UI ...\n"
	@printf "  \033[1mmake show-result\033[0m\n"
	@printf "  \033[1mmake undeploy\033[0m\n\n"
	@printf "Targets:\n"
	@awk 'BEGIN { FS = ":.*## " } \
	     /^[a-zA-Z_-]+:.*## / { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 }' \
	     $(MAKEFILE_LIST)
	@printf "\nVariables:\n"
	@printf "  \033[36m%-22s\033[0m %s\n" "KIND_CLUSTER_NAME" "kind cluster to load images into (default: kagenti)"
	@printf "  \033[36m%-22s\033[0m %s\n" "NAMESPACE"         "agent namespace (default: team1)"
	@printf "  \033[36m%-22s\033[0m %s\n" "AGENT_NAME"        "deployment name (default: echo-agent)"
	@printf "  \033[36m%-22s\033[0m %s\n" "SIDECAR_IMAGE"     "authbridge sidecar tag (default: authbridge:placeholder-dev)"
	@printf "  \033[36m%-22s\033[0m %s\n" "CONTAINER_RUNTIME" "podman or docker (auto-detected)"
	@printf "\nPrerequisites: kagenti install on a kind cluster, kubectl/kind/podman,\n"
	@printf "python3 + PyYAML, and python-keycloak for setup-keycloak.\n\n"

KIND_CLUSTER_NAME ?= kagenti
KIND_NODE         ?= $(KIND_CLUSTER_NAME)-control-plane
NAMESPACE         ?= team1
AGENT_NAME        ?= echo-agent

# Authbridge sidecar image built from the repo root (the placeholder-swap
# capability is new, so the demo overrides the platform's default sidecar
# image with this locally-built tag).
SIDECAR_IMAGE     ?= authbridge:placeholder-dev

# Container runtime auto-detect.
CONTAINER_RUNTIME ?= $(shell command -v podman >/dev/null 2>&1 && echo podman || echo docker)

# Demo-image tags (local-only, not pushed).
AGENT_IMAGE       := echo-agent:latest
UPSTREAM_IMAGE    := echo-upstream:latest

# Repo root (demos/echo -> demos -> authbridge). The sidecar Dockerfile
# lives at cmd/authbridge-proxy/Dockerfile and builds with the repo root
# as context.
REPO_ROOT         := $(abspath ../..)

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

# ---------- Sidecar (authbridge proxy) ----------

build-sidecar:  ## Build the authbridge sidecar image from the repo root
	@echo "[*] Building $(SIDECAR_IMAGE) from $(REPO_ROOT) ..."
	$(CONTAINER_RUNTIME) build -f $(REPO_ROOT)/cmd/authbridge-proxy/Dockerfile -t $(SIDECAR_IMAGE) $(REPO_ROOT)

load-sidecar:  ## Load the sidecar image into the kind cluster
	@echo "[*] Loading $(SIDECAR_IMAGE) into kind cluster $(KIND_CLUSTER_NAME) ..."
	kind load docker-image $(SIDECAR_IMAGE) --name $(KIND_CLUSTER_NAME)
	@# With the podman provider, kind leaves the image on the node as
	@# localhost/<img>; the operator references the bare tag (docker.io/library/<img>),
	@# so tag it there too or IfNotPresent will try (and fail) to pull. Mirrors load-images.
	@$(CONTAINER_RUNTIME) exec $(KIND_NODE) ctr -n k8s.io images tag \
	    localhost/$(SIDECAR_IMAGE) docker.io/library/$(SIDECAR_IMAGE) >/dev/null 2>&1 || true

# Point the kagenti platform config at our locally-built sidecar tag so
# the operator's injecting webhook uses it for echo-agent. We edit the
# `images.authbridge` key inside the config.yaml of the
# `kagenti-platform-config` ConfigMap (kagenti-system), re-apply it, and
# roll the controller so the new value is picked up.
override-sidecar-image:  ## Point kagenti's platform config at the local sidecar image
	@echo "[*] Overriding images.authbridge -> $(SIDECAR_IMAGE) in kagenti-platform-config ..."
	@CURRENT=$$(kubectl -n kagenti-system get configmap kagenti-platform-config \
	    -o jsonpath='{.data.config\.yaml}'); \
	if [ -z "$$CURRENT" ]; then \
	  echo "ERROR: could not read config.yaml from kagenti-platform-config (kagenti-system)."; \
	  echo "       Is kagenti installed? kubectl -n kagenti-system get cm kagenti-platform-config"; \
	  exit 1; \
	fi; \
	MERGED=$$(printf '%s' "$$CURRENT" | SIDECAR_IMAGE='$(SIDECAR_IMAGE)' python3 -c 'import os, sys, yaml; \
c = yaml.safe_load(sys.stdin) or {}; \
c.setdefault("images", {})["authbridge"] = os.environ["SIDECAR_IMAGE"]; \
sys.stdout.write(yaml.safe_dump(c, default_flow_style=False, sort_keys=False))'); \
	TMP=$$(mktemp); printf '%s' "$$MERGED" >"$$TMP"; \
	kubectl -n kagenti-system create configmap kagenti-platform-config \
	    --from-file=config.yaml="$$TMP" --dry-run=client -o yaml | kubectl apply -f -; \
	rm -f "$$TMP"
	@echo "[*] Restarting the kagenti controller to pick up the new sidecar image ..."
	kubectl -n kagenti-system rollout restart deploy/kagenti-controller-manager
	kubectl -n kagenti-system rollout status deploy/kagenti-controller-manager --timeout=120s

# ---------- Build demo images ----------

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

$(STAMP_DIR)/upstream.built: $(shell find upstream -type f) go.mod | $(STAMP_DIR)
	@echo "[*] Building $(UPSTREAM_IMAGE) ..."
	$(CONTAINER_RUNTIME) build -f upstream/Dockerfile -t $(UPSTREAM_IMAGE) .
	@touch $@

build-images: $(STAMP_DIR)/agent.built $(STAMP_DIR)/upstream.built  ## Build demo images (rebuilds only when sources change)

# load-images reloads each image when EITHER:
#   - the kind node doesn't have it under docker.io/library/<name>, OR
#   - the .built stamp is newer than the .loaded stamp (rebuilt this run
#     or in a prior session that didn't get to deploy).
# Both conditions independently fire a reload, which keeps the target
# self-healing across cluster recreation, manual `crictl rmi`, and
# rebuilds — without any flag.
load-images: build-images | $(STAMP_DIR)  ## Load demo images into kind (reloads when node is missing the image or sources were rebuilt)
	@rm -f $(LOAD_MARKER)
	@for spec in agent=$(AGENT_IMAGE) upstream=$(UPSTREAM_IMAGE); do \
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
	kubectl apply -f k8s/agent.yaml
	kubectl apply -f k8s/upstream.yaml
	@if [ -f $(LOAD_MARKER) ]; then \
	  echo "[*] Image content changed — rolling restart"; \
	  kubectl -n $(NAMESPACE) rollout restart deploy/echo-agent deploy/echo-upstream; \
	  rm -f $(LOAD_MARKER); \
	else \
	  echo "[*] No image changes this run — skip rollout restart."; \
	fi
	$(MAKE) wait-pods

wait-pods:  ## Wait for pods + operator-rendered authbridge ConfigMap
	@echo "[*] Waiting for echo-agent and echo-upstream ..."
	kubectl -n $(NAMESPACE) wait --for=condition=Available --timeout=120s \
	  deploy/echo-agent deploy/echo-upstream
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

# setup-keycloak configures the realm clients/scopes/users for the demo.
# It talks to Keycloak over HTTP; on a kind install the in-cluster service
# is reachable at http://keycloak.localtest.me:8080 (default). If you
# can't reach that, port-forward in another terminal:
#   kubectl port-forward service/keycloak-service -n keycloak 8080:8080
# and set KEYCLOAK_URL=http://localhost:8080.
# Requires the python-keycloak package: pip install python-keycloak
setup-keycloak:  ## Configure Keycloak (echo-upstream client, scopes, demo users)
	@python3 -c 'import keycloak' 2>/dev/null || { \
	  echo "ERROR: python-keycloak is required for setup-keycloak."; \
	  echo "       Install: pip install python-keycloak"; \
	  exit 1; \
	}
	python3 scripts/setup_keycloak.py --namespace $(NAMESPACE) --service-account $(AGENT_NAME)

patch-config:  ## Enable placeholder-swap + echo-upstream route in the rendered authbridge ConfigMap (and wait for hot-reload)
	bash scripts/patch-echo-config.sh $(NAMESPACE) $(AGENT_NAME)

# ---------- Top-level demo + result rendering ----------

demo-echo: preflight build-sidecar load-sidecar override-sidecar-image build-images load-images deploy wait-pods setup-keycloak patch-config  ## Build, load, deploy the demo end-to-end and print next-steps banner
	@echo
	@echo "╔══════════════════════════════════════════════════════════╗"
	@echo "║ ✅ Echo placeholder-swap demo agent ready                ║"
	@echo "║                                                          ║"
	@echo "║ Open the kagenti UI:                                     ║"
	@echo "║   http://kagenti-ui.localtest.me:8080                    ║"
	@echo "║                                                          ║"
	@echo "║ Log in as alice / alice123 (log out + back in if you    ║"
	@echo "║ were already signed in — new audience scope).            ║"
	@echo "║                                                          ║"
	@echo "║ Pick echo-agent from the Agents list, then chat:         ║"
	@echo "║                                                          ║"
	@echo "║   \"echo\"                                                 ║"
	@echo "║                                                          ║"
	@echo "║ The reply shows two lines:                               ║"
	@echo "║   - inbound  : an abph_... placeholder (what the agent   ║"
	@echo "║                received from jwt-validation)              ║"
	@echo "║   - outbound : the exchanged token echo-upstream saw     ║"
	@echo "║                (aud=echo-upstream)                        ║"
	@echo "║                                                          ║"
	@echo "║ Ground-truth check:                                      ║"
	@echo "║   make show-result                                       ║"
	@echo "╚══════════════════════════════════════════════════════════╝"

show-result:  ## Tail echo-upstream logs (the exchanged credential it received)
	@echo "--- echo-upstream (last 50 lines) ---"
	kubectl -n $(NAMESPACE) logs deploy/echo-upstream --tail=50

# ---------- Cleanup ----------

undeploy:  ## Remove demo resources from team1 (kagenti install untouched)
	-kubectl -n $(NAMESPACE) delete -f k8s/agent.yaml --ignore-not-found
	-kubectl -n $(NAMESPACE) delete -f k8s/upstream.yaml --ignore-not-found
	-kubectl -n $(NAMESPACE) delete agentruntime echo-agent --ignore-not-found
	@echo "[*] Note: the operator-created ConfigMap authbridge-config-$(AGENT_NAME)"
	@echo "    may linger after the agent is gone. Remove manually if needed:"
	@echo "      kubectl -n $(NAMESPACE) delete configmap authbridge-config-$(AGENT_NAME)"
	@rm -rf $(STAMP_DIR)
	@echo "[*] Demo resources removed from $(NAMESPACE)."
	@echo "    The kagenti install + the namespace itself are untouched."

# ---------- Diagnostics ----------

logs-agent:  ## Tail agent + authbridge sidecar logs
	@echo "--- agent ---"
	kubectl -n $(NAMESPACE) logs deploy/$(AGENT_NAME) -c agent --tail=100
	@echo "--- authbridge ---"
	kubectl -n $(NAMESPACE) logs deploy/$(AGENT_NAME) -c authbridge-proxy --tail=100

logs-upstream:  ## Tail echo-upstream logs
	kubectl -n $(NAMESPACE) logs deploy/echo-upstream --tail=100
