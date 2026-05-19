# IBAC demo Makefile — kagenti-UI flow.
#
# End-to-end:
#
#   make build-images          # 3 demo images (agent, email, evil)
#   make build-authbridge      # rebuild authbridge from this branch
#                              # under whatever tag the kagenti operator
#                              # pulls (autodetected from a running pod)
#   make load-images           # kind load all four
#   make demo-ibac             # deploy + patch + ready-to-chat banner
#   make show-result           # render pipeline forensic of last session
#   make undeploy              # remove the demo's resources from team1
#
# Prerequisites:
#   - kagenti installed on a kind cluster (this is the operator-flow
#     demo; for plain-kind testing of the IBAC plugin, see commit history
#     for the standalone variant)
#   - ollama on the host with llama3.2:3b pulled
#   - kubectl, kind, podman/docker, python3 + PyYAML

.PHONY: build-images build-authbridge load-images deploy patch-config \
        wait-reload demo-ibac show-result undeploy logs-agent logs-evil \
        preflight wait-pods

KIND_CLUSTER_NAME ?= kagenti
KIND_NODE         ?= $(KIND_CLUSTER_NAME)-control-plane
NAMESPACE         ?= team1
AGENT_NAME        ?= ibac-agent

# Container runtime auto-detect.
CONTAINER_RUNTIME ?= $(shell command -v podman >/dev/null 2>&1 && echo podman || echo docker)

# Demo-image tags (local-only, not pushed).
AGENT_IMAGE       := ibac-agent:latest
EMAIL_IMAGE       := ibac-email-server:latest
EVIL_IMAGE        := ibac-evil-server:latest

# Auto-detect the authbridge image tag the operator is configured to
# pull. Reads a running agent pod's authbridge-proxy container image.
# Falls back to v0.6.0-alpha.3 (current chart pin at the time of
# writing) when nothing is running yet — the user can override with
# `make AUTHBRIDGE_TAG=v0.6.0-alpha.4 ...`.
AUTHBRIDGE_TAG ?= $(shell kubectl get pod -A \
    -o jsonpath='{range .items[*]}{.spec.containers[?(@.name=="authbridge-proxy")].image}{"\n"}{end}' 2>/dev/null \
    | grep -E '^ghcr.io/kagenti/kagenti-extensions/authbridge:' \
    | head -1 | awk -F: '{print $$NF}')
ifeq ($(AUTHBRIDGE_TAG),)
  AUTHBRIDGE_TAG := v0.6.0-alpha.3
endif
AUTHBRIDGE_IMAGE := ghcr.io/kagenti/kagenti-extensions/authbridge:$(AUTHBRIDGE_TAG)

# ---------- Pre-flight ----------

preflight:
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

build-images:
	$(CONTAINER_RUNTIME) build -f agent/Dockerfile        -t $(AGENT_IMAGE) .
	$(CONTAINER_RUNTIME) build -f email-server/Dockerfile -t $(EMAIL_IMAGE) .
	$(CONTAINER_RUNTIME) build -f evil-server/Dockerfile  -t $(EVIL_IMAGE)  .

build-authbridge:
	@# Fail loudly if invoked from outside authbridge/demos/ibac/.
	@test -f ../../cmd/authbridge-proxy/Dockerfile || { \
	  echo "build-authbridge must be run from authbridge/demos/ibac/"; \
	  echo "(expected ../../cmd/authbridge-proxy/Dockerfile to exist)"; \
	  exit 1; \
	}
	@echo "[*] Building authbridge:$(AUTHBRIDGE_TAG) (matches operator-pinned tag)"
	cd ../.. && $(CONTAINER_RUNTIME) build \
	  -f cmd/authbridge-proxy/Dockerfile -t $(AUTHBRIDGE_IMAGE) .

load-images:
	kind load docker-image $(AGENT_IMAGE)      --name $(KIND_CLUSTER_NAME)
	kind load docker-image $(EMAIL_IMAGE)      --name $(KIND_CLUSTER_NAME)
	kind load docker-image $(EVIL_IMAGE)       --name $(KIND_CLUSTER_NAME)
	kind load docker-image $(AUTHBRIDGE_IMAGE) --name $(KIND_CLUSTER_NAME)
