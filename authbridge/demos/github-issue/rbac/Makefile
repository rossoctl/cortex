# demo-rbac Makefile — RBAC-aware GitHub Issue Agent demo with AuthBridge.
#
# End-to-end:
#
#   make build-images          # build + load agent/tool images into the Kind cluster
#   make setup-keycloak        # configure Keycloak (realm, clients, RBAC roles, users)
#   make deploy
#                              # namespace + ConfigMaps + PAT secret + tool + agent
#   make validate              # verify pod readiness and operator client registration
#   make test-ab               # authbridge inbound validation tests (Steps 8a–8f from demo-rbac.md)
#   make test-rbac             # show scope-based PAT selection logs (Alice vs Bob)
#   make clean                 # delete all deployed resources
#
# Prerequisites:
#   - uv (https://docs.astral.sh/uv/)
#   - kubectl pointing at the Kind cluster
#   - Kind cluster accessible (default name: rossoctl)
#   - Keycloak running at http://keycloak.localtest.me:8080
#   - AGENT_EXAMPLES_DIR set to the agent-examples repo clone (for build-images)
#   - PRIVILEGED_ACCESS_PAT and PUBLIC_ACCESS_PAT set for the deploy target

.PHONY: help preflight build-images setup-namespace setup-keycloak apply-configmaps \
        check-pats create-secret deploy-tool deploy-agent deploy validate test-ab test-rbac clean

# `make` with no target prints help.
.DEFAULT_GOAL := help

help:  ## Show this menu (default)
	@printf "\ndemo-rbac — RBAC-aware GitHub Issue Agent demo with AuthBridge.\n\n"
	@printf "Typical run:\n"
	@printf "  \033[1mmake build-images AGENT_EXAMPLES_DIR=<path>\033[0m  # build + load images\n"
	@printf "  \033[1mmake setup-keycloak\033[0m                           # configure Keycloak\n"
	@printf "  \033[1mmake deploy PRIVILEGED_ACCESS_PAT=<pat> PUBLIC_ACCESS_PAT=<pat>\033[0m\n"
	@printf "  \033[1mmake validate\033[0m                                 # verify pods ready\n"
	@printf "  \033[1mmake test\033[0m                                     # inbound tests\n"
	@printf "  \033[1mmake test-rbac\033[0m                                # Alice vs Bob\n"
	@printf "  \033[1mmake clean\033[0m                                    # tear everything down\n\n"
	@printf "Targets:\n"
	@awk 'BEGIN { FS = ":.*## " } \
	     /^[a-zA-Z_-]+:.*## / { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 }' \
	     $(MAKEFILE_LIST)
	@printf "\nVariables:\n"
	@printf "  \033[36m%-22s\033[0m %s\n" "NAMESPACE"             "target namespace (default: team1)"
	@printf "  \033[36m%-22s\033[0m %s\n" "KIND_CLUSTER_NAME"     "Kind cluster name (default: rossoctl)"
	@printf "  \033[36m%-22s\033[0m %s\n" "AGENT_EXAMPLES_DIR"    "path to agent-examples repo clone (required for build-images)"
	@printf "  \033[36m%-22s\033[0m %s\n" "PRIVILEGED_ACCESS_PAT" "GitHub PAT with all-repo access (required for deploy)"
	@printf "  \033[36m%-22s\033[0m %s\n" "PUBLIC_ACCESS_PAT"     "GitHub PAT with public-repo access (required for deploy)"
	@printf "  \033[36m%-22s\033[0m %s\n" "RBAC_CONFIG"           "RBAC config file (default: rbac/config.yaml)"
	@printf "  \033[36m%-22s\033[0m %s\n" "RBAC_POLICY"           "RBAC policy file (default: rbac/policy.yaml)"
	@printf "  \033[36m%-22s\033[0m %s\n" "PYTHON"                "Python interpreter (default: rossocortex/.venv/bin/python)"
	@printf "\nPrerequisites: uv, kubectl, kind, docker, Keycloak running.\n\n"

MAKEFILE_DIR := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))

NAMESPACE             ?= team1
KIND_CLUSTER_NAME     ?= rossoctl
RBAC_CONFIG           ?= rbac/config.yaml
RBAC_POLICY           ?= rbac/policy.yaml
AGENT_EXAMPLES_DIR    ?= $(MAKEFILE_DIR)../../../../../agent-examples


