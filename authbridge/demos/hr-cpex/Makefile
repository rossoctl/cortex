# CPEX demo Makefile — kind-cluster flow.
#
# End-to-end:
#
#   make deploy        # build images, load into kind, kubectl apply
#   make port-forward  # Keycloak (8081) + agent (8082) + fwd proxy (8083) + session API (9094)
#   make scenarios     # run all 9 demo curl scenarios (through the forward proxy)
#   make logs          # tail the authbridge-cpex sidecar logs
#   make undeploy      # delete the cpex-demo namespace
#
# Prerequisites:
#   - kind cluster (we target `kagenti`; override KIND_CLUSTER)
#   - docker / podman
#   - kubectl
#   - (chat only) host Ollama on 0.0.0.0 + the right LLM_API_BASE — see the
#     "Agent LLM wiring" block below and `make ollama-check`. The curl
#     `scenarios` need NO LLM.
# All source needed for the demo lives in this directory tree
# (hr-mcp-server/, k8s/, agent/).

KIND_CLUSTER ?= kagenti
# Fixed, not overridable: the k8s/*.yaml manifests hardcode the
# `cpex-demo` namespace (00-namespace.yaml plus metadata.namespace on
# every object), so an override here would create the ConfigMaps in one
# namespace while the Deployments landed in `cpex-demo` — and the later
# `set env` / `rollout status` targets would miss them.
NAMESPACE    := cpex-demo

# Image tags — local images, loaded into kind, never pushed to a
# registry. imagePullPolicy: Never in the Deployment manifests.
AUTHBRIDGE_IMAGE ?= authbridge-cpex:dev
HR_MCP_IMAGE     ?= hr-mcp:dev
AGENT_IMAGE      ?= hr-cpex-agent:dev

# Source paths for the images. authbridge-cpex builds from the
# authbridge module (cmd/ + authlib/, two levels up from this demo
# dir); hr-mcp and the agent are vendored alongside the manifests so
# the demo is self-contained.
AUTHBRIDGE_ROOT := $(abspath $(CURDIR)/../..)
HR_MCP_SRC      ?= $(CURDIR)/hr-mcp-server
AGENT_SRC       ?= $(CURDIR)/agent

# Agent LLM wiring (injected into the deployment on `apply`). The agent's
# inference goes DIRECT to this base (never through the cpex sidecar).
#
#   LLM_API_BASE — how a cluster pod reaches the host's Ollama:
#     * Docker Desktop / kind-with-host-mappings:  http://host.docker.internal:11434  (default)
#     * Rancher Desktop (lima):                     http://192.168.5.2:11434  (lima host-gateway)
#   In BOTH cases Ollama must listen beyond loopback:  OLLAMA_HOST=0.0.0.0 (restart Ollama).
#   Override per-env, e.g.:  make deploy LLM_API_BASE=http://192.168.5.2:11434
#   MODEL — any litellm-routed model the host Ollama has pulled.
LLM_API_BASE ?= http://host.docker.internal:11434
MODEL        ?= ollama/llama3.2:3b

.PHONY: help build-images load-images apply deploy port-forward scenarios logs undeploy preflight ollama-check

.DEFAULT_GOAL := help

