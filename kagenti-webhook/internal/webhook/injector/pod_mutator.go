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
	"fmt"

	"github.com/kagenti/kagenti-extensions/kagenti-webhook/internal/webhook/config"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var mutatorLog = logf.Log.WithName("pod-mutator")

const (
	// Container names
	SpiffeHelperContainerName       = "spiffe-helper"
	ClientRegistrationContainerName = "kagenti-client-registration"

	// Default configuration (deprecated paths use these directly)
	DefaultNamespaceAnnotation = "kagenti.dev/inject"
	DefaultCRAnnotation        = "kagenti.dev/inject"
	// Label selector for authbridge injection.
	// Injection uses opt-in semantics: only AuthBridgeInjectValue triggers
	// injection; any other value (including AuthBridgeDisabledValue, absent,
	// or unrecognised) skips injection. AuthBridgeDisabledValue is a
	// conventional opt-out spelling — it is not special-cased in code.
	AuthBridgeInjectLabel   = "kagenti.io/inject"
	AuthBridgeInjectValue   = "enabled"
	AuthBridgeDisabledValue = "disabled"

	// Label selector for SPIRE enablement
	SpireEnableLabel   = "kagenti.io/spire"
	SpireEnabledValue  = "enabled"
	SpireDisabledValue = "disabled"
	// Port exclusion annotations — per-pod overrides for proxy-init iptables rules
	OutboundPortsExcludeAnnotation = "kagenti.io/outbound-ports-exclude"
	InboundPortsExcludeAnnotation  = "kagenti.io/inbound-ports-exclude"

	// Istio exclusion annotations
	IstioSidecarInjectAnnotation = "sidecar.istio.io/inject"
	AmbientRedirectionAnnotation = "ambient.istio.io/redirection"

	// KagentiTypeLabel is the label key that identifies the workload type
	KagentiTypeLabel = "kagenti.io/type"
	// KagentiTypeAgent is the label value that identifies agent workloads
	KagentiTypeAgent = "agent"
	// KagentiTypeTool is the label value that identifies tool workloads
	KagentiTypeTool = "tool"
)

type PodMutator struct {
	Client                   client.Client
	EnableClientRegistration bool
	NamespaceLabel           string
	NamespaceAnnotation      string
	Builder                  *ContainerBuilder
	// Getter functions for hot-reloadable config (used by precedence evaluator)
	GetPlatformConfig func() *config.PlatformConfig
	GetFeatureGates   func() *config.FeatureGates
}

func NewPodMutator(
	client client.Client,
	enableClientRegistration bool,
	getPlatformConfig func() *config.PlatformConfig,
	getFeatureGates func() *config.FeatureGates,
) *PodMutator {
	cfg := getPlatformConfig()
	return &PodMutator{
		Client:                   client,
		EnableClientRegistration: enableClientRegistration,
		NamespaceLabel:           LabelNamespaceInject,
		NamespaceAnnotation:      DefaultNamespaceAnnotation,
		Builder:                  NewContainerBuilder(cfg),
		GetPlatformConfig:        getPlatformConfig,
		GetFeatureGates:          getFeatureGates,
	}
}

// DEPRECATED, used by Agent CR. Remove ShouldMutate after Agent CR is deleted and use InjectAuthBridge instead.

// main entry point for pod mutations
// It checks if injection should occur and performs all necessary mutations
func (m *PodMutator) MutatePodSpec(ctx context.Context, podSpec *corev1.PodSpec, namespace, crName string, crAnnotations map[string]string) error {
	mutatorLog.Info("MutatePodSpec called", "namespace", namespace, "crName", crName, "annotations", crAnnotations)

	shouldMutate, err := m.ShouldMutate(ctx, namespace, crAnnotations)
	if err != nil {
		mutatorLog.Error(err, "Failed to determine if mutation should occur", "namespace", namespace, "crName", crName)
		return fmt.Errorf("failed to determine if mutation should occur: %w", err)
	}

	if !shouldMutate {
		mutatorLog.Info("Skipping mutation (injection not enabled)", "namespace", namespace, "crName", crName)
		return nil // Skip mutation
	}

	mutatorLog.Info("Mutation enabled - injecting sidecars and volumes", "namespace", namespace, "crName", crName)

	if err := m.InjectSidecars(podSpec, namespace, crName); err != nil {
		mutatorLog.Error(err, "Failed to inject sidecars", "namespace", namespace, "crName", crName)
		return fmt.Errorf("failed to inject sidecars: %w", err)
	}

	if err := m.InjectVolumes(podSpec); err != nil {
		mutatorLog.Error(err, "Failed to inject volumes", "namespace", namespace, "crName", crName)
		return fmt.Errorf("failed to inject volumes: %w", err)
	}

	mutatorLog.Info("Successfully mutated pod spec", "namespace", namespace, "crName", crName, "containers", len(podSpec.Containers), "volumes", len(podSpec.Volumes))
	return nil
}

