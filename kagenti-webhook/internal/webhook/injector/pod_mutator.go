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
	"strings"

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

	// Authentication mode labels
	AuthModeLabel     = "kagenti.io/auth-mode"
	AuthModeWaypoint  = "waypoint"
	AuthModeSidecar   = "sidecar"

	// Label selector for authbridge injection opt-out.
	// Injection uses opt-out semantics for agents: sidecars are injected by
	// default. Setting AuthBridgeInjectLabel=AuthBridgeDisabledValue on a
	// workload explicitly opts it out. Any other value (including absent) does
	// not block injection.
	AuthBridgeInjectLabel   = "kagenti.io/inject"
	AuthBridgeInjectValue   = "enabled" // retained for backwards compat / tests
	AuthBridgeDisabledValue = "disabled"

	// SPIRE opt-out label. Setting kagenti.io/spire=disabled on a workload blocks
	// spiffe-helper injection (layer 7 of the precedence chain). Any other value
	// (including absence of the label) leaves spiffe-helper injection to the
	// upstream precedence layers.
	SpireEnableLabel   = "kagenti.io/spire"
	SpireDisabledValue = "disabled"
	// SpireEnabledValue is a non-operative label value under the opt-out model.
	// Retained as a named constant so tests can assert that a non-disabled value
	// does not block injection.
	SpireEnabledValue = "enabled"
	// Istio exclusion annotations
	IstioSidecarInjectAnnotation = "sidecar.istio.io/inject"
	AmbientRedirectionAnnotation = "ambient.istio.io/redirection"

	// Port exclusion annotations — per-workload iptables overrides.
	// Values are comma-separated port numbers. Outbound values are appended
	// to the mandatory exclusion (8080). Example: "11434,4317"
	OutboundPortsExcludeAnnotation = "kagenti.io/outbound-ports-exclude"
	InboundPortsExcludeAnnotation  = "kagenti.io/inbound-ports-exclude"

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
	// Getter functions for hot-reloadable config (used by precedence evaluator)
	GetPlatformConfig func() *config.PlatformConfig
	GetFeatureGates   func() *config.FeatureGates
	// DefaultAuthMode is the authentication mode to use when no explicit mode is set
	DefaultAuthMode string
}

func NewPodMutator(
	client client.Client,
	enableClientRegistration bool,
	getPlatformConfig func() *config.PlatformConfig,
	getFeatureGates func() *config.FeatureGates,
	defaultAuthMode string,
) *PodMutator {
	// Validate and normalize defaultAuthMode
	if defaultAuthMode != AuthModeWaypoint && defaultAuthMode != AuthModeSidecar {
		mutatorLog.Info("Invalid default auth mode, using waypoint", "provided", defaultAuthMode, "using", AuthModeWaypoint)
		defaultAuthMode = AuthModeWaypoint
	}

	return &PodMutator{
		Client:                   client,
		EnableClientRegistration: enableClientRegistration,
		GetPlatformConfig:        getPlatformConfig,
		GetFeatureGates:          getFeatureGates,
		DefaultAuthMode:          defaultAuthMode,
	}
}

// getAuthMode determines the authentication mode for a pod based on labels.
// Priority:
//  1. Explicit kagenti.io/auth-mode label (waypoint or sidecar)
//  2. Legacy labels (kagenti.io/inject=enabled or kagenti.io/envoy-proxy-inject=true → sidecar)
//  3. Default configured mode (waypoint)
func (m *PodMutator) getAuthMode(labels map[string]string) string {
	// Check explicit auth mode label
	if mode, ok := labels[AuthModeLabel]; ok {
		if mode == AuthModeWaypoint || mode == AuthModeSidecar {
			return mode
		}
		mutatorLog.Info("Invalid auth-mode label value, falling back to default",
			"value", mode, "default", m.DefaultAuthMode)
	}

	// Check legacy inject labels (backward compatibility)
	if labels[AuthBridgeInjectLabel] == AuthBridgeInjectValue {
		return AuthModeSidecar
	}

	// Check legacy per-sidecar labels
	if labels["kagenti.io/envoy-proxy-inject"] == "true" {
		return AuthModeSidecar
	}

	// Default to configured default mode
	return m.DefaultAuthMode
}