help:  ## Show this menu (default)
	@printf "\nCPEX demo — runs CPEX/APL policy on the kagenti AuthBridge\n"
	@printf "platform. Self-contained namespace: $(NAMESPACE).\n\n"
	@printf "Typical run:\n"
	@printf "  \033[1mmake deploy\033[0m\n"
	@printf "  \033[1mmake port-forward\033[0m   (in another terminal)\n"
	@printf "  \033[1mmake scenarios\033[0m\n\n"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[1m%-18s\033[0m %s\n", $$1, $$2}'

preflight:  ## Check that prerequisites are in place
	@command -v kind >/dev/null      || { echo "missing: kind";       exit 1; }
	@command -v kubectl >/dev/null   || { echo "missing: kubectl";    exit 1; }
	@command -v docker >/dev/null    || { echo "missing: docker";     exit 1; }
	@kind get clusters | grep -q '^$(KIND_CLUSTER)$$' || { echo "missing kind cluster: $(KIND_CLUSTER)"; exit 1; }
	@test -d "$(HR_MCP_SRC)" || { echo "missing: $(HR_MCP_SRC) (should ship in the demo dir)"; exit 1; }
	@test -d "$(AGENT_SRC)" || { echo "missing: $(AGENT_SRC) (should ship in the demo dir)"; exit 1; }
	@echo "preflight OK"

build-images: preflight  ## Build authbridge-cpex + hr-mcp + agent docker images
	@echo "[build] authbridge-cpex from $(AUTHBRIDGE_ROOT)"
	cd $(AUTHBRIDGE_ROOT) && docker build \
		-f cmd/authbridge-cpex/Dockerfile \
		--build-arg CPEX_FFI_VERSION=$$(cat cmd/authbridge-cpex/CPEX_FFI_VERSION | tr -d '[:space:]') \
		-t $(AUTHBRIDGE_IMAGE) .
	@echo "[build] hr-mcp from $(HR_MCP_SRC)"
	cd $(HR_MCP_SRC) && docker build -t $(HR_MCP_IMAGE) .
	@echo "[build] hr-cpex-agent from $(AGENT_SRC)"
	cd $(AGENT_SRC) && docker build -t $(AGENT_IMAGE) .

load-images: preflight  ## Load all three images into the kind cluster
	kind load docker-image $(AUTHBRIDGE_IMAGE) --name $(KIND_CLUSTER)
	kind load docker-image $(HR_MCP_IMAGE)     --name $(KIND_CLUSTER)
	kind load docker-image $(AGENT_IMAGE)      --name $(KIND_CLUSTER)

apply:  ## kubectl apply the manifests (assumes images already loaded)
	kubectl apply -f k8s/00-namespace.yaml
	# Recreate the realm ConfigMap from the JSON file each time so
	# realm-export.json edits take effect on `make apply`.
	kubectl create configmap keycloak-realm \
		--namespace $(NAMESPACE) \
		--from-file=cpex-demo-realm.json=k8s/realm-export.json \
		--dry-run=client -o yaml | kubectl apply -f -
	# Same dynamic ConfigMap shape for cpex-policy.yaml so YAML
	# edits propagate without rewriting a manifest.
	kubectl create configmap cpex-policy \
		--namespace $(NAMESPACE) \
		--from-file=cpex.yaml=k8s/cpex-policy.yaml \
		--dry-run=client -o yaml | kubectl apply -f -
	kubectl apply -f k8s/10-keycloak.yaml
	kubectl apply -f k8s/20-hr-mcp.yaml
	kubectl apply -f k8s/30-agent.yaml
	# Inject the env-specific LLM wiring. Idempotent: when these match the
	# manifest defaults (Docker Desktop case) kubectl reports no change and
	# does not roll the pod; override LLM_API_BASE for Rancher Desktop.
	kubectl -n $(NAMESPACE) set env deployment/hr-cpex-agent -c agent \
		LLM_API_BASE='$(LLM_API_BASE)' MODEL='$(MODEL)'

deploy: build-images load-images apply  ## Full bring-up: build, load, apply
	@echo
	@echo "Waiting for rollouts to finish..."
	@# rollout status follows the new ReplicaSet, so it's correct even when the
	@# `set env` in `apply` rolls the agent pod (a plain label wait can race a
	@# terminating old pod against the new one).
	kubectl -n $(NAMESPACE) rollout status deployment/keycloak      --timeout=180s
	kubectl -n $(NAMESPACE) rollout status deployment/hr-mcp        --timeout=120s
	kubectl -n $(NAMESPACE) rollout status deployment/hr-cpex-agent --timeout=120s
	@echo
	@printf "\033[1;32mDeploy complete.\033[0m Next:\n"
	@printf "  make port-forward   # in another terminal\n"
	@printf "  make scenarios      # deterministic curl matrix (no LLM needed)\n"
	@printf "  make ollama-check   # only for the interactive chat — confirms a pod can reach Ollama\n"

port-forward:  ## Forward Keycloak (8081) + agent (8082) + fwd proxy (8083) + session API (9094)
	@echo "Port-forwarding (Ctrl+C to stop)..."
	@echo "  Keycloak admin:     http://localhost:8081  (admin/admin)"
	@echo "  Agent (A2A):        http://localhost:8082   <- chat.py talks here"
	@echo "  Sidecar fwd proxy:  http://localhost:8083   <- scenarios curl -x here"
	@echo "  CPEX session API:   http://localhost:9094   <- inspect taint/sessions"
	@# Keycloak + agent go via Service; the forward proxy (8081) and session
	@# API (9094) bind to localhost in the pod, so we port-forward those
	@# against the deployment's pod directly (8083 on the host avoids the
	@# Keycloak 8081 collision).
	kubectl -n $(NAMESPACE) port-forward svc/keycloak 8081:8080 & \
	kubectl -n $(NAMESPACE) port-forward svc/hr-cpex-agent 8082:8080 & \
	kubectl -n $(NAMESPACE) port-forward deploy/hr-cpex-agent 8083:8081 9094:9094 & \
	wait

scenarios:  ## Run all demo curl scenarios (assumes port-forward up)
	@for s in $(CURDIR)/scenarios/0[1-9]-*.sh; do \
	  PROXY=http://localhost:8083 \
	  KEYCLOAK_HOST=http://localhost:8081 \
	  "$$s"; \
	done

logs:  ## Tail the authbridge-cpex sidecar logs
	kubectl -n $(NAMESPACE) logs -f deploy/hr-cpex-agent -c authbridge-cpex

ollama-check:  ## Verify a cluster pod can reach the host Ollama at LLM_API_BASE
	@echo "Checking pod -> $(LLM_API_BASE) (the agent's inference endpoint)..."
	@kubectl -n $(NAMESPACE) exec deploy/hr-cpex-agent -c agent -- \
		python3 -c "import urllib.request,sys; \
u='$(LLM_API_BASE)'.rstrip('/')+'/api/tags'; \
print('OK', urllib.request.urlopen(u, timeout=5).status, u)" \
	  || { \
		echo; \
		echo "UNREACHABLE. Two things to check:"; \
		echo "  1) Ollama must listen beyond loopback:  OLLAMA_HOST=0.0.0.0  then restart Ollama."; \
		echo "  2) LLM_API_BASE must be the host address your cluster pods can route to:"; \
		echo "       Docker Desktop / kind-host-mappings:  http://host.docker.internal:11434"; \
		echo "       Rancher Desktop (lima):                http://192.168.5.2:11434"; \
		echo "     Re-deploy with the right one, e.g.:  make deploy LLM_API_BASE=http://192.168.5.2:11434"; \
		exit 1; \
	  }

undeploy:  ## Tear down the demo
	-kubectl delete namespace $(NAMESPACE) --wait=false
