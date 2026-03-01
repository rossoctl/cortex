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
		NamespaceLabel:           LabelNamespaceInject,
		NamespaceAnnotation:      DefaultNamespaceAnnotation,
		Builder:                  NewContainerBuilder(config.CompiledDefaults()),
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
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "test-ns",
			Labels: map[string]string{LabelNamespaceInject: "true"},
		},
	}
	m := newTestMutator(ns)
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		KagentiTypeLabel:      KagentiTypeAgent,
		SpireEnableLabel:      SpireEnabledValue,
		AuthBridgeInjectLabel: AuthBridgeInjectValue,
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
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "test-ns",
			Labels: map[string]string{LabelNamespaceInject: "true"},
		},
	}
	m := newTestMutator(ns)
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		ServiceAccountName: "custom-sa",
	}
	labels := map[string]string{
		KagentiTypeLabel:      KagentiTypeAgent,
		SpireEnableLabel:      SpireEnabledValue,
		AuthBridgeInjectLabel: AuthBridgeInjectValue,
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
	// With opt-in injection, spiffe-helper is injected by default when
	// kagenti.io/inject=enabled. SA is only skipped when spiffe-helper is
	// explicitly opted out via its per-sidecar label.
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "test-ns",
			Labels: map[string]string{LabelNamespaceInject: "true"},
		},
	}
	m := newTestMutator(ns)
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		KagentiTypeLabel:        KagentiTypeAgent,
		AuthBridgeInjectLabel:   AuthBridgeInjectValue,
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

func TestInjectAuthBridge_NoInjectLabel_SkipsInjection(t *testing.T) {
	// Label missing entirely — the most common case for tools that never set
	// kagenti.io/inject. With opt-in semantics this must not inject anything.
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "test-ns",
			Labels: map[string]string{LabelNamespaceInject: "true"},
		},
	}
	m := newTestMutator(ns)
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		KagentiTypeLabel: KagentiTypeTool,
		// kagenti.io/inject deliberately absent
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-tool", labels, nil)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if injected {
		t.Fatal("expected InjectAuthBridge to return false when kagenti.io/inject label is absent")
	}
	if len(podSpec.Containers) != 0 || len(podSpec.InitContainers) != 0 {
		t.Errorf("expected no containers injected, got containers=%v initContainers=%v",
			podSpec.Containers, podSpec.InitContainers)
	}
}

func TestInjectAuthBridge_GlobalOptOut_Agent(t *testing.T) {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "test-ns",
			Labels: map[string]string{LabelNamespaceInject: "true"},
		},
	}
	m := newTestMutator(ns)
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

func TestInjectAuthBridge_GlobalOptOut_Tool(t *testing.T) {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "test-ns",
			Labels: map[string]string{LabelNamespaceInject: "true"},
		},
	}
	m := newTestMutator(ns)
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
		t.Fatal("expected InjectAuthBridge to return false when kagenti.io/inject=disabled")
	}
	if len(podSpec.Containers) != 0 || len(podSpec.InitContainers) != 0 {
		t.Errorf("expected no containers to be injected, got containers=%v initContainers=%v",
			podSpec.Containers, podSpec.InitContainers)
	}
}

func TestInjectAuthBridge_DefaultSAOverridden(t *testing.T) {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "test-ns",
			Labels: map[string]string{LabelNamespaceInject: "true"},
		},
	}
	m := newTestMutator(ns)
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		ServiceAccountName: "default",
	}
	labels := map[string]string{
		KagentiTypeLabel:      KagentiTypeAgent,
		SpireEnableLabel:      SpireEnabledValue,
		AuthBridgeInjectLabel: AuthBridgeInjectValue,
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

func TestInjectAuthBridge_PortExclusionAnnotations(t *testing.T) {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "test-ns",
			Labels: map[string]string{LabelNamespaceInject: "true"},
		},
	}
	m := newTestMutator(ns)
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		KagentiTypeLabel:      KagentiTypeAgent,
		AuthBridgeInjectLabel: AuthBridgeInjectValue,
	}
	annotations := map[string]string{
		OutboundPortsExcludeAnnotation: "443,4317",
		InboundPortsExcludeAnnotation:  "8443,18789",
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", labels, annotations)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if !injected {
		t.Fatal("expected InjectAuthBridge to return true")
	}

	// Find proxy-init init container
	var proxyInit *corev1.Container
	for i := range podSpec.InitContainers {
		if podSpec.InitContainers[i].Name == ProxyInitContainerName {
			proxyInit = &podSpec.InitContainers[i]
			break
		}
	}
	if proxyInit == nil {
		t.Fatal("proxy-init init container not found")
	}

	envMap := make(map[string]string)
	for _, env := range proxyInit.Env {
		envMap[env.Name] = env.Value
	}

	if envMap["OUTBOUND_PORTS_EXCLUDE"] != "8080,443,4317" {
		t.Errorf("OUTBOUND_PORTS_EXCLUDE = %q, want %q", envMap["OUTBOUND_PORTS_EXCLUDE"], "8080,443,4317")
	}
	if envMap["INBOUND_PORTS_EXCLUDE"] != "8443,18789" {
		t.Errorf("INBOUND_PORTS_EXCLUDE = %q, want %q", envMap["INBOUND_PORTS_EXCLUDE"], "8443,18789")
	}
}

func TestInjectAuthBridge_NilAnnotations(t *testing.T) {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "test-ns",
			Labels: map[string]string{LabelNamespaceInject: "true"},
		},
	}
	m := newTestMutator(ns)
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		KagentiTypeLabel:      KagentiTypeAgent,
		AuthBridgeInjectLabel: AuthBridgeInjectValue,
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", labels, nil)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if !injected {
		t.Fatal("expected InjectAuthBridge to return true")
	}

	// Find proxy-init and verify default outbound exclusion only
	for i := range podSpec.InitContainers {
		if podSpec.InitContainers[i].Name == ProxyInitContainerName {
			envMap := make(map[string]string)
			for _, env := range podSpec.InitContainers[i].Env {
				envMap[env.Name] = env.Value
			}
			if envMap["OUTBOUND_PORTS_EXCLUDE"] != "8080" {
				t.Errorf("OUTBOUND_PORTS_EXCLUDE = %q, want %q", envMap["OUTBOUND_PORTS_EXCLUDE"], "8080")
			}
			if _, hasInbound := envMap["INBOUND_PORTS_EXCLUDE"]; hasInbound {
				t.Error("INBOUND_PORTS_EXCLUDE should not be set with nil annotations")
			}
			return
		}
	}
	t.Fatal("proxy-init init container not found")
}