# Resolve the venv relative to this Makefile, regardless of where make is invoked.
VENV         := $(MAKEFILE_DIR)../../../../.venv
PYTHON       ?= $(VENV)/bin/python

# ---------- Pre-flight ----------

preflight:  ## Verify prerequisites (kubectl, kind, docker, python deps, Keycloak)
	@command -v kubectl >/dev/null 2>&1 || { \
	  echo "ERROR: kubectl not found."; exit 1; \
	}
	@command -v kind >/dev/null 2>&1 || { \
	  echo "ERROR: kind not found."; exit 1; \
	}
	@command -v docker >/dev/null 2>&1 || command -v podman >/dev/null 2>&1 || { \
	  echo "ERROR: docker or podman not found."; exit 1; \
	}
	@test -x "$(VENV)/bin/python" || { \
	  echo "ERROR: venv not found at $(VENV)"; \
	  echo "       Run: uv sync  (from authbridge/)"; \
	  exit 1; \
	}
	@$(PYTHON) -c 'import yaml, keycloak' 2>/dev/null || { \
	  echo "ERROR: Python dependencies missing."; \
	  echo "       Run: uv pip install --python $(VENV)/bin/python -r ../../../requirements.txt"; \
	  exit 1; \
	}
	@curl -sf "http://keycloak.localtest.me:8080/realms/master" -o /dev/null || { \
	  echo "ERROR: Keycloak not reachable at http://keycloak.localtest.me:8080"; \
	  echo "       Start: kubectl port-forward service/keycloak-service -n keycloak 8080:8080"; \
	  exit 1; \
	}
	@echo "[✓] All prerequisites met."

# ---------- Build images ----------

build-images:  ## Build + load agent and tool container images into Kind (requires AGENT_EXAMPLES_DIR)
	@test -n "$(AGENT_EXAMPLES_DIR)" || { \
	  echo "ERROR: AGENT_EXAMPLES_DIR is not set."; \
	  echo "       Set it to the path of your agent-examples repo clone."; \
	  exit 1; \
	}
	@echo "[*] Building github-tool image ..."
	docker build -t ghcr.io/rossoctl/examples/github-tool:latest \
	  -f $(AGENT_EXAMPLES_DIR)/mcp/github_tool/Dockerfile \
	  $(AGENT_EXAMPLES_DIR)/mcp/github_tool/
	@echo "[*] Building git-issue-agent image ..."
	docker build -t ghcr.io/rossoctl/examples/git-issue-agent:latest \
	  -f $(AGENT_EXAMPLES_DIR)/a2a/git_issue_agent/Dockerfile \
	  $(AGENT_EXAMPLES_DIR)/a2a/git_issue_agent/
	@echo "[*] Loading images into Kind cluster '$(KIND_CLUSTER_NAME)' ..."
	kind load docker-image --name $(KIND_CLUSTER_NAME) \
	  ghcr.io/rossoctl/examples/github-tool:latest
	kind load docker-image --name $(KIND_CLUSTER_NAME) \
	  ghcr.io/rossoctl/examples/git-issue-agent:latest
	@echo "[✓] Images built and loaded."

# ---------- Setup ----------

setup-namespace:  ## Create namespace and apply webhook ConfigMaps
	@echo "[*] Creating namespace '$(NAMESPACE)' ..."
	kubectl create namespace $(NAMESPACE) --dry-run=client -o yaml | kubectl apply -f -
	@test -f k8s/configmaps-webhook.yaml && { \
	  echo "[*] Applying webhook ConfigMaps ..."; \
	  kubectl apply -f k8s/configmaps-webhook.yaml -n $(NAMESPACE); \
	} || echo "[!] k8s/configmaps-webhook.yaml not found — skipping (Rossoctl installer creates these)."
	@echo "[✓] Namespace ready."