// InjectAuthBridge evaluates the multi-layer precedence chain and conditionally injects sidecars.
// annotations carries per-pod overrides such as kagenti.io/outbound-ports-exclude and
// kagenti.io/inbound-ports-exclude which are forwarded to proxy-init for iptables configuration.
func (m *PodMutator) InjectAuthBridge(ctx context.Context, podSpec *corev1.PodSpec, namespace, crName string, labels, annotations map[string]string) (bool, error) {
	mutatorLog.Info("InjectAuthBridge called", "namespace", namespace, "crName", crName, "labels", labels)

	// Pre-filter: only agent/tool workloads are eligible
	kagentiType, hasKagentiLabel := labels[KagentiTypeLabel]
	if !hasKagentiLabel || (kagentiType != KagentiTypeAgent && kagentiType != KagentiTypeTool) {
		mutatorLog.Info("Skipping mutation: workload is not an agent or a tool",
			"hasLabel", hasKagentiLabel,
			"labelValue", kagentiType)
		return false, nil
	}

	// Opt-in: injection only proceeds when kagenti.io/inject=enabled is
	// explicitly set on the workload. A missing label or any other value
	// (including "disabled") skips injection. This prevents sidecars from
	// being injected into workloads that never requested them — consistent
	// with the existing opt-in behaviour of kagenti.io/spire=enabled.
	if labels[AuthBridgeInjectLabel] != AuthBridgeInjectValue {
		mutatorLog.Info("Skipping mutation: kagenti.io/inject not set to enabled",
			"namespace", namespace, "crName", crName,
			"value", labels[AuthBridgeInjectLabel])
		return false, nil
	}

	// Fetch namespace labels for the precedence evaluator
	ns := &corev1.Namespace{}
	if err := m.Client.Get(ctx, client.ObjectKey{Name: namespace}, ns); err != nil {
		mutatorLog.Error(err, "Failed to fetch namespace", "namespace", namespace)
		return false, fmt.Errorf("failed to fetch namespace: %w", err)
	}

	// Get fresh config snapshots for this request (hot-reloadable)
	currentConfig := m.GetPlatformConfig()
	currentGates := m.GetFeatureGates()

	// Evaluate the precedence chain
	evaluator := NewPrecedenceEvaluator(currentGates, currentConfig)
	decision := evaluator.Evaluate(ns.Labels, labels, nil)

	// Log each sidecar decision
	for _, d := range []struct {
		name string
		sd   SidecarDecision
	}{
		{"envoy-proxy", decision.EnvoyProxy},
		{"proxy-init", decision.ProxyInit},
		{"spiffe-helper", decision.SpiffeHelper},
		{"client-registration", decision.ClientRegistration},
	} {
		mutatorLog.Info("injection decision",
			"sidecar", d.name,
			"inject", d.sd.Inject,
			"reason", d.sd.Reason,
			"layer", d.sd.Layer,
		)
	}

	if !decision.AnyInjected() {
		mutatorLog.Info("Skipping mutation (no sidecars to inject)", "namespace", namespace, "crName", crName)
		return false, nil
	}

	// Derive SPIRE mode from the injection decision: if spiffe-helper is being
	// injected then SPIRE volumes and a dedicated ServiceAccount are needed.
	spireEnabled := decision.SpiffeHelper.Inject

	// When SPIRE is enabled, ensure a dedicated ServiceAccount exists so
	// the SPIFFE ID reflects the workload name instead of "default".
	if spireEnabled && (podSpec.ServiceAccountName == "" || podSpec.ServiceAccountName == "default") {
		if err := m.ensureServiceAccount(ctx, namespace, crName); err != nil {
			mutatorLog.Error(err, "Failed to ensure ServiceAccount", "namespace", namespace, "name", crName)
			return false, fmt.Errorf("failed to ensure service account: %w", err)
		}
		podSpec.ServiceAccountName = crName
		mutatorLog.Info("Set ServiceAccountName for SPIRE identity", "namespace", namespace, "serviceAccount", crName)
	}

	// Initialize slices
	if podSpec.Containers == nil {
		podSpec.Containers = []corev1.Container{}
	}
	if podSpec.InitContainers == nil {
		podSpec.InitContainers = []corev1.Container{}
	}
	if podSpec.Volumes == nil {
		podSpec.Volumes = []corev1.Volume{}
	}

	// Build containers using fresh config (picks up hot-reloaded images/resources)
	builder := NewContainerBuilder(currentConfig)

	// Conditionally inject sidecars based on precedence decisions
	if decision.EnvoyProxy.Inject && !containerExists(podSpec.Containers, EnvoyProxyContainerName) {
		podSpec.Containers = append(podSpec.Containers, builder.BuildEnvoyProxyContainerWithSpireOption(spireEnabled))
	}

	if decision.ProxyInit.Inject && !containerExists(podSpec.InitContainers, ProxyInitContainerName) {
		outboundExclude := annotations[OutboundPortsExcludeAnnotation]
		inboundExclude := annotations[InboundPortsExcludeAnnotation]
		podSpec.InitContainers = append(podSpec.InitContainers, builder.BuildProxyInitContainer(outboundExclude, inboundExclude))
	}

	if decision.SpiffeHelper.Inject && !containerExists(podSpec.Containers, SpiffeHelperContainerName) {
		podSpec.Containers = append(podSpec.Containers, builder.BuildSpiffeHelperContainer())
	}

	if decision.ClientRegistration.Inject && !containerExists(podSpec.Containers, ClientRegistrationContainerName) {
		podSpec.Containers = append(podSpec.Containers, builder.BuildClientRegistrationContainerWithSpireOption(crName, namespace, spireEnabled))
	}

	// Inject volumes — use SPIRE volumes when spireEnabled because both
	// spiffe-helper AND client-registration mount svid-output in that mode.
	var requiredVolumes []corev1.Volume
	if spireEnabled {
		requiredVolumes = BuildRequiredVolumes()
	} else {
		requiredVolumes = BuildRequiredVolumesNoSpire()
	}
	for _, vol := range requiredVolumes {
		if !volumeExists(podSpec.Volumes, vol.Name) {
			podSpec.Volumes = append(podSpec.Volumes, vol)
		}
	}

	mutatorLog.Info("Successfully mutated pod spec", "namespace", namespace, "crName", crName,
		"containers", len(podSpec.Containers),
		"initContainers", len(podSpec.InitContainers),
		"volumes", len(podSpec.Volumes),
		"spireEnabled", spireEnabled)
	return true, nil
}

