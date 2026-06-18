# AIAC demo Makefile — AI-generated access control policy workflow.
#
# End-to-end:
#
#   make setup           # provision Keycloak realm (clients, roles, users)
#   make apply-policy    # generate + apply regular_policy.txt (full pipeline)
#   make show-result     # render active composite-role mappings in Keycloak
#   make reset           # wipe generated configs + re-provision realm
#
# Prerequisites:
#   - Python 3.9+ with pip/uv
#   - Keycloak running and accessible
#   - aiac.env configured (copy from aiac.env.TEMPLATE)
#   - aiac_agent/config/llm_conf.yaml configured (copy from llm_conf.yaml.TEMPLATE)

.PHONY: help preflight setup apply-policy apply-permissive show-result reset

# `make` with no target prints help.
.DEFAULT_GOAL := help

help:  ## Show this menu (default)
	@printf "\nAIAC demo — AI-generated access control policies for the GitHub issue agent.\n\n"
	@printf "Typical run:\n"
	@printf "  \033[1mmake setup\033[0m           # provision Keycloak realm\n"
	@printf "  \033[1mmake apply-policy\033[0m    # generate + apply regular policy\n"
	@printf "  \033[1mmake show-result\033[0m     # verify composite roles in Keycloak\n"
	@printf "  \033[1mmake reset\033[0m           # wipe generated configs + re-provision realm\n\n"
	@printf "Targets:\n"
	@awk 'BEGIN { FS = ":.*## " } \
	     /^[a-zA-Z_-]+:.*## / { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 }' \
	     $(MAKEFILE_LIST)
	@printf "\nVariables:\n"
	@printf "  \033[36m%-20s\033[0m %s\n" "POLICY" "policy file to apply (default: policies/regular_policy.txt)"
	@printf "  \033[36m%-20s\033[0m %s\n" "PYTHON"  "Python interpreter (default: python3)"
	@printf "\nPrerequisites: Python 3.9+, Keycloak running, aiac.env and llm_conf.yaml configured.\n\n"

POLICY ?= policies/regular_policy.txt
PYTHON ?= python3

# ---------- Pre-flight ----------

preflight:  ## Verify prerequisites (Python, aiac.env, llm_conf.yaml, Keycloak)
	@$(PYTHON) --version >/dev/null 2>&1 || { \
	  echo "ERROR: $(PYTHON) not found."; exit 1; \
	}
	@test -f aiac.env || { \
	  echo "ERROR: aiac.env not found."; \
	  echo "       Copy aiac.env.TEMPLATE to aiac.env and fill in your Keycloak credentials."; \
	  exit 1; \
	}
	@test -f aiac_agent/config/llm_conf.yaml || { \
	  echo "ERROR: aiac_agent/config/llm_conf.yaml not found."; \
	  echo "       Copy aiac_agent/config/llm_conf.yaml.TEMPLATE and configure your LLM."; \
	  exit 1; \
	}
	@$(PYTHON) -c 'import dotenv, yaml, keycloak, langgraph' 2>/dev/null || { \
	  echo "ERROR: Python dependencies missing."; \
	  echo "       Run: pip install -r ../../../requirements.txt"; \
	  exit 1; \
	}
	@KEYCLOAK_URL=$$(grep -E '^KEYCLOAK_URL=' aiac.env | cut -d= -f2-); \
	 curl -sf "$$KEYCLOAK_URL/realms/master" -o /dev/null || { \
	   echo "ERROR: Keycloak not reachable at $$KEYCLOAK_URL"; \
	   echo "       Ensure Keycloak is running and KEYCLOAK_URL in aiac.env is correct."; \
	   exit 1; \
	 }
	@echo "[✓] All prerequisites met."

# ---------- Setup ----------

setup: preflight  ## Provision Keycloak realm with clients, roles, and users
	@echo "[*] Provisioning Keycloak realm ..."
	$(PYTHON) ../setup_keycloak.py -rbac config.yaml
	@echo "[✓] Keycloak realm provisioned."

# ---------- Apply policy ----------

apply-policy: preflight  ## Generate + apply POLICY (default: policies/regular_policy.txt) to Keycloak
	@echo "[*] Running AIAC full pipeline: $(POLICY)"
	$(PYTHON) aiac_cli.py --yes $(POLICY)

apply-permissive: POLICY = policies/permissive_policy.txt
apply-permissive: apply-policy  ## Generate + apply the permissive policy (grants Sales access)

# ---------- Show result ----------

show-result: preflight  ## Show active composite-role mappings in Keycloak (verify applied policy)
	$(PYTHON) scripts/show-result.py

# ---------- Reset ----------

reset: preflight  ## Wipe generated configs and re-provision the realm from scratch
	@echo "[*] Removing generated configs ..."
	@rm -f generated_configs/*.yaml
	@echo "[*] Re-provisioning Keycloak realm ..."
	$(PYTHON) ../setup_keycloak.py -rbac config.yaml
	@echo "[✓] Reset complete."