// InjectAuthBridge determines the authentication mode and delegates to the appropriate injection method.
func (m *PodMutator) InjectAuthBridge(ctx context.Context, podSpec *corev1.PodSpec, namespace, crName string, labels, annotations map[string]string) (bool, error) {
	mutatorLog.Info("InjectAuthBridge called", "namespace", namespace, "crName", crName, "labels", labels)

	// Determine authentication mode
	authMode := m.getAuthMode(labels)
	mutatorLog.Info("Authentication mode determined", "namespace", namespace, "crName", crName, "authMode", authMode)

	// Branch based on authentication mode
	switch authMode {
	case AuthModeWaypoint:
		return m.injectAuthBridgeWaypointMode(ctx, podSpec, namespace, crName, labels, annotations)
	case AuthModeSidecar:
		return m.injectAuthBridgeSidecarMode(ctx, podSpec, namespace, crName, labels, annotations)
	default:
		mutatorLog.Info("Unknown auth mode, defaulting to waypoint", "namespace", namespace, "crName", crName, "authMode", authMode)
		return m.injectAuthBridgeWaypointMode(ctx, podSpec, namespace, crName, labels, annotations)
	}
}

// injectAuthBridgeWaypointMode handles waypoint-based authentication.
// In waypoint mode, no sidecars are injected. Authentication is handled by the namespace waypoint gateway.
func (m *PodMutator) injectAuthBridgeWaypointMode(ctx context.Context, podSpec *corev1.PodSpec, namespace, crName string, labels, annotations map[string]string) (bool, error) {
	mutatorLog.Info("Waypoint mode: no sidecar injection needed", "namespace", namespace, "crName", crName)

	// Pre-filter: kagenti.io/type must be agent or tool
	kagentiType, hasKagentiLabel := labels[KagentiTypeLabel]
	if !hasKagentiLabel || (kagentiType != KagentiTypeAgent && kagentiType != KagentiTypeTool) {
		mutatorLog.Info("Skipping mutation: workload is not an agent or a tool",
			"hasLabel", hasKagentiLabel,
			"labelValue", kagentiType)
		return false, nil
	}

	// Get feature gates for global kill switch check
	currentGates := m.GetFeatureGates()
	if !currentGates.GlobalEnabled {
		mutatorLog.Info("Skipping mutation: global feature gate disabled",
			"namespace", namespace, "crName", crName)
		return false, nil
	}

	// Tool workloads are only processed when the injectTools feature gate is on
	if kagentiType == KagentiTypeTool && !currentGates.InjectTools {
		mutatorLog.Info("Skipping mutation: tool injection disabled via injectTools feature gate",
			"namespace", namespace, "crName", crName)
		return false, nil
	}

	// Opt-out: skip even annotation when kagenti.io/inject=disabled is explicitly set
	if labels[AuthBridgeInjectLabel] == AuthBridgeDisabledValue {
		mutatorLog.Info("Skipping mutation: workload opted out via kagenti.io/inject=disabled",
			"namespace", namespace, "crName", crName)
		return false, nil
	}

	// In waypoint mode, we don't inject any sidecars
	// The namespace waypoint gateway (managed by the operator) handles authentication
	// We just add an annotation to indicate the pod is using waypoint mode
	mutatorLog.Info("Waypoint mode enabled, no sidecars injected",
		"namespace", namespace,
		"crName", crName,
		"kagentiType", kagentiType,
		"message", "Authentication will be handled by namespace waypoint gateway")

	return false, nil
}

