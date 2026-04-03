/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package injector

import (
	"context"
	"testing"

	"github.com/kagenti/kagenti-extensions/kagenti-webhook/internal/webhook/config"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newTestMutator(objs ...client.Object) *PodMutator {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &PodMutator{
		Client:                   fakeClient,
		EnableClientRegistration: true,
		GetPlatformConfig:        config.CompiledDefaults,
		GetFeatureGates:          config.DefaultFeatureGates,
	}
}

func TestEnsureServiceAccount_CreatesNew(t *testing.T) {
	m := newTestMutator()
	ctx := context.Background()

	if err := m.ensureServiceAccount(ctx, "test-ns", "my-agent"); err != nil {
		t.Fatalf("ensureServiceAccount() returned error: %v", err)
	}

	sa := &corev1.ServiceAccount{}
	if err := m.Client.Get(ctx, client.ObjectKey{Namespace: "test-ns", Name: "my-agent"}, sa); err != nil {
		t.Fatalf("expected ServiceAccount to be created, got error: %v", err)
	}
	if sa.Labels[managedByLabel] != managedByValue {
		t.Errorf("expected label %s=%s, got %s", managedByLabel, managedByValue, sa.Labels[managedByLabel])
	}
}

func TestEnsureServiceAccount_AlreadyExistsWithLabel(t *testing.T) {
	existing := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-agent",
			Namespace: "test-ns",
			Labels:    map[string]string{managedByLabel: managedByValue},
		},
	}
	m := newTestMutator(existing)
	ctx := context.Background()

	if err := m.ensureServiceAccount(ctx, "test-ns", "my-agent"); err != nil {
		t.Fatalf("ensureServiceAccount() returned error: %v", err)
	}
}

func TestEnsureServiceAccount_AlreadyExistsWithoutLabel(t *testing.T) {
	existing := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-agent",
			Namespace: "test-ns",
			Labels:    map[string]string{"app": "something-else"},
		},
	}
	m := newTestMutator(existing)
	ctx := context.Background()

	// Should still succeed (returns nil) but logs a warning internally.
	if err := m.ensureServiceAccount(ctx, "test-ns", "my-agent"); err != nil {
		t.Fatalf("ensureServiceAccount() returned error: %v", err)
	}

	sa := &corev1.ServiceAccount{}
	if err := m.Client.Get(ctx, client.ObjectKey{Namespace: "test-ns", Name: "my-agent"}, sa); err != nil {
		t.Fatalf("expected ServiceAccount to still exist, got error: %v", err)
	}
	if sa.Labels[managedByLabel] == managedByValue {
		t.Error("existing SA should NOT have been updated with the managed-by label")
	}
}

func TestEnsureServiceAccount_AlreadyExistsNoLabels(t *testing.T) {
	existing := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-agent",
			Namespace: "test-ns",
		},
	}
	m := newTestMutator(existing)
	ctx := context.Background()

	if err := m.ensureServiceAccount(ctx, "test-ns", "my-agent"); err != nil {
		t.Fatalf("ensureServiceAccount() returned error: %v", err)
	}
}

func TestInjectAuthBridge_SetsServiceAccountName(t *testing.T) {
	// Agent workloads get envoy/spiffe by default; legacy client-registration sidecar is opt-in.
	m := newTestMutator()
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		KagentiTypeLabel: KagentiTypeAgent,
		AuthModeLabel:    AuthModeSidecar,
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", labels, nil)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if !injected {
		t.Fatal("expected InjectAuthBridge to return true")
	}
	if podSpec.ServiceAccountName != "my-agent" {
		t.Errorf("expected ServiceAccountName=%q, got %q", "my-agent", podSpec.ServiceAccountName)
	}

	sa := &corev1.ServiceAccount{}
	if err := m.Client.Get(ctx, client.ObjectKey{Namespace: "test-ns", Name: "my-agent"}, sa); err != nil {
		t.Fatalf("expected ServiceAccount to be created, got error: %v", err)
	}
}

