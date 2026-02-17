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
	"fmt"

	"github.com/kagenti/kagenti-extensions/kagenti-webhook/internal/webhook/config"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var builderLog = logf.Log.WithName("container-builder")

const (
	// Container names for AuthBridge sidecars
	EnvoyProxyContainerName = "envoy-proxy"
	ProxyInitContainerName  = "proxy-init"

	// Client registration container configuration
	// Keep in sync with AuthBridge/client-registration/Dockerfile
	ClientRegistrationUID = 1000
	ClientRegistrationGID = 1000
)

type ContainerBuilder struct {
	cfg *config.PlatformConfig
}

func NewContainerBuilder(cfg *config.PlatformConfig) *ContainerBuilder {
	if cfg == nil {
		cfg = config.CompiledDefaults()
	}
	return &ContainerBuilder{cfg: cfg}
}

func (b *ContainerBuilder) BuildSpiffeHelperContainer() corev1.Container {
	builderLog.Info("building SpiffeHelper Container")

	return corev1.Container{
		Name:            SpiffeHelperContainerName,
		Image:           b.cfg.Images.SpiffeHelper,
		ImagePullPolicy: b.cfg.Images.PullPolicy,
		Resources:       b.cfg.Resources.SpiffeHelper,
		Command: []string{
			"/spiffe-helper",
			"-config=/etc/spiffe-helper/helper.conf",
			"run",
		},
		// Run as the same UID/GID as client-registration so that SVID files
		// written to the shared svid-output volume (/opt) are readable by
		// the client-registration container. spiffe-helper writes files with
		// restrictive permissions (0600), so matching the UID is required.
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:  ptr.To(int64(ClientRegistrationUID)),
			RunAsGroup: ptr.To(int64(ClientRegistrationGID)),
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "spiffe-helper-config",
				MountPath: "/etc/spiffe-helper",
			},
			{
				Name:      "spire-agent-socket",
				MountPath: "/spiffe-workload-api",
			},
			{
				Name:      "svid-output",
				MountPath: "/opt",
			},
			{
				Name:      "shared-data",
				MountPath: "/shared",
			},
		},
	}
}

func (b *ContainerBuilder) BuildClientRegistrationContainer(name, namespace string) corev1.Container {
	// Default to SPIRE enabled for backward compatibility
	return b.BuildClientRegistrationContainerWithSpireOption(name, namespace, true)
}

