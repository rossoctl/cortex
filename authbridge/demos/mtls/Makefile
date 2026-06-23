.DEFAULT_GOAL := help

NAMESPACE      ?= team1
CALLER         ?= mtls-caller
CALLEE         ?= mtls-callee
CALLER_ENVOY   ?= mtls-caller-envoy
CALLEE_ENVOY   ?= mtls-callee-envoy

.PHONY: help
help:
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-32s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: wait-operator-labels
wait-operator-labels: ## Wait for the operator to apply kagenti.io/type from AgentRuntime CRs
	@for deploy in $(CALLER) $(CALLEE); do \
	  echo "[*] Waiting for operator to label $$deploy ..."; \
	  for i in $$(seq 1 60); do \
	    val=$$(kubectl -n $(NAMESPACE) get deploy/$$deploy \
	      -o jsonpath="{.spec.template.metadata.labels['kagenti.io/type']}" 2>/dev/null); \
	    if [ "$$val" = "agent" ]; then echo "    ✓ $$deploy labeled"; break; fi; \
	    sleep 2; \
	  done; \
	  val=$$(kubectl -n $(NAMESPACE) get deploy/$$deploy \
	    -o jsonpath="{.spec.template.metadata.labels['kagenti.io/type']}" 2>/dev/null); \
	  if [ "$$val" != "agent" ]; then \
	    echo "ERROR: operator did not label $$deploy within 120s"; exit 1; \
	  fi; \
	done

.PHONY: deploy
deploy: ## Deploy caller + callee pods (both with mtls.mode: strict)
	kubectl apply -f k8s/caller.yaml
	kubectl apply -f k8s/callee.yaml
	$(MAKE) wait-operator-labels
	./scripts/patch-mtls-config.sh $(NAMESPACE) strict
	kubectl -n $(NAMESPACE) rollout restart deploy/$(CALLER) deploy/$(CALLEE)
	kubectl -n $(NAMESPACE) rollout status deploy/$(CALLER) deploy/$(CALLEE) --timeout=120s

.PHONY: demo-mtls
demo-mtls: deploy ## Deploy + run the encryption-on-the-wire check (strict mode)
	./scripts/verify-encrypted.sh $(NAMESPACE) $(CALLER) $(CALLEE)

.PHONY: demo-mtls-permissive
demo-mtls-permissive: ## Same shape but with mtls.mode: permissive; verify mixed-mode serving
	kubectl apply -f k8s/caller.yaml
	kubectl apply -f k8s/callee.yaml
	$(MAKE) wait-operator-labels
	./scripts/patch-mtls-config.sh $(NAMESPACE) permissive
	kubectl -n $(NAMESPACE) rollout restart deploy/$(CALLER) deploy/$(CALLEE)
	kubectl -n $(NAMESPACE) rollout status deploy/$(CALLER) deploy/$(CALLEE) --timeout=120s
	./scripts/verify-permissive.sh $(NAMESPACE) $(CALLEE)

.PHONY: demo-mtls-strict-rejects-plain
demo-mtls-strict-rejects-plain: deploy ## Verify that plain HTTP gets dropped on a strict callee
	./scripts/verify-strict-rejects-plain.sh $(NAMESPACE) $(CALLEE)

.PHONY: undeploy
undeploy: ## Tear down the demo's resources
	kubectl delete -f k8s/caller.yaml --ignore-not-found
	kubectl delete -f k8s/callee.yaml --ignore-not-found

# ---------------------------------------------------------------------------
# envoy-sidecar variant
#
# Same contract as the proxy-sidecar targets above, but the demo pods
# are fully hand-crafted (no kagenti.io/inject label — see the demo
# README's "How the demo wires mTLS" section for why) and mTLS is
# configured in the Envoy data plane (k8s/envoy-config-mtls.yaml)
# rather than at the authbridge-runtime layer.
#
# These targets manage demo-scoped ConfigMaps
# (envoy-config-mtls-{permissive,strict,active} and
# authbridge-runtime-mtls) via scripts/swap-envoy-config.sh.
# undeploy-envoy clean-deletes them; nothing in the namespace's
# operator-managed `envoy-config` is touched. Avoid running multiple
# envoy-sidecar demo variants concurrently in the same namespace —
# they share the active CM.
# ---------------------------------------------------------------------------

.PHONY: deploy-envoy-strict
deploy-envoy-strict: ## Deploy envoy-sidecar variant with strict mTLS (inbound TLS-only, outbound TLS-or-fail)
	kubectl apply -f k8s/caller-envoy.yaml
	kubectl apply -f k8s/callee-envoy.yaml
	./scripts/swap-envoy-config.sh $(NAMESPACE) strict
	kubectl -n $(NAMESPACE) rollout restart deploy/$(CALLER_ENVOY) deploy/$(CALLEE_ENVOY)
	kubectl -n $(NAMESPACE) rollout status  deploy/$(CALLER_ENVOY) deploy/$(CALLEE_ENVOY) --timeout=120s

.PHONY: deploy-envoy-permissive
deploy-envoy-permissive: ## Deploy envoy-sidecar variant with permissive mTLS (inbound peeks, outbound plaintext)
	kubectl apply -f k8s/caller-envoy.yaml
	kubectl apply -f k8s/callee-envoy.yaml
	./scripts/swap-envoy-config.sh $(NAMESPACE) permissive
	kubectl -n $(NAMESPACE) rollout restart deploy/$(CALLER_ENVOY) deploy/$(CALLEE_ENVOY)
	kubectl -n $(NAMESPACE) rollout status  deploy/$(CALLER_ENVOY) deploy/$(CALLEE_ENVOY) --timeout=120s

.PHONY: demo-mtls-envoy
demo-mtls-envoy: deploy-envoy-strict ## Deploy + verify envoy-sidecar mTLS (strict)
	./scripts/verify-encrypted-envoy.sh $(NAMESPACE) $(CALLER_ENVOY) $(CALLEE_ENVOY)

.PHONY: demo-mtls-envoy-permissive
demo-mtls-envoy-permissive: deploy-envoy-permissive ## Deploy + verify envoy-sidecar permissive mode (plaintext callers served)
	./scripts/verify-permissive-envoy.sh $(NAMESPACE) $(CALLEE_ENVOY)

.PHONY: demo-mtls-envoy-strict-rejects-plain
demo-mtls-envoy-strict-rejects-plain: deploy-envoy-strict ## Verify envoy-sidecar strict mode rejects plaintext callers
	./scripts/verify-strict-rejects-plain-envoy.sh $(NAMESPACE) $(CALLEE_ENVOY)

.PHONY: undeploy-envoy
undeploy-envoy: ## Tear down envoy-sidecar variant + clean up demo CMs
	kubectl delete -f k8s/caller-envoy.yaml --ignore-not-found
	kubectl delete -f k8s/callee-envoy.yaml --ignore-not-found
	./scripts/swap-envoy-config.sh $(NAMESPACE) clean || true