func TestInjectAuthBridge_RespectsExistingServiceAccountName(t *testing.T) {
	m := newTestMutator()
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		ServiceAccountName: "custom-sa",
	}
	labels := map[string]string{
		KagentiTypeLabel: KagentiTypeAgent,
		AuthModeLabel:    AuthModeSidecar,
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", labels, nil)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if !injected {
		t.Fatal("expected InjectAuthBridge to return true")
	}
	if podSpec.ServiceAccountName != "custom-sa" {
		t.Errorf("expected ServiceAccountName to remain %q, got %q", "custom-sa", podSpec.ServiceAccountName)
	}
}

func TestInjectAuthBridge_NoSACreationWhenSpiffeHelperDisabled(t *testing.T) {
	// Spiffe-helper is injected by default for agents. SA creation is skipped
	// when spiffe-helper is explicitly opted out via its per-sidecar label.
	m := newTestMutator()
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		KagentiTypeLabel:        KagentiTypeAgent,
		AuthModeLabel:           AuthModeSidecar,
		LabelSpiffeHelperInject: "false", // explicitly opt out of spiffe-helper
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", labels, nil)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if !injected {
		t.Fatal("expected InjectAuthBridge to return true (other sidecars still inject)")
	}
	if podSpec.ServiceAccountName != "" {
		t.Errorf("expected ServiceAccountName to be empty when spiffe-helper is disabled, got %q", podSpec.ServiceAccountName)
	}

	sa := &corev1.ServiceAccount{}
	err = m.Client.Get(ctx, client.ObjectKey{Namespace: "test-ns", Name: "my-agent"}, sa)
	if err == nil {
		t.Error("expected ServiceAccount to NOT be created when spiffe-helper is disabled")
	}
}

func TestInjectAuthBridge_Tool_SkipsInjectionByDefault(t *testing.T) {
	// Tool workloads are not injected by default — the injectTools feature gate
	// is false unless explicitly enabled. No inject label needed to confirm this.
	m := newTestMutator()
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		KagentiTypeLabel: KagentiTypeTool,
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-tool", labels, nil)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if injected {
		t.Fatal("expected InjectAuthBridge to return false: injectTools gate is false by default")
	}
	if len(podSpec.Containers) != 0 || len(podSpec.InitContainers) != 0 {
		t.Errorf("expected no containers injected, got containers=%v initContainers=%v",
			podSpec.Containers, podSpec.InitContainers)
	}
}

func TestInjectAuthBridge_GlobalOptOut_Agent(t *testing.T) {
	// Agent workloads are injected by default; kagenti.io/inject=disabled opts out.
	m := newTestMutator()
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		KagentiTypeLabel:      KagentiTypeAgent,
		AuthBridgeInjectLabel: AuthBridgeDisabledValue,
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", labels, nil)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if injected {
		t.Fatal("expected InjectAuthBridge to return false when kagenti.io/inject=disabled")
	}
	if len(podSpec.Containers) != 0 || len(podSpec.InitContainers) != 0 {
		t.Errorf("expected no containers to be injected, got containers=%v initContainers=%v",
			podSpec.Containers, podSpec.InitContainers)
	}
}

func TestInjectAuthBridge_Tool_SkippedByGateRegardlessOfOptOut(t *testing.T) {
	// Tool workloads are blocked by the injectTools gate (false by default)
	// before the opt-out label is even evaluated.
	m := newTestMutator()
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		KagentiTypeLabel:      KagentiTypeTool,
		AuthBridgeInjectLabel: AuthBridgeDisabledValue,
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-tool", labels, nil)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if injected {
		t.Fatal("expected InjectAuthBridge to return false: tool blocked by injectTools gate")
	}
	if len(podSpec.Containers) != 0 || len(podSpec.InitContainers) != 0 {
		t.Errorf("expected no containers to be injected, got containers=%v initContainers=%v",
			podSpec.Containers, podSpec.InitContainers)
	}
}

func TestInjectAuthBridge_DefaultSAOverridden(t *testing.T) {
	m := newTestMutator()
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		ServiceAccountName: "default",
	}
	labels := map[string]string{
		KagentiTypeLabel: KagentiTypeAgent,
		AuthModeLabel:    AuthModeSidecar,
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", labels, nil)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if !injected {
		t.Fatal("expected InjectAuthBridge to return true")
	}
	if podSpec.ServiceAccountName != "my-agent" {
		t.Errorf("expected ServiceAccountName=%q (overriding 'default'), got %q", "my-agent", podSpec.ServiceAccountName)
	}
}