// BuildClientRegistrationContainerWithSpireOption creates the client registration container
// with optional SPIRE support
func (b *ContainerBuilder) BuildClientRegistrationContainerWithSpireOption(name, namespace string, spireEnabled bool) corev1.Container {
	builderLog.Info("building ClientRegistration Container", "spireEnabled", spireEnabled)

	clientName := namespace + "/" + name

	// Base environment variables
	env := []corev1.EnvVar{
		{
			Name:  "SPIRE_ENABLED",
			Value: fmt.Sprintf("%t", spireEnabled),
		},
		{
			Name: "KEYCLOAK_URL",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: "environments",
					},
					Key:      "KEYCLOAK_URL",
					Optional: ptr.To(true),
				},
			},
		},
		{
			Name: "KEYCLOAK_REALM",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: "environments",
					},
					Key: "KEYCLOAK_REALM",
				},
			},
		},
		{
			Name: "KEYCLOAK_ADMIN_USERNAME",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: "environments",
					},
					Key: "KEYCLOAK_ADMIN_USERNAME",
				},
			},
		},
		{
			Name: "KEYCLOAK_ADMIN_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: "environments",
					},
					Key: "KEYCLOAK_ADMIN_PASSWORD",
				},
			},
		},
		{
			Name:  "CLIENT_NAME",
			Value: clientName,
		},
		{
			Name:  "SECRET_FILE_PATH",
			Value: "/shared/client-secret.txt",
		},
	}

	// Volume mounts depend on SPIRE enablement
	var volumeMounts []corev1.VolumeMount
	if spireEnabled {
		volumeMounts = []corev1.VolumeMount{
			{
				Name:      "svid-output",
				MountPath: "/opt",
			},
			{
				Name:      "shared-data",
				MountPath: "/shared",
			},
		}
	} else {
		volumeMounts = []corev1.VolumeMount{
			{
				Name:      "shared-data",
				MountPath: "/shared",
			},
		}
	}

	// Build the command based on SPIRE enablement
	// When SPIRE is enabled, extract client ID from JWT
	// When SPIRE is disabled, use CLIENT_NAME as the client ID
	var command string
	if spireEnabled {
		command = `
echo "Waiting for SPIFFE credentials..."
while [ ! -f /opt/jwt_svid.token ]; do
  echo "waiting for SVID"
  sleep 1
done
echo "SPIFFE credentials ready!"

# Extract client ID (SPIFFE ID) from JWT and save to file
JWT_PAYLOAD=$(cat /opt/jwt_svid.token | cut -d'.' -f2)
if ! CLIENT_ID=$(echo "${JWT_PAYLOAD}==" | base64 -d | python -c "import sys,json; print(json.load(sys.stdin).get('sub',''))"); then
  echo "Error: Failed to decode JWT payload or extract client ID" >&2
  exit 1
fi
if [ -z "$CLIENT_ID" ]; then
  echo "Error: Extracted client ID is empty" >&2
  exit 1
fi
echo "$CLIENT_ID" > /shared/client-id.txt
echo "Client ID (SPIFFE ID): $CLIENT_ID"

echo "Starting client registration..."
python client_registration.py
echo "Client registration complete!"
tail -f /dev/null
`
	} else {
		command = `
echo "SPIRE disabled - using static client ID"

# Use CLIENT_NAME as the client ID
echo "$CLIENT_NAME" > /shared/client-id.txt
echo "Client ID: $CLIENT_NAME"

echo "Starting client registration..."
python client_registration.py
echo "Client registration complete!"
tail -f /dev/null
`
	}

	return corev1.Container{
		Name:            ClientRegistrationContainerName,
		Image:           b.cfg.Images.ClientRegistration,
		ImagePullPolicy: b.cfg.Images.PullPolicy,
		Resources:       b.cfg.Resources.ClientRegistration,
		Command: []string{
			"/bin/sh",
			"-c",
			command,
		},
		Env:          env,
		VolumeMounts: volumeMounts,
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:    ptr.To(int64(ClientRegistrationUID)),
			RunAsGroup:   ptr.To(int64(ClientRegistrationGID)),
			RunAsNonRoot: ptr.To(true),
		},
	}
}

// BuildEnvoyProxyContainer creates the envoy-proxy sidecar container
// This container intercepts inbound traffic (JWT validation) and outbound traffic (token exchange) via ext-proc
func (b *ContainerBuilder) BuildEnvoyProxyContainer() corev1.Container {
	builderLog.Info("building EnvoyProxy Container")

	return corev1.Container{
		Name:            EnvoyProxyContainerName,
		Image:           b.cfg.Images.EnvoyProxy,
		ImagePullPolicy: b.cfg.Images.PullPolicy,
		Resources:       b.cfg.Resources.EnvoyProxy,
		Ports: []corev1.ContainerPort{
			{
				Name:          "envoy-outbound",
				ContainerPort: b.cfg.Proxy.Port,
				Protocol:      corev1.ProtocolTCP,
			},
			{
				Name:          "envoy-inbound",
				ContainerPort: b.cfg.Proxy.InboundProxyPort,
				Protocol:      corev1.ProtocolTCP,
			},
			{
				Name:          "envoy-admin",
				ContainerPort: b.cfg.Proxy.AdminPort,
				Protocol:      corev1.ProtocolTCP,
			},
			{
				Name:          "ext-proc",
				ContainerPort: 9090,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		Env: []corev1.EnvVar{
			{
				Name: "TOKEN_URL",
				ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: "authbridge-config",
						},
						Key:      "TOKEN_URL",
						Optional: ptr.To(true),
					},
				},
			},
			{
				Name: "ISSUER",
				ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: "authbridge-config",
						},
						Key:      "ISSUER",
						Optional: ptr.To(false),
					},
				},
			},
			{
				Name: "EXPECTED_AUDIENCE",
				ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: "authbridge-config",
						},
						Key:      "EXPECTED_AUDIENCE",
						Optional: ptr.To(true),
					},
				},
			},
			{
				Name: "TARGET_AUDIENCE",
				ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: "authbridge-config",
						},
						Key:      "TARGET_AUDIENCE",
						Optional: ptr.To(true),
					},
				},
			},
			{
				Name: "TARGET_SCOPES",
				ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: "authbridge-config",
						},
						Key:      "TARGET_SCOPES",
						Optional: ptr.To(true),
					},
				},
			},
			{
				Name:  "CLIENT_ID_FILE",
				Value: "/shared/client-id.txt",
			},
			{
				Name:  "CLIENT_SECRET_FILE",
				Value: "/shared/client-secret.txt",
			},
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:  ptr.To(b.cfg.Proxy.UID),
			RunAsGroup: ptr.To(b.cfg.Proxy.UID),
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "envoy-config",
				MountPath: "/etc/envoy",
				ReadOnly:  true,
			},
			{
				Name:      "shared-data",
				MountPath: "/shared",
				ReadOnly:  true,
			},
		},
	}
}