// DEPRECATED, used by Agent CR. Remove ShouldMutate after Agent CR is deleted and use NeedsMutation instead.

// ShouldMutate determines if pod mutation should occur based on annotations and namespace labels
// Priority order:
// 1. CR annotation (opt-out): kagenti.dev/inject=false
// 2. CR annotation (opt-in): kagenti.dev/inject=true
// 3. Namespace label: kagenti-enabled=true
// 4. Namespace annotation: kagenti.dev/inject=true

func (m *PodMutator) ShouldMutate(ctx context.Context, namespace string, crAnnotations map[string]string) (bool, error) {
	mutatorLog.Info("Checking if mutation should occur", "namespace", namespace, "crAnnotations", crAnnotations)

	// Priority 1: CR-level opt-out (explicit disable)
	if crAnnotations[DefaultCRAnnotation] == "false" {
		mutatorLog.Info("CR annotation opt-out detected", "namespace", namespace, "annotation", DefaultCRAnnotation)
		return false, nil
	}

	// Priority 2: CR-level opt-in (explicit enable)
	if crAnnotations[DefaultCRAnnotation] == "true" {
		mutatorLog.Info("CR annotation opt-in detected", "namespace", namespace, "annotation", DefaultCRAnnotation)
		return true, nil
	}

	// Priority 3 & 4: Check namespace-level settings
	mutatorLog.Info("Checking namespace-level injection settings", "namespace", namespace, "label", m.NamespaceLabel, "annotation", m.NamespaceAnnotation)
	nsInjectionEnabled, err := CheckNamespaceInjectionEnabled(ctx, m.Client, namespace, m.NamespaceLabel, m.NamespaceAnnotation)
	if err != nil {
		mutatorLog.Error(err, "Failed to check namespace injection settings", "namespace", namespace)
		return false, fmt.Errorf("failed to check namespace injection settings: %w", err)
	}

	if nsInjectionEnabled {
		mutatorLog.Info("Namespace-level injection enabled", "namespace", namespace)
		return true, nil
	}
	return false, nil
}

