#!/usr/bin/env python3
"""
Webhook Injection E2E Tests

Validates that the kagenti-webhook correctly injects AuthBridge sidecars
into pods based on the kagenti.io/type label.

Injection decision (from pod_mutator.go):
  - `kagenti.io/type: agent` → inject all sidecars (default feature gates)
  - `kagenti.io/type: tool`  → skip injection (InjectTools=false by default)
  - `kagenti.io/inject: disabled` → skip injection (whole-workload opt-out)
  - No label → skip injection

Injected containers for agent workloads (default feature gates):
  Init:    proxy-init
  Sidecars: envoy-proxy, spiffe-helper

Legacy `kagenti-client-registration` is opt-in via
`kagenti.io/client-registration-inject: "true"` (default is operator-managed).

Note: Injected pods may remain in Pending state (sidecars can't start without
Keycloak/SPIRE), but the API server stores and returns the mutated pod spec.
Tests check the spec, not the runtime state.

Usage:
    pytest tests/e2e/test_webhook_injection.py -v
"""

import uuid

import pytest
from kubernetes import client
from kubernetes.client.rest import ApiException

# Container names as defined in kagenti-webhook/internal/webhook/injector/
_ENVOY_PROXY = "envoy-proxy"
_PROXY_INIT = "proxy-init"
_SPIFFE_HELPER = "spiffe-helper"
_CLIENT_REGISTRATION = "kagenti-client-registration"

# Default agent injection (envoy + spiffe); legacy client-registration is opt-in.
_DEFAULT_AGENT_SIDECARS = {_ENVOY_PROXY, _SPIFFE_HELPER}
# Any sidecar the webhook may inject (for opt-out / idempotency assertions).
_ALL_POSSIBLE_SIDECARS = _DEFAULT_AGENT_SIDECARS | {_CLIENT_REGISTRATION}
_ALL_INIT = {_PROXY_INIT}

# Labels
_TYPE_LABEL = "kagenti.io/type"
_INJECT_LABEL = "kagenti.io/inject"
_CLIENT_REGISTRATION_INJECT_LABEL = "kagenti.io/client-registration-inject"
_AUTH_MODE_LABEL = "kagenti.io/auth-mode"

# App container included in every test pod
_APP_CONTAINER = "app"


def _make_pod_name(prefix):
    """Generate a unique pod name to avoid conflicts between test runs."""
    return f"{prefix}-{uuid.uuid4().hex[:8]}"


def _build_test_pod(name, labels):
    """Build a minimal pod spec with the given labels."""
    return client.V1Pod(
        metadata=client.V1ObjectMeta(
            name=name,
            labels=labels,
        ),
        spec=client.V1PodSpec(
            # Use a minimal image; pod won't start but injection happens at admission
            containers=[
                client.V1Container(
                    name=_APP_CONTAINER,
                    image="busybox:1.37",
                    command=["sh", "-c", "sleep 3600"],
                )
            ],
            # Prevent the pod from being scheduled (saves resources in CI)
            node_selector={"non-existent-node": "true"},
        ),
    )