ifeq ($(CONTAINER_RUNTIME),podman)
	$(CONTAINER_RUNTIME) exec $(KIND_NODE) ctr --namespace=k8s.io images tag localhost/$(AGENT_IMAGE)        docker.io/library/$(AGENT_IMAGE)        || true
	$(CONTAINER_RUNTIME) exec $(KIND_NODE) ctr --namespace=k8s.io images tag localhost/$(EMAIL_IMAGE)        docker.io/library/$(EMAIL_IMAGE)        || true
	$(CONTAINER_RUNTIME) exec $(KIND_NODE) ctr --namespace=k8s.io images tag localhost/$(EVIL_IMAGE)         docker.io/library/$(EVIL_IMAGE)         || true
endif

# ---------- Deploy + patch ----------

deploy:
	kubectl apply -f k8s/email-server.yaml
	kubectl apply -f k8s/evil-server.yaml
	kubectl apply -f k8s/agent.yaml
	@# Force a rollout so the deployments pick up newly-loaded images.
	@# kubectl apply is a no-op when only the image content changed
	@# but the manifest didn't (image tag is :latest). We rollout-
	@# restart unconditionally; idempotent on the first run since
	@# Available + Available == still Available.
	kubectl -n $(NAMESPACE) rollout restart deploy/ibac-email-server deploy/ibac-evil-server deploy/ibac-agent
	$(MAKE) wait-pods

wait-pods:
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

patch-config:
	bash scripts/patch-ibac-config.sh $(NAMESPACE) $(AGENT_NAME)

wait-reload:
	bash scripts/wait-for-reload.sh $(NAMESPACE) $(AGENT_NAME) 120

# ---------- Top-level demo + result rendering ----------

demo-ibac: preflight deploy patch-config wait-reload
	@echo
	@echo "╔══════════════════════════════════════════════════════════╗"
	@echo "║ ✅ IBAC demo agent ready in $(NAMESPACE)                          ║"
	@echo "║                                                          ║"
	@echo "║ Open the kagenti UI:                                     ║"
	@echo "║   http://kagenti-ui.localtest.me:8080                    ║"
	@echo "║                                                          ║"
	@echo "║ Pick \`$(AGENT_NAME)\` from the Agents list, then chat:       ║"
	@echo "║                                                          ║"
	@echo "║   \"Summarize my emails.\"                                 ║"
	@echo "║                                                          ║"
	@echo "║ The agent will attempt the email-poison attack, IBAC     ║"
	@echo "║ will block the exfiltration, and the chat response will  ║"
	@echo "║ start with a ⚠️  Security event message.                  ║"
	@echo "║                                                          ║"
	@echo "║ Then run:                                                ║"
	@echo "║                                                          ║"
	@echo "║   make show-result                                       ║"
	@echo "║                                                          ║"
	@echo "║ to see the pipeline-level forensic.                      ║"
	@echo "╚══════════════════════════════════════════════════════════╝"

show-result:
	bash scripts/show-result.sh $(NAMESPACE) $(AGENT_NAME)

# ---------- Cleanup ----------

undeploy:
	-kubectl -n $(NAMESPACE) delete -f k8s/agent.yaml --ignore-not-found
	-kubectl -n $(NAMESPACE) delete -f k8s/email-server.yaml --ignore-not-found
	-kubectl -n $(NAMESPACE) delete -f k8s/evil-server.yaml --ignore-not-found
	-kubectl -n $(NAMESPACE) delete configmap authbridge-config-$(AGENT_NAME) --ignore-not-found
	-kubectl -n $(NAMESPACE) delete secret -l kagenti.io/client-name=$(AGENT_NAME) --ignore-not-found
	@echo "[*] Demo resources removed from $(NAMESPACE)."
	@echo "    The kagenti install + the namespace itself are untouched."

# ---------- Diagnostics ----------

logs-agent:
	@echo "--- agent ---"
	kubectl -n $(NAMESPACE) logs deploy/$(AGENT_NAME) -c agent --tail=100
	@echo "--- authbridge ---"
	kubectl -n $(NAMESPACE) logs deploy/$(AGENT_NAME) -c authbridge-proxy --tail=100

logs-evil:
	kubectl -n $(NAMESPACE) logs deploy/ibac-evil-server --tail=50