// NeedsMutation is DEPRECATED (used by Agent CR only).
// It uses different opt-in semantics than InjectAuthBridge: when
// kagenti.io/inject is absent it falls back to namespace-level settings.
// InjectAuthBridge requires an explicit kagenti.io/inject=enabled label.
// Do NOT align these — the behavioural difference is intentional.
func (m *PodMutator) NeedsMutation(ctx context.Context, namespace string, labels map[string]string) (bool, error) {
	mutatorLog.Info("Checking if mutation should occur", "namespace", namespace, "labels", labels)

	// First, check if this is an agent workload (required for authbridge injection)
	kagentiType, hasKagentiLabel := labels[KagentiTypeLabel]
	if !hasKagentiLabel || (kagentiType != KagentiTypeAgent && kagentiType != KagentiTypeTool) {
		mutatorLog.Info("Skipping mutation: workload is not an agent or a tool",
			"hasLabel", hasKagentiLabel,
			"labelValue", kagentiType)
		return false, nil
	}

	value, exists := labels[AuthBridgeInjectLabel]

	// If label exists, respect its value (opt-in or opt-out)
	if exists {
		if value == AuthBridgeInjectValue {
			mutatorLog.Info("Workload label opt-in detected ")
			return true, nil
		}
		// Any other value (including "disabled", "false", etc.) is opt-out
		mutatorLog.Info("Workload label opt-out detected ")
		return false, nil
	}

	// No label - fall back to namespace-level settings
	mutatorLog.Info("Checking namespace-level injection settings", "namespace", namespace, "label", m.NamespaceLabel)
	return IsNamespaceInjectionEnabled(ctx, m.Client, namespace, m.NamespaceLabel)
}
func (m *PodMutator) InjectSidecars(podSpec *corev1.PodSpec, namespace, crName string) error {
	// Default to SPIRE enabled for backward compatibility
	return m.InjectSidecarsWithSpireOption(podSpec, namespace, crName, true)
}

// InjectSidecarsWithSpireOption injects sidecars with optional SPIRE support
func (m *PodMutator) InjectSidecarsWithSpireOption(podSpec *corev1.PodSpec, namespace, crName string, spireEnabled bool) error {
	if podSpec.Containers == nil {
		podSpec.Containers = []corev1.Container{}
	}

	// Only inject spiffe-helper if SPIRE is enabled
	if spireEnabled {
		if !containerExists(podSpec.Containers, SpiffeHelperContainerName) {
			mutatorLog.Info("Injecting spiffe-helper (SPIRE enabled)")
			podSpec.Containers = append(podSpec.Containers, m.Builder.BuildSpiffeHelperContainer())
		}
	} else {
		mutatorLog.Info("Skipping spiffe-helper injection (SPIRE disabled)")
	}

	// Check and inject client-registration sidecar (with SPIRE option)
	if m.EnableClientRegistration {
		if !containerExists(podSpec.Containers, ClientRegistrationContainerName) {
			podSpec.Containers = append(podSpec.Containers, m.Builder.BuildClientRegistrationContainerWithSpireOption(crName, namespace, spireEnabled))
		}
	} else {
		mutatorLog.Info("Skipping client-registration injection (disabled via --enable-client-registration=false)")
	}

	// Check and inject envoy-proxy sidecar
	if !containerExists(podSpec.Containers, EnvoyProxyContainerName) {
		podSpec.Containers = append(podSpec.Containers, m.Builder.BuildEnvoyProxyContainerWithSpireOption(spireEnabled))
	}

	return nil
}