// injectAuthBridgeSidecarMode evaluates the multi-layer precedence chain and conditionally injects sidecars.
// This is the legacy sidecar-based authentication mode.
func (m *PodMutator) injectAuthBridgeSidecarMode(ctx context.Context, podSpec *corev1.PodSpec, namespace, crName string, labels, annotations map[string]string) (bool, error) {
	mutatorLog.Info("Sidecar mode: evaluating injection precedence", "namespace", namespace, "crName", crName)

	// Pre-filter: kagenti.io/type must be agent or tool.
	kagentiType, hasKagentiLabel := labels[KagentiTypeLabel]
	if !hasKagentiLabel || (kagentiType != KagentiTypeAgent && kagentiType != KagentiTypeTool) {
		mutatorLog.Info("Skipping mutation: workload is not an agent or a tool",
			"hasLabel", hasKagentiLabel,
			"labelValue", kagentiType)
		return false, nil
	}

	// Get fresh config snapshots for this request (hot-reloadable)
	currentConfig := m.GetPlatformConfig()
	currentGates := m.GetFeatureGates()

	// Global kill switch — disables all injection cluster-wide.
	if !currentGates.GlobalEnabled {
		mutatorLog.Info("Skipping mutation: global feature gate disabled",
			"namespace", namespace, "crName", crName)
		return false, nil
	}

	// Tool workloads are only injected when the injectTools feature gate is on.
	if kagentiType == KagentiTypeTool && !currentGates.InjectTools {
		mutatorLog.Info("Skipping mutation: tool injection disabled via injectTools feature gate",
			"namespace", namespace, "crName", crName)
		return false, nil
	}

	// Opt-out: skip injection when kagenti.io/inject=disabled is explicitly set.
	if labels[AuthBridgeInjectLabel] == AuthBridgeDisabledValue {
		mutatorLog.Info("Skipping mutation: workload opted out via kagenti.io/inject=disabled",
			"namespace", namespace, "crName", crName)
		return false, nil
	}

	// Evaluate the per-sidecar precedence chain
	evaluator := NewPrecedenceEvaluator(currentGates, currentConfig)
	decision := evaluator.Evaluate(labels)

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

	// ========================================
	// Build containers + volumes
	// ========================================
	//
	// Two modes controlled by the perWorkloadConfigResolution feature gate:
	//   false (default) → legacy path: ValueFrom refs for env vars, kubelet
	//                     resolves ConfigMap/Secret values at container start.
	//   true            → resolved path: webhook reads namespace ConfigMaps/
	//                     Secrets at admission time and injects literal values.

	var builder *ContainerBuilder
	var requiredVolumes []corev1.Volume

	if currentGates.PerWorkloadConfigResolution {
		// Resolved path: read namespace config and build literal env vars
		var nsConfig *NamespaceConfig
		var err error
		mutatorLog.V(1).Info("reading namespace config per-workload", "namespace", namespace)
		nsConfig, err = ReadNamespaceConfig(ctx, m.Client, namespace)
		if err != nil {
			mutatorLog.Error(err, "failed to read namespace config, using empty defaults",
				"namespace", namespace)
			nsConfig = &NamespaceConfig{}
		}

		// Read AgentRuntime overrides (nil if CR not found or CRD not installed)
		var arOverrides *AgentRuntimeOverrides
		arOverrides, err = ReadAgentRuntimeOverrides(ctx, m.Client, namespace, crName)
		if err != nil {
			mutatorLog.Error(err, "failed to read AgentRuntime overrides, continuing without",
				"namespace", namespace, "crName", crName)
		}

		resolved := ResolveConfig(currentConfig, nsConfig, arOverrides)
		builder = NewResolvedContainerBuilder(resolved)
		requiredVolumes = BuildResolvedVolumes(spireEnabled, "")

		mutatorLog.Info("Using resolved config path",
			"namespace", namespace, "crName", crName,
			"hasAgentRuntimeOverrides", arOverrides != nil)
	} else {
		// Legacy path: ValueFrom refs, kubelet resolves at runtime
		builder = NewContainerBuilder(currentConfig)
		if spireEnabled {
			requiredVolumes = BuildRequiredVolumes()
		} else {
			requiredVolumes = BuildRequiredVolumesNoSpire()
		}
		mutatorLog.Info("Using legacy ValueFrom config path",
			"namespace", namespace, "crName", crName)
	}

	// Conditionally inject sidecars based on precedence decisions.
	// Two modes controlled by the combinedSidecar feature gate:
	//   true  → combined mode: single "authbridge" container replaces envoy-proxy +
	//           spiffe-helper + client-registration. proxy-init is still separate.
	//   false → legacy mode: separate sidecar containers (unchanged behavior).
	if currentGates.CombinedSidecar {
		// Combined mode: inject single authbridge container (only when envoy-proxy is enabled)
		if decision.EnvoyProxy.Inject && !containerExists(podSpec.Containers, AuthBridgeContainerName) {
			podSpec.Containers = append(podSpec.Containers,
				builder.BuildAuthBridgeContainer(crName, namespace,
					decision.SpiffeHelper.Inject,
					decision.ClientRegistration.Inject))
		}
		// proxy-init is still injected separately
		if decision.ProxyInit.Inject && !containerExists(podSpec.InitContainers, ProxyInitContainerName) {
			outboundExclude := annotations[OutboundPortsExcludeAnnotation]
			inboundExclude := annotations[InboundPortsExcludeAnnotation]
			podSpec.InitContainers = append(podSpec.InitContainers, builder.BuildProxyInitContainer(outboundExclude, inboundExclude))
		}
	} else {
		// Legacy mode: separate sidecar containers
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
	}

	// Inject volumes
	for i := range requiredVolumes {
		if !volumeExists(podSpec.Volumes, requiredVolumes[i].Name) {
			podSpec.Volumes = append(podSpec.Volumes, requiredVolumes[i])
		}
	}

	// Operator-managed client registration: mount Keycloak credentials from a Secret named in
	// annotations (written by kagenti-operator) for all containers using shared-data.
	ApplyKeycloakClientCredentialsSecretVolumes(podSpec, annotations)

	logClientRegistrationPaths(namespace, crName, labels, currentGates.CombinedSidecar, decision, annotations)

	// Set fsGroup for shared volume access when SPIRE is enabled
	if spireEnabled {
		ensureFSGroup(podSpec)
	}

	mutatorLog.Info("Successfully mutated pod spec", "namespace", namespace, "crName", crName,
		"containers", len(podSpec.Containers),
		"initContainers", len(podSpec.InitContainers),
		"volumes", len(podSpec.Volumes),
		"spireEnabled", spireEnabled)
	return true, nil
}

