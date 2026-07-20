# finance-sparc demo — SPARC pre-tool reflection on a regular finance agent.
#
#   make demo                 # full end-to-end, watsonx (needs WX_* env)
#   make demo PROVIDER=ollama # full end-to-end, local ollama (no creds)
#   make undeploy             # remove demo resources
#
# `make demo` is one-shot: it sets up the host (ollama reachable from the
# cluster), builds + loads images (incl. an authbridge image carrying the sparc
# plugin), deploys the SPARC service + finance MCP server + agent, enables the
# pipeline, configures Keycloak, and drives the two-turn scenario through REAL
# inbound auth (no jwt bypass), printing SPARC's verdicts.
#
# Prereqs: a rossoctl kind cluster, Ollama on the host with the model, and
# kubectl / kind / docker(or podman) / python3 / uv. For PROVIDER=watsonx also
# export WX_API_KEY and WX_PROJECT_ID (e.g. `set -a; . /path/.env; set +a`).

.PHONY: help preflight host-setup build-images build-authbridge load-images \
        ollama-up deploy wait-pods patch-config setup-keycloak drive demo \
        build-abctl show-result undeploy logs-agent logs-sparc logs-ollama

.DEFAULT_GOAL := help

PROVIDER          ?= watsonx
KIND_CLUSTER_NAME ?= rossoctl
KIND_NODE         := $(KIND_CLUSTER_NAME)-control-plane
# NS / AGENT / PLATFORM_NS are fixed: the k8s/ manifests hardcode these names
# (team1 / finance-agent / rossoctl-system), so they are not override knobs —
# changing them here alone would target resources the manifests never create.
NS                := team1
AGENT             := finance-agent
PLATFORM_NS       := rossoctl-system
# Prefer whichever runtime's daemon is actually reachable (docker first).
CONTAINER_RUNTIME ?= $(shell docker info >/dev/null 2>&1 && echo docker || (podman info >/dev/null 2>&1 && echo podman) || echo docker)
AUTHBRIDGE_IMAGE  ?= ghcr.io/rossoctl/cortex/authbridge:v0.6.0-alpha.4

AGENT_IMAGE := finance-agent:latest
MCP_IMAGE   := finance-mcp:latest
SPARC_IMAGE := sparc-service:latest
# Shared SPARC-service installer (the demo deploys the service through it).
SPARC_DEPLOY := ../../sparc-service/deploy
# Pinned (must match k8s/ollama.yaml). Override only if you stage a different tag.
OLLAMA_IMAGE := ollama/ollama:0.30.2
# In-cluster Ollama (the agent's reasoning LLM for the watsonx path).
OLLAMA_URL  := http://ollama.$(NS).svc.cluster.local:11434

# Host Ollama, reachable from the cluster. ALTK's ollama reflection is free-form
# generate+validate (guided decoding is disabled for ollama), which is slow on a
# CPU-only in-cluster node but fast on a Metal/GPU host. So PROVIDER=ollama uses
# the HOST Ollama for both reasoning and reflection. host.docker.internal is the
# portable default; on Rancher Desktop/colima use http://192.168.5.2:11434.
HOST_OLLAMA_URL ?= http://host.docker.internal:11434

# Per-provider models/timeouts (all override-able).
#  - watsonx: reflection on watsonx (fast); reasoning on in-cluster Ollama (1b).
#  - ollama:  reflection AND reasoning on the host Ollama. Reflection needs a
#             capable instruct model (3b/1b hallucinate the function-selection
#             metric); reasoning needs a clean tool-caller. Override SPARC_MODEL/
#             AGENT_OLLAMA_MODEL to match what your host Ollama has pulled.
ifeq ($(PROVIDER),watsonx)
SPARC_MODEL        ?= mistral-large-2512
SPARC_TIMEOUT_MS   ?= 30000
TURN_TIMEOUT       ?= 240
AGENT_OLLAMA_URL   ?= $(OLLAMA_URL)
AGENT_OLLAMA_MODEL ?= llama3.2:1b
else
SPARC_OLLAMA_URL   ?= $(HOST_OLLAMA_URL)
SPARC_MODEL        ?= gemma4:e4b
SPARC_TIMEOUT_MS   ?= 200000
TURN_TIMEOUT       ?= 600
AGENT_OLLAMA_URL   ?= $(HOST_OLLAMA_URL)
AGENT_OLLAMA_MODEL ?= llama3.2:3b
endif