class TestAgentSidecarInjection:
    """
    Verify that pods with kagenti.io/type=agent receive full sidecar injection.

    The webhook intercepts Pod CREATE at admission time and injects containers
    into the pod spec before it is stored. The API response contains the
    mutated spec, which is what we assert against.
    """

    def test_agent_pod_gets_envoy_proxy(self, k8s_client, test_namespace):
        """Agent pod must have envoy-proxy sidecar injected."""
        name = _make_pod_name("agent-envoy")
        pod = _build_test_pod(name, {_TYPE_LABEL: "agent", _AUTH_MODE_LABEL: "sidecar"})

        try:
            created = k8s_client.create_namespaced_pod(namespace=test_namespace, body=pod)
        except ApiException as e:
            pytest.fail(
                f"Pod creation failed (webhook may be down): {e}\n"
                "Check: kubectl get pods -n kagenti-webhook-system"
            )
        finally:
            try:
                k8s_client.delete_namespaced_pod(name=name, namespace=test_namespace)
            except ApiException:
                pass

        container_names = {c.name for c in created.spec.containers}
        assert _ENVOY_PROXY in container_names, (
            f"envoy-proxy not injected into agent pod.\n"
            f"Containers: {sorted(container_names)}\n"
            "Check: webhook logs for injection decision"
        )

    def test_agent_pod_gets_proxy_init(self, k8s_client, test_namespace):
        """Agent pod must have proxy-init init container injected (mirrors envoy-proxy)."""
        name = _make_pod_name("agent-init")
        pod = _build_test_pod(name, {_TYPE_LABEL: "agent", _AUTH_MODE_LABEL: "sidecar"})

        try:
            created = k8s_client.create_namespaced_pod(namespace=test_namespace, body=pod)
        except ApiException as e:
            pytest.fail(f"Pod creation failed: {e}")
        finally:
            try:
                k8s_client.delete_namespaced_pod(name=name, namespace=test_namespace)
            except ApiException:
                pass

        init_names = {c.name for c in (created.spec.init_containers or [])}
        assert _PROXY_INIT in init_names, (
            f"proxy-init not injected as init container.\n"
            f"Init containers: {sorted(init_names)}"
        )

    def test_agent_pod_gets_spiffe_helper(self, k8s_client, test_namespace):
        """Agent pod must have spiffe-helper sidecar injected."""
        name = _make_pod_name("agent-spiffe")
        pod = _build_test_pod(name, {_TYPE_LABEL: "agent", _AUTH_MODE_LABEL: "sidecar"})

        try:
            created = k8s_client.create_namespaced_pod(namespace=test_namespace, body=pod)
        except ApiException as e:
            pytest.fail(f"Pod creation failed: {e}")
        finally:
            try:
                k8s_client.delete_namespaced_pod(name=name, namespace=test_namespace)
            except ApiException:
                pass

        container_names = {c.name for c in created.spec.containers}
        assert _SPIFFE_HELPER in container_names, (
            f"spiffe-helper not injected into agent pod.\n"
            f"Containers: {sorted(container_names)}"
        )

    def test_agent_pod_has_no_client_registration_without_opt_in(
        self, k8s_client, test_namespace
    ):
        """Default agent pods do not get the legacy kagenti-client-registration sidecar."""
        name = _make_pod_name("agent-noclientreg")
        pod = _build_test_pod(name, {_TYPE_LABEL: "agent", _AUTH_MODE_LABEL: "sidecar"})

        try:
            created = k8s_client.create_namespaced_pod(namespace=test_namespace, body=pod)
        except ApiException as e:
            pytest.fail(f"Pod creation failed: {e}")
        finally:
            try:
                k8s_client.delete_namespaced_pod(name=name, namespace=test_namespace)
            except ApiException:
                pass

        container_names = {c.name for c in created.spec.containers}
        assert _CLIENT_REGISTRATION not in container_names, (
            f"legacy kagenti-client-registration injected without opt-in label.\n"
            f"Containers: {sorted(container_names)}"
        )

    def test_agent_pod_gets_client_registration_when_opt_in(self, k8s_client, test_namespace):
        """With kagenti.io/client-registration-inject=true, legacy sidecar is injected."""
        name = _make_pod_name("agent-clientreg-optin")
        pod = _build_test_pod(
            name,
            {
                _TYPE_LABEL: "agent",
                _AUTH_MODE_LABEL: "sidecar",
                _CLIENT_REGISTRATION_INJECT_LABEL: "true",
            },
        )

        try:
            created = k8s_client.create_namespaced_pod(namespace=test_namespace, body=pod)
        except ApiException as e:
            pytest.fail(f"Pod creation failed: {e}")
        finally:
            try:
                k8s_client.delete_namespaced_pod(name=name, namespace=test_namespace)
            except ApiException:
                pass

        container_names = {c.name for c in created.spec.containers}
        assert _CLIENT_REGISTRATION in container_names, (
            f"kagenti-client-registration not injected with opt-in label.\n"
            f"Containers: {sorted(container_names)}"
        )

    def test_agent_pod_gets_all_sidecars(self, k8s_client, test_namespace):
        """Agent pod must have default injected containers (envoy, spiffe, proxy-init)."""
        name = _make_pod_name("agent-all")
        pod = _build_test_pod(name, {_TYPE_LABEL: "agent", _AUTH_MODE_LABEL: "sidecar"})

        try:
            created = k8s_client.create_namespaced_pod(namespace=test_namespace, body=pod)
        except ApiException as e:
            pytest.fail(f"Pod creation failed: {e}")
        finally:
            try:
                k8s_client.delete_namespaced_pod(name=name, namespace=test_namespace)
            except ApiException:
                pass

        container_names = {c.name for c in created.spec.containers}
        init_names = {c.name for c in (created.spec.init_containers or [])}
        missing_sidecars = _DEFAULT_AGENT_SIDECARS - container_names
        missing_init = _ALL_INIT - init_names

        assert not missing_sidecars and not missing_init, (
            f"Agent pod is missing injected containers.\n"
            f"Missing sidecars: {missing_sidecars}\n"
            f"Missing init containers: {missing_init}\n"
            f"Present sidecars: {sorted(container_names)}\n"
            f"Present init: {sorted(init_names)}"
        )

    def test_agent_pod_keeps_app_container(self, k8s_client, test_namespace):
        """Injection must not remove the original application container."""
        name = _make_pod_name("agent-app")
        pod = _build_test_pod(name, {_TYPE_LABEL: "agent", _AUTH_MODE_LABEL: "sidecar"})

        try:
            created = k8s_client.create_namespaced_pod(namespace=test_namespace, body=pod)
        except ApiException as e:
            pytest.fail(f"Pod creation failed: {e}")
        finally:
            try:
                k8s_client.delete_namespaced_pod(name=name, namespace=test_namespace)
            except ApiException:
                pass

        container_names = {c.name for c in created.spec.containers}
        assert _APP_CONTAINER in container_names, (
            f"Original app container was removed by webhook!\n"
            f"Containers: {sorted(container_names)}"
        )