// logClientRegistrationPaths records how Keycloak client credentials are delivered for this Pod:
// Secret mounted from annotation (kagenti-operator), legacy kagenti-client-registration sidecar, or combined authbridge.
// deliveryPaths is comma-separated short tokens: operator-secret, combined-authbridge, sidecar, skip.
func logClientRegistrationPaths(namespace, crName string, labels map[string]string, combinedSidecar bool, decision InjectionDecision, annotations map[string]string) {
	keycloakClientCredentialsSecret := strings.TrimSpace(annotations[AnnotationKeycloakClientSecretName])

	var paths []string
	if keycloakClientCredentialsSecret != "" {
		paths = append(paths, "operator-secret")
	}

	if combinedSidecar {
		if decision.EnvoyProxy.Inject && decision.ClientRegistration.Inject {
			paths = append(paths, "combined-authbridge")
		}
	} else if decision.ClientRegistration.Inject {
		paths = append(paths, "sidecar")
	}

	if len(paths) == 0 {
		paths = append(paths, "skip")
	}

	mutatorLog.Info("AuthBridge client registration: how credentials are supplied for this Pod",
		"namespace", namespace,
		"workloadKey", crName,
		"kagentiType", labels[KagentiTypeLabel],
		"deliveryPaths", strings.Join(paths, ","),
		"keycloakClientCredentialsSecretName", keycloakClientCredentialsSecret,
		"combinedSidecarMode", combinedSidecar,
		"injectClientRegistrationSidecar", decision.ClientRegistration.Inject,
		"injectEnvoyOrAuthbridge", decision.EnvoyProxy.Inject,
	)
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
				return fmt.Errorf("failed to fetch existing ServiceAccount %s/%s: %w", namespace, name, getErr)
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
		return fmt.Errorf("failed to create ServiceAccount %s/%s: %w", namespace, name, err)
	}
	mutatorLog.Info("Created ServiceAccount", "namespace", namespace, "name", name)
	return nil
}

func containerExists(containers []corev1.Container, name string) bool {
	for i := range containers {
		if containers[i].Name == name {
			return true
		}
	}
	return false
}

func volumeExists(volumes []corev1.Volume, name string) bool {
	for i := range volumes {
		if volumes[i].Name == name {
			return true
		}
	}
	return false
}

// ensureFSGroup sets fsGroup in the pod security context to enable shared volume access.
// This allows containers with different UIDs (spiffe-helper, client-registration, envoy-proxy)
// to read/write files in shared volumes like svid-output.
func ensureFSGroup(podSpec *corev1.PodSpec) {
	fsGroupValue := int64(SharedVolumesFSGroup)

	if podSpec.SecurityContext == nil {
		podSpec.SecurityContext = &corev1.PodSecurityContext{}
	}

	if podSpec.SecurityContext.FSGroup == nil {
		podSpec.SecurityContext.FSGroup = &fsGroupValue
		mutatorLog.Info("Set fsGroup for shared volume access", "fsGroup", fsGroupValue)
	}
}