help:  ## Show this menu
	@printf "\nfinance-sparc demo — SPARC reflection on a regular finance agent.\n\n"
	@printf "Run:\n  \033[1mmake demo\033[0m                 (watsonx; needs WX_* env)\n"
	@printf "  \033[1mmake demo PROVIDER=ollama\033[0m  (local ollama; no creds)\n\n"
	@printf "Targets:\n"
	@awk 'BEGIN{FS=":.*## "} /^[a-zA-Z_-]+:.*## /{printf "  \033[36m%-16s\033[0m %s\n",$$1,$$2}' $(MAKEFILE_LIST)
	@printf "\nVars: PROVIDER(=%s) KIND_CLUSTER_NAME AUTHBRIDGE_IMAGE SPARC_MODEL HOST_OLLAMA_URL\n\n" "$(PROVIDER)"

preflight:  ## Verify rossoctl + tools (+ watsonx creds when PROVIDER=watsonx)
	@kubectl get ns $(PLATFORM_NS) >/dev/null 2>&1 || { echo "ERROR: '$(PLATFORM_NS)' not found — install rossoctl first."; exit 1; }
	@kubectl get ns $(NS) >/dev/null 2>&1 || { echo "ERROR: agent namespace '$(NS)' not found."; exit 1; }
	@for t in kubectl kind python3 uv; do command -v $$t >/dev/null 2>&1 || { echo "ERROR: $$t not installed"; exit 1; }; done
	@if [ "$(PROVIDER)" = "watsonx" ]; then \
	  [ -n "$${WX_API_KEY:-}" ] && [ -n "$${WX_PROJECT_ID:-}" ] || { echo "ERROR: PROVIDER=watsonx needs WX_API_KEY/WX_PROJECT_ID (or run PROVIDER=ollama)"; exit 1; }; \
	fi

host-setup:  ## Ensure cluster internet egress works (MSS-clamp on hotspot/VPN networks)
	KIND_CLUSTER_NAME=$(KIND_CLUSTER_NAME) bash scripts/host-setup.sh

ifeq ($(PROVIDER),watsonx)
ollama-up:  ## watsonx: deploy in-cluster Ollama with the reasoning model (staged from host)
	@if $(CONTAINER_RUNTIME) exec $(KIND_NODE) ctr -n k8s.io images list -q 2>/dev/null | grep -q "$(OLLAMA_IMAGE)"; then \
	  echo "[*] $(OLLAMA_IMAGE) already present in node"; \
	else \
	  $(CONTAINER_RUNTIME) image inspect $(OLLAMA_IMAGE) >/dev/null 2>&1 || $(CONTAINER_RUNTIME) pull $(OLLAMA_IMAGE); \
	  kind load docker-image $(OLLAMA_IMAGE) --name $(KIND_CLUSTER_NAME); \
	  $(CONTAINER_RUNTIME) exec $(KIND_NODE) ctr -n k8s.io images tag localhost/$(OLLAMA_IMAGE) docker.io/library/$(OLLAMA_IMAGE) >/dev/null 2>&1 || true; \
	fi
	OLLAMA_MODEL=$(AGENT_OLLAMA_MODEL) KIND_CLUSTER_NAME=$(KIND_CLUSTER_NAME) bash scripts/stage-ollama-model.sh $(AGENT_OLLAMA_MODEL)
	kubectl apply -f k8s/ollama.yaml
	kubectl -n $(NS) rollout status deploy/ollama --timeout=180s
	@echo "[*] warming $(AGENT_OLLAMA_MODEL) (loads into memory; KEEP_ALIVE keeps it resident) ..."
	-kubectl -n $(NS) exec deploy/ollama -- ollama run $(AGENT_OLLAMA_MODEL) "hi" >/dev/null 2>&1