// BuildProxyInitContainer creates the init container that sets up iptables
// to redirect outbound traffic to the Envoy proxy.
//
// SECURITY NOTE: This init container requires elevated privileges:
//   - RunAsUser: 0 (root) - Required to modify network namespace iptables rules
//   - RunAsNonRoot: false - Explicitly allows root execution
//   - Privileged: true - Required for iptables manipulation and sysctl commands
//     (e.g., sysctl -w net.ipv4.conf.all.route_localnet=1 for Istio Ambient Mesh coexistence)
//
// These privileges are necessary because iptables manipulation is a kernel-level
// operation that requires root access. This is a common pattern used by service
// meshes (Istio, Linkerd) for transparent traffic interception.
//
// Risk mitigations:
//   - This runs as an init container (not a long-running sidecar), limiting exposure window
//   - The container exits immediately after configuring iptables rules
//   - Minimal resource limits are applied (10m CPU, 10Mi memory)
//   - The container image should be regularly updated and scanned for vulnerabilities
//   - Consider using a distroless or minimal base image for the proxy-init container
//
// Alternative approaches (not currently implemented):
//   - CNI plugin: Configure iptables at pod network setup time (requires cluster-level changes)
//   - Istio CNI: Similar approach used by Istio to avoid privileged init containers
func (b *ContainerBuilder) BuildProxyInitContainer() corev1.Container {
	builderLog.Info("building ProxyInit Container")

	return corev1.Container{
		Name:            ProxyInitContainerName,
		Image:           b.cfg.Images.ProxyInit,
		ImagePullPolicy: b.cfg.Images.PullPolicy,
		Resources:       b.cfg.Resources.ProxyInit,
		Env: []corev1.EnvVar{
			{
				Name:  "PROXY_PORT",
				Value: fmt.Sprintf("%d", b.cfg.Proxy.Port),
			},
			{
				Name:  "INBOUND_PROXY_PORT",
				Value: fmt.Sprintf("%d", b.cfg.Proxy.InboundProxyPort),
			},
			{
				Name:  "PROXY_UID",
				Value: fmt.Sprintf("%d", b.cfg.Proxy.UID),
			},
			{
				Name:  "OUTBOUND_PORTS_EXCLUDE",
				Value: "8080", // Exclude Keycloak port from redirect
			},
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:    ptr.To(int64(0)),
			RunAsNonRoot: ptr.To(false),
			Privileged:   ptr.To(true),
		},
	}
}

// Backward-compatible package-level wrappers using compiled defaults.
// These are called by PodMutator and will be removed in Phase 4
// when PodMutator is rewired to use ContainerBuilder directly.

func BuildSpiffeHelperContainer() corev1.Container {
	return NewContainerBuilder(nil).BuildSpiffeHelperContainer()
}

func BuildClientRegistrationContainerWithSpireOption(name, namespace string, spireEnabled bool) corev1.Container {
	return NewContainerBuilder(nil).BuildClientRegistrationContainerWithSpireOption(name, namespace, spireEnabled)
}

func BuildEnvoyProxyContainer() corev1.Container {
	return NewContainerBuilder(nil).BuildEnvoyProxyContainer()
}

func BuildProxyInitContainer() corev1.Container {
	return NewContainerBuilder(nil).BuildProxyInitContainer()
}
