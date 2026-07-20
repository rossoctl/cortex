# Install the SPARC reflection service into a cluster — one command.
#
# Deploy this ONCE per cluster BEFORE enabling the `sparc` plugin on any agent.
#
#   make install                      # watsonx (needs WX_API_KEY / WX_PROJECT_ID)
#   make install PROVIDER=ollama OLLAMA_BASE_URL=http://host.docker.internal:11434
#   make install PROVIDER=openai      # needs OPENAI_API_KEY
#   make image install                # build the image locally + load into kind first
#   make uninstall
#
# By default it deploys the published image. On a kind dev cluster (or to run a
# local build) prepend the `image` target to build + load sparc-service:latest.

.PHONY: help install uninstall image status

.DEFAULT_GOAL := help

PROVIDER          ?= watsonx
NAMESPACE         ?= rossoctl-system
IMAGE             ?= ghcr.io/rossoctl/cortex/sparc-service:latest
TRACK             ?= fast_track
LLM_TIMEOUT       ?= 120
PORT              ?= 8090
# Optional (provider-dependent). MODEL defaults per-provider in the service.
MODEL             ?=
OLLAMA_BASE_URL   ?= http://host.docker.internal:11434
LLM_KWARGS_JSON   ?=
LLM_REGISTRY_ID   ?=
# Local build / kind (only used by `make image`).
KIND_CLUSTER_NAME ?= rossoctl
KIND_NODE         := $(KIND_CLUSTER_NAME)-control-plane
CONTAINER_RUNTIME ?= $(shell docker info >/dev/null 2>&1 && echo docker || (podman info >/dev/null 2>&1 && echo podman) || echo docker)

# Provider credential env vars copied into the sparc-creds Secret when present.
# LiteLLM (openai/azure/litellm) reads its provider key from the environment, so
# injecting these as env via the Secret is enough.
CRED_VARS := WX_API_KEY WX_PROJECT_ID WX_URL OPENAI_API_KEY OPENAI_BASE_URL \
             AZURE_API_KEY ANTHROPIC_API_KEY GEMINI_API_KEY OLLAMA_API_KEY

help:  ## Show this menu
	@printf "\nInstall the SPARC reflection service (deploy before enabling the sparc plugin).\n\n"
	@printf "Run:\n  \033[1mmake install\033[0m                       (watsonx; needs WX_* env)\n"
	@printf "  \033[1mmake install PROVIDER=ollama\033[0m         (local ollama)\n"
	@printf "  \033[1mmake image install\033[0m                   (build + kind-load, then install)\n\n"
	@printf "Targets:\n"
	@awk 'BEGIN{FS=":.*## "} /^[a-zA-Z_-]+:.*## /{printf "  \033[36m%-12s\033[0m %s\n",$$1,$$2}' $(MAKEFILE_LIST)
	@printf "\nVars: PROVIDER(=%s) NAMESPACE(=%s) IMAGE MODEL TRACK OLLAMA_BASE_URL\n" "$(PROVIDER)" "$(NAMESPACE)"
	@printf "      LLM_KWARGS_JSON LLM_REGISTRY_ID KIND_CLUSTER_NAME\n\n"

image:  ## Build the sparc-service image locally and load it into kind
	@echo "[*] building $(IMAGE)"
	$(CONTAINER_RUNTIME) build -t $(IMAGE) ..
	@echo "[*] kind load $(IMAGE) into $(KIND_CLUSTER_NAME)"
	kind load docker-image $(IMAGE) --name $(KIND_CLUSTER_NAME)
	@# Let containerd resolve the bare docker.io/library/<img> ref kubelet uses.
	-$(CONTAINER_RUNTIME) exec $(KIND_NODE) ctr -n k8s.io images tag localhost/$(IMAGE) docker.io/library/$(IMAGE) >/dev/null 2>&1 || true

install:  ## Create config + creds and deploy the SPARC service into $(NAMESPACE)
	@kubectl get ns $(NAMESPACE) >/dev/null 2>&1 || kubectl create ns $(NAMESPACE)
	@echo "[*] config: provider=$(PROVIDER) namespace=$(NAMESPACE) track=$(TRACK)"
	@# --- ConfigMap (non-secret settings) ---
	@cfg="--from-literal=SPARC_LLM_PROVIDER=$(PROVIDER) --from-literal=SPARC_TRACK=$(TRACK)"; \
	cfg="$$cfg --from-literal=SPARC_LLM_TIMEOUT=$(LLM_TIMEOUT) --from-literal=PORT=$(PORT)"; \
	[ -n "$(MODEL)" ]           && cfg="$$cfg --from-literal=SPARC_MODEL=$(MODEL)"; \
	[ "$(PROVIDER)" = "ollama" ] && cfg="$$cfg --from-literal=OLLAMA_BASE_URL=$(OLLAMA_BASE_URL)"; \
	[ -n "$(LLM_KWARGS_JSON)" ] && cfg="$$cfg --from-literal=SPARC_LLM_KWARGS_JSON=$(LLM_KWARGS_JSON)"; \
	[ -n "$(LLM_REGISTRY_ID)" ] && cfg="$$cfg --from-literal=SPARC_LLM_REGISTRY_ID=$(LLM_REGISTRY_ID)"; \
	kubectl -n $(NAMESPACE) create configmap sparc-service-config $$cfg --dry-run=client -o yaml | kubectl apply -f -
	@# --- Secret (provider credentials present in the environment) ---
	@sec=""; for v in $(CRED_VARS); do \
	  val=$$(printenv $$v || true); [ -n "$$val" ] && sec="$$sec --from-literal=$$v=$$val"; \
	done; \
	kubectl -n $(NAMESPACE) create secret generic sparc-creds $$sec --dry-run=client -o yaml | kubectl apply -f -
	@# --- Workload ---
	kubectl -n $(NAMESPACE) apply -f sparc-service.yaml
	kubectl -n $(NAMESPACE) set image deploy/sparc-service sparc-service=$(IMAGE) >/dev/null
	@kubectl -n $(NAMESPACE) rollout restart deploy/sparc-service >/dev/null 2>&1 || true
	kubectl -n $(NAMESPACE) rollout status deploy/sparc-service --timeout=180s
	@echo "[*] SPARC service ready at http://sparc-service.$(NAMESPACE).svc:$(PORT)/reflect"
	@echo "    Now enable the 'sparc' plugin in an agent's authbridge config (see docs/sparc-plugin.md)."

status:  ## Show the SPARC service deployment + config
	kubectl -n $(NAMESPACE) get deploy,svc,cm/sparc-service-config -l app.kubernetes.io/name=sparc-service 2>/dev/null; \
	kubectl -n $(NAMESPACE) get cm sparc-service-config -o yaml 2>/dev/null | sed -n '/^data:/,/^[a-z]/p'

uninstall:  ## Remove the SPARC service, its config, and creds from $(NAMESPACE)
	-kubectl -n $(NAMESPACE) delete -f sparc-service.yaml --ignore-not-found
	-kubectl -n $(NAMESPACE) delete configmap sparc-service-config --ignore-not-found
	-kubectl -n $(NAMESPACE) delete secret sparc-creds --ignore-not-found
	@echo "[*] SPARC service removed from $(NAMESPACE)."