else
ollama-up:  ## ollama: use the host Ollama (Metal/GPU) for reasoning + reflection — no in-cluster Ollama
	@echo "[*] PROVIDER=ollama: reasoning + reflection use the HOST Ollama at $(HOST_OLLAMA_URL)"
	@echo "    reasoning model=$(AGENT_OLLAMA_MODEL)  reflection model=$(SPARC_MODEL)"
	@echo "    (ALTK reflects free-form on ollama; a CPU-only in-cluster Ollama is too slow, so we use the host.)"
	@echo "[*] checking the cluster can reach the host Ollama ..."
	@kubectl run sparc-ollama-probe-$$$$ --rm -i --restart=Never --image=curlimages/curl:8.10.1 --quiet -- \
	   -s -m 10 -o /dev/null -w 'host Ollama %{http_code}\n' $(HOST_OLLAMA_URL)/api/tags 2>/dev/null \
	   || echo "    WARN: could not reach $(HOST_OLLAMA_URL) from the cluster. Ensure host Ollama runs (OLLAMA_HOST=0.0.0.0) and HOST_OLLAMA_URL is correct (Rancher/colima: http://192.168.5.2:11434)."
endif

build-images:  ## Build the agent, MCP server, and SPARC service images
	@echo "[*] building $(SPARC_IMAGE)";  $(CONTAINER_RUNTIME) build -t $(SPARC_IMAGE) ../../sparc-service
	@echo "[*] building $(AGENT_IMAGE)";  $(CONTAINER_RUNTIME) build -f finance-agent/Dockerfile -t $(AGENT_IMAGE) .
	@echo "[*] building $(MCP_IMAGE)";    $(CONTAINER_RUNTIME) build -f finance-mcp/Dockerfile -t $(MCP_IMAGE) .

build-authbridge:  ## Build the authbridge image WITH the sparc plugin (operator's tag)
	@echo "[*] building $(AUTHBRIDGE_IMAGE) (includes sparc plugin)"
	cd ../.. && $(CONTAINER_RUNTIME) build -f cmd/authbridge-proxy/Dockerfile -t $(AUTHBRIDGE_IMAGE) .

load-images: build-images build-authbridge  ## Load all images into kind
	@for img in $(AGENT_IMAGE) $(MCP_IMAGE) $(SPARC_IMAGE) $(AUTHBRIDGE_IMAGE); do \
	  echo "[*] kind load $$img"; kind load docker-image "$$img" --name $(KIND_CLUSTER_NAME); \
	  $(CONTAINER_RUNTIME) exec $(KIND_NODE) ctr -n k8s.io images tag localhost/$$img docker.io/library/$$img >/dev/null 2>&1 || true; \
	done

deploy:  ## Deploy the SPARC service (shared installer), finance MCP server, and agent
	@# The SPARC service is deployed through the shared installer — the same
	@# one-command path a user runs for any cluster. The demo's images are
	@# already built + kind-loaded by load-images, so we pass IMAGE and skip the
	@# installer's own build. WX_* creds (watsonx) are read from the environment.
	$(MAKE) -C $(SPARC_DEPLOY) install \
	  NAMESPACE=$(PLATFORM_NS) IMAGE=$(SPARC_IMAGE) PROVIDER=$(PROVIDER) \
	  MODEL=$(SPARC_MODEL) TRACK=fast_track OLLAMA_BASE_URL=$(SPARC_OLLAMA_URL) \
	  KIND_CLUSTER_NAME=$(KIND_CLUSTER_NAME)
	kubectl apply -f k8s/finance-mcp.yaml
	kubectl apply -f k8s/agent.yaml
	@# Point the agent's reasoning LLM at the provider's Ollama (in-cluster for
	@# watsonx, host for ollama). This rolls the agent; the operator then resets
	@# its authbridge ConfigMap, which patch-config re-applies next.
	kubectl -n $(NS) set env deploy/$(AGENT) -c agent OLLAMA_URL=$(AGENT_OLLAMA_URL) OLLAMA_MODEL=$(AGENT_OLLAMA_MODEL) >/dev/null
	@# Pick up freshly-loaded images (apply is a no-op when only :latest content changed).
	-kubectl -n $(NS) rollout restart deploy/finance-mcp >/dev/null 2>&1
	-kubectl -n $(NS) rollout restart deploy/$(AGENT) >/dev/null 2>&1
	$(MAKE) wait-pods