func (m *PodMutator) InjectInitContainers(podSpec *corev1.PodSpec) error {
	mutatorLog.Info("Injecting init containers", "existingInitContainers", len(podSpec.InitContainers))

	if podSpec.InitContainers == nil {
		podSpec.InitContainers = []corev1.Container{}
	}

	// Check and inject proxy-init init container
	if !containerExists(podSpec.InitContainers, ProxyInitContainerName) {
		mutatorLog.Info("Injecting proxy-init init container")
		podSpec.InitContainers = append(podSpec.InitContainers, m.Builder.BuildProxyInitContainer("", ""))
	}

	return nil
}

func (m *PodMutator) InjectVolumes(podSpec *corev1.PodSpec) error {
	// Default to SPIRE enabled for backward compatibility
	return m.InjectVolumesWithSpireOption(podSpec, true)
}

// InjectVolumesWithSpireOption injects volumes with optional SPIRE support
func (m *PodMutator) InjectVolumesWithSpireOption(podSpec *corev1.PodSpec, spireEnabled bool) error {
	mutatorLog.Info("Injecting volumes", "existingVolumes", len(podSpec.Volumes), "spireEnabled", spireEnabled)

	if podSpec.Volumes == nil {
		podSpec.Volumes = []corev1.Volume{}
	}

	// Add all required volumes if they don't exist
	var requiredVolumes []corev1.Volume
	if spireEnabled {
		requiredVolumes = BuildRequiredVolumes()
	} else {
		requiredVolumes = BuildRequiredVolumesNoSpire()
	}

	injectedCount := 0
	for _, vol := range requiredVolumes {
		if !volumeExists(podSpec.Volumes, vol.Name) {
			mutatorLog.Info("Injecting volume", "volumeName", vol.Name)
			podSpec.Volumes = append(podSpec.Volumes, vol)
			injectedCount++
		}
	}

	mutatorLog.Info("Volume injection complete", "totalVolumes", len(podSpec.Volumes), "injected", injectedCount)
	return nil
}

const managedByLabel = "kagenti.io/managed-by"
const managedByValue = "webhook"

// ensureServiceAccount creates a ServiceAccount in the given namespace if it
// does not already exist. This gives SPIRE-enabled workloads a dedicated
// identity so the SPIFFE ID is spiffe://<trust-domain>/ns/<ns>/sa/<name>
// rather than .../sa/default.
func (m *PodMutator) ensureServiceAccount(ctx context.Context, namespace, name string) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				managedByLabel: managedByValue,
			},
		},
	}
	if err := m.Client.Create(ctx, sa); err != nil {
		if apierrors.IsAlreadyExists(err) {
			existing := &corev1.ServiceAccount{}
			if getErr := m.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, existing); getErr != nil {
				mutatorLog.Error(getErr, "Failed to fetch existing ServiceAccount", "namespace", namespace, "name", name)
				return nil
			}
			if existing.Labels[managedByLabel] != managedByValue {
				mutatorLog.Info("WARNING: ServiceAccount exists but is not managed by this webhook",
					"namespace", namespace, "name", name,
					"existingLabels", existing.Labels)
			} else {
				mutatorLog.Info("ServiceAccount already exists", "namespace", namespace, "name", name)
			}
			return nil
		}
		return err
	}
	mutatorLog.Info("Created ServiceAccount", "namespace", namespace, "name", name)
	return nil
}

func containerExists(containers []corev1.Container, name string) bool {
	for _, container := range containers {
		if container.Name == name {
			return true
		}
	}
	return false
}

func volumeExists(volumes []corev1.Volume, name string) bool {
	for _, vol := range volumes {
		if vol.Name == name {
			return true
		}
	}
	return false
}