setup-keycloak: preflight  ## Configure Keycloak: realm, clients, RBAC roles, and users
	@test -f ../$(RBAC_CONFIG) || { \
	  echo "ERROR: RBAC config not found at $(RBAC_CONFIG)"; \
	  echo "       Expected: $(MAKEFILE_DIR)$(RBAC_CONFIG)"; \
	  exit 1; \
	}
	@test -f ../$(RBAC_POLICY) || { \
	  echo "ERROR: RBAC policy not found at $(RBAC_POLICY)"; \
	  echo "       Expected: $(MAKEFILE_DIR)$(RBAC_POLICY)"; \
	  exit 1; \
	}
	@echo "[*] Configuring Keycloak realm ..."
	$(PYTHON) ../setup_keycloak.py -rbac $(RBAC_CONFIG) -policy $(RBAC_POLICY)
	@echo "[✓] Keycloak configured."

apply-configmaps:  ## Apply demo-specific ConfigMaps (authbridge-config, authproxy-routes)
	@echo "[*] Applying demo ConfigMaps ..."
	kubectl apply -f ../k8s/configmaps.yaml -n $(NAMESPACE)
	@echo "[✓] ConfigMaps applied."

# ---------- PAT secret ----------

check-pats:
	@test -n "$(PRIVILEGED_ACCESS_PAT)" || { \
	  echo "ERROR: PRIVILEGED_ACCESS_PAT is not set."; \
	  echo "       make deploy PRIVILEGED_ACCESS_PAT=ghp_xxx PUBLIC_ACCESS_PAT=ghp_yyy"; \
	  exit 1; \
	}
	@test -n "$(PUBLIC_ACCESS_PAT)" || { \
	  echo "ERROR: PUBLIC_ACCESS_PAT is not set."; \
	  echo "       make deploy PRIVILEGED_ACCESS_PAT=ghp_xxx PUBLIC_ACCESS_PAT=ghp_yyy"; \
	  exit 1; \
	}

create-secret: check-pats  ## Create github-tool-secrets with PAT tokens (requires PRIVILEGED_ACCESS_PAT, PUBLIC_ACCESS_PAT)
	@echo "[*] Creating github-tool-secrets ..."
	kubectl create secret generic github-tool-secrets -n $(NAMESPACE) \
	  --from-literal=INIT_AUTH_HEADER="Bearer $(PRIVILEGED_ACCESS_PAT)" \
	  --from-literal=UPSTREAM_HEADER_TO_USE_IF_IN_AUDIENCE="Bearer $(PRIVILEGED_ACCESS_PAT)" \
	  --from-literal=UPSTREAM_HEADER_TO_USE_IF_NOT_IN_AUDIENCE="Bearer $(PUBLIC_ACCESS_PAT)" \
	  --dry-run=client -o yaml | kubectl apply -f -
	@echo "[✓] github-tool-secrets created."

# ---------- Deploy ----------

deploy-tool:  ## Deploy GitHub MCP tool and wait for readiness
	@echo "[*] Deploying GitHub tool ..."
	kubectl apply -f ../k8s/github-tool-deployment.yaml -n $(NAMESPACE)
	kubectl wait --for=condition=available --timeout=120s deployment/github-tool -n $(NAMESPACE)
	@echo "[✓] GitHub tool ready."

deploy-agent:  ## Deploy GitHub Issue Agent (AuthBridge injected by webhook) and wait for readiness
	@echo "[*] Deploying GitHub Issue Agent ..."
	kubectl apply -f ../k8s/git-issue-agent-deployment.yaml -n $(NAMESPACE)
	kubectl wait --for=condition=available --timeout=180s deployment/git-issue-agent -n $(NAMESPACE)
	@echo "[✓] GitHub Issue Agent ready."

deploy: preflight setup-namespace apply-configmaps create-secret deploy-tool deploy-agent  ## Full deploy: namespace + ConfigMaps + PAT secret + tool + agent

# ---------- Validate ----------