func TestInjectAuthBridge_OutboundPortsExcludeAnnotation(t *testing.T) {
	m := newTestMutator()
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		KagentiTypeLabel: KagentiTypeAgent,
		AuthModeLabel:    AuthModeSidecar,
	}
	annotations := map[string]string{
		OutboundPortsExcludeAnnotation: "11434",
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", labels, annotations)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if !injected {
		t.Fatal("expected InjectAuthBridge to return true")
	}

	for _, ic := range podSpec.InitContainers {
		if ic.Name != ProxyInitContainerName {
			continue
		}
		for _, env := range ic.Env {
			if env.Name == "OUTBOUND_PORTS_EXCLUDE" {
				if env.Value != "8080,11434" {
					t.Errorf("OUTBOUND_PORTS_EXCLUDE = %q, want %q", env.Value, "8080,11434")
				}
				return
			}
		}
		t.Fatal("proxy-init container missing OUTBOUND_PORTS_EXCLUDE env var")
	}
	t.Fatal("proxy-init container not found in initContainers")
}

func TestInjectAuthBridge_InboundPortsExcludeAnnotation(t *testing.T) {
	m := newTestMutator()
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		KagentiTypeLabel: KagentiTypeAgent,
		AuthModeLabel:    AuthModeSidecar,
	}
	annotations := map[string]string{
		OutboundPortsExcludeAnnotation: "11434",
		InboundPortsExcludeAnnotation:  "8443,18789",
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", labels, annotations)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if !injected {
		t.Fatal("expected InjectAuthBridge to return true")
	}

	for _, ic := range podSpec.InitContainers {
		if ic.Name != ProxyInitContainerName {
			continue
		}
		var foundOutbound, foundInbound bool
		for _, env := range ic.Env {
			if env.Name == "OUTBOUND_PORTS_EXCLUDE" {
				foundOutbound = true
				if env.Value != "8080,11434" {
					t.Errorf("OUTBOUND_PORTS_EXCLUDE = %q, want %q", env.Value, "8080,11434")
				}
			}
			if env.Name == "INBOUND_PORTS_EXCLUDE" {
				foundInbound = true
				if env.Value != "8443,18789" {
					t.Errorf("INBOUND_PORTS_EXCLUDE = %q, want %q", env.Value, "8443,18789")
				}
			}
		}
		if !foundOutbound {
			t.Fatal("proxy-init container missing OUTBOUND_PORTS_EXCLUDE env var")
		}
		if !foundInbound {
			t.Fatal("proxy-init container missing INBOUND_PORTS_EXCLUDE env var")
		}
		return
	}
	t.Fatal("proxy-init container not found in initContainers")
}

// ========================================
// Combined sidecar mode tests
// ========================================

func newTestMutatorWithCombinedSidecar(objs ...client.Object) *PodMutator {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &PodMutator{
		Client:                   fakeClient,
		EnableClientRegistration: true,
		GetPlatformConfig:        config.CompiledDefaults,
		GetFeatureGates: func() *config.FeatureGates {
			fg := config.DefaultFeatureGates()
			fg.CombinedSidecar = true
			return fg
		},
	}
}

func TestInjectAuthBridge_CombinedMode_SingleContainer(t *testing.T) {
	m := newTestMutatorWithCombinedSidecar()
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		KagentiTypeLabel: KagentiTypeAgent,
		AuthModeLabel:    AuthModeSidecar,
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", labels, nil)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if !injected {
		t.Fatal("expected InjectAuthBridge to return true")
	}

	// Should have exactly 1 sidecar container (authbridge) — NOT envoy-proxy, spiffe-helper, or client-registration
	if !containerExists(podSpec.Containers, AuthBridgeContainerName) {
		t.Error("expected authbridge container to be injected")
	}
	if containerExists(podSpec.Containers, EnvoyProxyContainerName) {
		t.Error("unexpected envoy-proxy container in combined mode")
	}
	if containerExists(podSpec.Containers, SpiffeHelperContainerName) {
		t.Error("unexpected spiffe-helper container in combined mode")
	}
	if containerExists(podSpec.Containers, ClientRegistrationContainerName) {
		t.Error("unexpected client-registration container in combined mode")
	}

	// Should still have proxy-init
	if !containerExists(podSpec.InitContainers, ProxyInitContainerName) {
		t.Error("expected proxy-init init container to be injected")
	}
}