class TestInjectionOptOut:
    """
    Verify that injection is correctly skipped based on labels and type.

    The webhook must NOT inject when:
    - `kagenti.io/inject: disabled` is set (whole-workload opt-out)
    - `kagenti.io/type: tool` (InjectTools=false by default)
    - No type label is set
    """

    def _assert_not_injected(self, created_pod):
        """Assert that none of the AuthBridge containers are in the pod spec."""
        container_names = {c.name for c in created_pod.spec.containers}
        init_names = {c.name for c in (created_pod.spec.init_containers or [])}
        injected = (_ALL_POSSIBLE_SIDECARS & container_names) | (_ALL_INIT & init_names)
        assert not injected, (
            f"Expected no AuthBridge injection, but found: {injected}\n"
            f"Sidecars: {sorted(container_names)}\n"
            f"Init: {sorted(init_names)}"
        )

    def test_inject_disabled_label_skips_injection(self, k8s_client, test_namespace):
        """
        Pod with kagenti.io/inject=disabled must not receive sidecars.

        This is the whole-workload opt-out mechanism.
        """
        name = _make_pod_name("optout-disabled")
        pod = _build_test_pod(name, {_TYPE_LABEL: "agent", _INJECT_LABEL: "disabled"})

        try:
            created = k8s_client.create_namespaced_pod(namespace=test_namespace, body=pod)
        except ApiException as e:
            pytest.fail(f"Pod creation failed: {e}")
        finally:
            try:
                k8s_client.delete_namespaced_pod(name=name, namespace=test_namespace)
            except ApiException:
                pass

        self._assert_not_injected(created)

    def test_tool_pod_not_injected_by_default(self, k8s_client, test_namespace):
        """
        Pod with kagenti.io/type=tool must NOT receive sidecars.

        InjectTools feature gate defaults to false, so tool workloads are
        skipped by default. Operators must explicitly enable it.
        """
        name = _make_pod_name("tool-noinject")
        pod = _build_test_pod(name, {_TYPE_LABEL: "tool"})

        try:
            created = k8s_client.create_namespaced_pod(namespace=test_namespace, body=pod)
        except ApiException as e:
            pytest.fail(f"Pod creation failed: {e}")
        finally:
            try:
                k8s_client.delete_namespaced_pod(name=name, namespace=test_namespace)
            except ApiException:
                pass

        self._assert_not_injected(created)

    def test_unlabeled_pod_not_injected(self, k8s_client, test_namespace):
        """
        Pod with no kagenti labels must not receive sidecars.

        The webhook pre-filter requires kagenti.io/type=agent|tool. Without
        this label, the webhook handler returns the pod unchanged.
        """
        name = _make_pod_name("nolabel")
        pod = _build_test_pod(name, {"app": "test-nolabel"})

        try:
            created = k8s_client.create_namespaced_pod(namespace=test_namespace, body=pod)
        except ApiException as e:
            pytest.fail(f"Pod creation failed: {e}")
        finally:
            try:
                k8s_client.delete_namespaced_pod(name=name, namespace=test_namespace)
            except ApiException:
                pass

        self._assert_not_injected(created)


class TestInjectionIdempotency:
    """Verify that the webhook is idempotent (no double injection on re-invocation)."""

    def test_already_injected_pod_not_double_injected(self, k8s_client, test_namespace):
        """
        A pod that already has AuthBridge containers must not get them again.

        The webhook's isAlreadyInjected() check detects existing sidecars and
        skips re-injection. This prevents duplicate containers on re-invocation
        (reinvocationPolicy: IfNeeded).
        """
        name = _make_pod_name("agent-idempotent")
        # Pre-populate pod spec with one of the injected containers
        pod = client.V1Pod(
            metadata=client.V1ObjectMeta(
                name=name,
                labels={_TYPE_LABEL: "agent", _AUTH_MODE_LABEL: "sidecar"},
            ),
            spec=client.V1PodSpec(
                containers=[
                    client.V1Container(
                        name=_APP_CONTAINER,
                        image="busybox:1.37",
                        command=["sh", "-c", "sleep 3600"],
                    ),
                    # Pre-existing envoy-proxy triggers isAlreadyInjected()
                    client.V1Container(
                        name=_ENVOY_PROXY,
                        image="envoyproxy/envoy:v1.28-latest",
                    ),
                ],
                node_selector={"non-existent-node": "true"},
            ),
        )

        try:
            created = k8s_client.create_namespaced_pod(namespace=test_namespace, body=pod)
        except ApiException as e:
            pytest.fail(f"Pod creation failed: {e}")
        finally:
            try:
                k8s_client.delete_namespaced_pod(name=name, namespace=test_namespace)
            except ApiException:
                pass

        # Count envoy-proxy containers - should be exactly 1 (no duplicate)
        envoy_count = sum(1 for c in created.spec.containers if c.name == _ENVOY_PROXY)
        assert envoy_count == 1, (
            f"Expected exactly 1 envoy-proxy container, found {envoy_count}.\n"
            "Webhook may have double-injected (idempotency check failed)."
        )


if __name__ == "__main__":
    import sys

    sys.exit(pytest.main([__file__, "-v"]))