wait-pods:  ## Wait for service/MCP/agent + the operator-rendered authbridge ConfigMap
	kubectl -n $(PLATFORM_NS) rollout status deploy/sparc-service --timeout=180s
	kubectl -n $(NS) rollout status deploy/finance-mcp --timeout=120s
	kubectl -n $(NS) rollout status deploy/$(AGENT) --timeout=180s
	@for i in $$(seq 1 60); do kubectl -n $(NS) get cm authbridge-config-$(AGENT) >/dev/null 2>&1 && break; sleep 2; done
	@kubectl -n $(NS) get cm authbridge-config-$(AGENT) >/dev/null 2>&1 || { echo "ERROR: operator didn't create authbridge-config-$(AGENT)"; exit 1; }

patch-config:  ## Enable the parsers + sparc in the agent's authbridge pipeline (hot-reload)
	SPARC_TIMEOUT_MS=$(SPARC_TIMEOUT_MS) bash scripts/patch-sparc-config.sh $(NS) $(AGENT)

setup-keycloak:  ## Create the audience scope, ROPC client, and alice so inbound auth passes
	@kubectl -n keycloak port-forward svc/keycloak-service 18081:8080 >/tmp/finance-kc-setup-pf.log 2>&1 & \
	  KCPF=$$!; sleep 4; \
	  NAMESPACE=$(NS) AGENT_SA=$(AGENT) KEYCLOAK_URL=http://localhost:18081 \
	    uv run --with python-keycloak python scripts/setup_keycloak_finance.py; rc=$$?; \
	  kill $$KCPF 2>/dev/null || true; exit $$rc

drive:  ## Drive the two-turn scenario through real auth and print SPARC verdicts
	TURN_TIMEOUT=$(TURN_TIMEOUT) bash scripts/drive-demo.sh $(NS) $(AGENT)

demo: preflight host-setup load-images ollama-up deploy patch-config setup-keycloak drive  ## Full end-to-end demo
	@echo
	@echo "Demo complete. UI: http://rossoctl-ui.localtest.me:8080  |  forensic: make show-result"

build-abctl:  ## Build abctl into /tmp (for show-result)
	cd ../../cmd/abctl && go build -o /tmp/abctl-finance-sparc-demo .

show-result: build-abctl  ## Launch abctl against the agent's session API
	bash scripts/launch-abctl.sh $(NS) $(AGENT)

undeploy:  ## Remove demo resources (rossoctl install untouched)
	-kubectl -n $(NS) delete -f k8s/agent.yaml --ignore-not-found
	-kubectl -n $(NS) delete -f k8s/finance-mcp.yaml --ignore-not-found
	-kubectl -n $(NS) delete -f k8s/ollama.yaml --ignore-not-found
	-$(MAKE) -C $(SPARC_DEPLOY) uninstall NAMESPACE=$(PLATFORM_NS)
	-kubectl -n $(NS) delete configmap authbridge-config-$(AGENT) --ignore-not-found
	-kubectl -n $(NS) delete secret -l rossoctl.io/client-name=$(AGENT) --ignore-not-found
	@echo "[*] demo resources removed."

logs-agent:  ## Tail agent + authbridge sidecar logs
	@echo "--- agent ---";      kubectl -n $(NS) logs deploy/$(AGENT) -c agent --tail=80
	@echo "--- authbridge ---"; kubectl -n $(NS) logs deploy/$(AGENT) -c authbridge-proxy --tail=80

logs-sparc:  ## Tail the SPARC reflection service logs
	kubectl -n $(PLATFORM_NS) logs deploy/sparc-service --tail=80

logs-ollama:  ## Tail the in-cluster Ollama logs
	kubectl -n $(NS) logs deploy/ollama --tail=80