validate:  ## Verify pod readiness and operator-managed client registration
	@echo "[*] Pod status in namespace '$(NAMESPACE)':"
	@kubectl get pods -n $(NAMESPACE)
	@echo ""
	@echo "[*] Injected sidecar containers on git-issue-agent:"
	@kubectl get pod -n $(NAMESPACE) -l app.kubernetes.io/name=git-issue-agent \
	  -o jsonpath='{.items[0].spec.containers[*].name}' 2>/dev/null \
	  && echo "" || echo "(pod not found)"
	@echo ""
	@echo "[*] Operator-managed credential Secret mounted into agent:"
	@kubectl get pod -n $(NAMESPACE) -l app.kubernetes.io/name=git-issue-agent \
	  -o jsonpath='{.items[0].spec.volumes[?(@.secret)].secret.secretName}' 2>/dev/null \
	  && echo "" || echo "(pod not found)"
	@echo ""
	@echo "[*] Operator client-registration logs (last 10 relevant lines):"
	@kubectl logs -n rossoctl-system deployment/rossoctl-controller-manager 2>/dev/null \
	  | grep -iE "clientregistration|git-issue-agent" | tail -10 || echo "(operator not reachable)"
	@echo ""
	@echo "[✓] Validation complete — check output above."

# ---------- Test ----------

test-ab:  ## Run inbound validation tests (Steps 8a-8f): public endpoint, no-token, invalid-token, sidecar logs
	@echo "[*] Starting test-client pod ..."
	-kubectl run test-client --image=nicolaka/netshoot -n $(NAMESPACE) --restart=Never \
	  -- sleep 3600 2>/dev/null
	kubectl wait --for=condition=ready pod/test-client -n $(NAMESPACE) --timeout=30s
	@echo ""
	@echo "[*] 8a. Agent card (public endpoint — no token required):"
	kubectl exec test-client -n $(NAMESPACE) -- curl -s \
	  http://git-issue-agent:8080/.well-known/agent.json | grep -o '"name":"[^"]*"'
	@echo ""
	@echo "[*] 8b. Inbound rejection — no token:"
	kubectl exec test-client -n $(NAMESPACE) -- curl -s http://git-issue-agent:8080/
	@echo ""
	@echo "[*] 8c. Inbound rejection — invalid token:"
	kubectl exec test-client -n $(NAMESPACE) -- curl -s \
	  -H "Authorization: Bearer invalid-token" http://git-issue-agent:8080/
	@echo ""
	@echo "[*] 8f. AuthBridge inbound validation logs:"
	$(eval SIDECAR := $(shell kubectl get pod -n $(NAMESPACE) -l app.kubernetes.io/name=git-issue-agent \
	  -o jsonpath='{.items[0].spec.containers[*].name}' 2>/dev/null \
	  | tr ' ' '\n' | grep -E '^(authbridge-proxy|envoy-proxy)$$' | head -1))
	kubectl logs deployment/git-issue-agent -n $(NAMESPACE) -c $(SIDECAR) 2>&1 | grep "\[Inbound\]" || true
	@echo ""
	@echo "[✓] Test run complete."

# ---------- RBAC test ----------

test-rbac:  ## Show scope-based PAT selection logs (Alice vs Bob) — see demo-rbac.md §10 for full interactive steps
	@echo "[*] GitHub tool scope-selection logs (last 20 lines):"
	@kubectl logs deployment/github-tool -n $(NAMESPACE) 2>/dev/null \
	  | grep -E "REQUIRED_SCOPE|scopes" | tail -20 || echo "(no relevant log lines yet)"
	@echo ""
	@printf "To run the full Alice vs Bob test interactively:\n"
	@printf "  \033[1mkubectl run test-client --image=nicolaka/netshoot -n $(NAMESPACE) --restart=Never -- sleep 3600\033[0m\n"
	@printf "  \033[1mkubectl exec -it test-client -n $(NAMESPACE) -- sh\033[0m\n"
	@printf "Then follow Steps 10b–10d from demo-rbac.md.\n"

# ---------- Clean ----------

clean:  ## Delete all deployed resources and the namespace
	@echo "[*] Removing deployments ..."
	-kubectl delete -f ../k8s/git-issue-agent-deployment.yaml -n $(NAMESPACE) 2>/dev/null
	-kubectl delete -f ../k8s/github-tool-deployment.yaml -n $(NAMESPACE) 2>/dev/null
	@echo "[*] Removing secrets and ConfigMaps ..."
	-kubectl delete -f ../k8s/configmaps.yaml -n $(NAMESPACE) 2>/dev/null
	-kubectl delete secret github-tool-secrets -n $(NAMESPACE) 2>/dev/null
	-kubectl delete pod test-client -n $(NAMESPACE) --ignore-not-found 2>/dev/null
	@echo "[✓] Cleanup complete."