func TestInjectAuthBridge_CombinedMode_EnvoyDisabled_NoInjection(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	m := &PodMutator{
		Client:                   fakeClient,
		EnableClientRegistration: true,
		GetPlatformConfig:        config.CompiledDefaults,
		GetFeatureGates: func() *config.FeatureGates {
			fg := config.DefaultFeatureGates()
			fg.CombinedSidecar = true
			fg.EnvoyProxy = false
			return fg
		},
	}
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		KagentiTypeLabel: KagentiTypeAgent,
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", labels, nil)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	// With envoy-proxy disabled, spiffe-helper and client-registration may still inject
	// but the combined container should NOT be present
	if containerExists(podSpec.Containers, AuthBridgeContainerName) {
		t.Error("authbridge container should not be injected when envoy-proxy is disabled")
	}
	_ = injected
}

func TestInjectAuthBridge_CombinedMode_SpiffeDisabled_FlagPassed(t *testing.T) {
	m := newTestMutatorWithCombinedSidecar()
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		KagentiTypeLabel:        KagentiTypeAgent,
		AuthModeLabel:           AuthModeSidecar,
		LabelSpiffeHelperInject: "false",
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", labels, nil)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if !injected {
		t.Fatal("expected InjectAuthBridge to return true")
	}

	// authbridge container should be present with SPIRE_ENABLED=false
	if !containerExists(podSpec.Containers, AuthBridgeContainerName) {
		t.Fatal("expected authbridge container to be injected")
	}

	for _, c := range podSpec.Containers {
		if c.Name != AuthBridgeContainerName {
			continue
		}
		for _, env := range c.Env {
			if env.Name == "SPIRE_ENABLED" {
				if env.Value != "false" {
					t.Errorf("SPIRE_ENABLED = %q, want %q", env.Value, "false")
				}
				return
			}
		}
		t.Fatal("missing SPIRE_ENABLED env var on authbridge container")
	}
}

func TestInjectAuthBridge_CombinedMode_Idempotency(t *testing.T) {
	m := newTestMutatorWithCombinedSidecar()
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		Containers: []corev1.Container{
			{Name: AuthBridgeContainerName, Image: "authbridge:test"},
		},
	}
	labels := map[string]string{
		KagentiTypeLabel: KagentiTypeAgent,
		AuthModeLabel:    AuthModeSidecar,
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", labels, nil)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if !injected {
		t.Fatal("expected InjectAuthBridge to return true")
	}

	// Should still be exactly 1 authbridge container
	count := 0
	for _, c := range podSpec.Containers {
		if c.Name == AuthBridgeContainerName {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 authbridge container, got %d", count)
	}
}

func TestInjectAuthBridge_NilAnnotations(t *testing.T) {
	m := newTestMutator()
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		KagentiTypeLabel: KagentiTypeAgent,
		AuthModeLabel:    AuthModeSidecar,
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", labels, nil)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if !injected {
		t.Fatal("expected InjectAuthBridge to return true")
	}

	for _, ic := range podSpec.InitContainers {
		if ic.Name != ProxyInitContainerName {
			continue
		}
		for _, env := range ic.Env {
			if env.Name == "OUTBOUND_PORTS_EXCLUDE" {
				if env.Value != "8080" {
					t.Errorf("OUTBOUND_PORTS_EXCLUDE = %q, want %q (nil annotations should default to 8080 only)", env.Value, "8080")
				}
				return
			}
		}
		t.Fatal("proxy-init container missing OUTBOUND_PORTS_EXCLUDE env var")
	}
	t.Fatal("proxy-init container not found in initContainers")
}
