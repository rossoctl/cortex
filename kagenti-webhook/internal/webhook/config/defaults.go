package config

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// CompiledDefaults returns hardcoded defaults used when no config is provided
func CompiledDefaults() *PlatformConfig {
	return &PlatformConfig{
		Images: ImageConfig{
			EnvoyProxy:         "ghcr.io/kagenti/kagenti-extensions/envoy-with-processor:local",
			ProxyInit:          "ghcr.io/kagenti/kagenti-extensions/proxy-init:local",
			SpiffeHelper:       "ghcr.io/spiffe/spiffe-helper:nightly",
			ClientRegistration: "ghcr.io/kagenti/kagenti-extensions/client-registration:local",
			PullPolicy:         corev1.PullNever,
		},
		Proxy: ProxyConfig{
			Port:             15123,
			UID:              1337,
			InboundProxyPort: 15124,
			AdminPort:        9901,
		},
		Resources: ResourcesConfig{
			EnvoyProxy: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("50m"),
					corev1.ResourceMemory: resource.MustParse("64Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("200m"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
			},
			ProxyInit: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("10m"),
					corev1.ResourceMemory: resource.MustParse("10Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("10m"),
					corev1.ResourceMemory: resource.MustParse("10Mi"),
				},
			},
			SpiffeHelper: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("50m"),
					corev1.ResourceMemory: resource.MustParse("64Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
			},
			ClientRegistration: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("50m"),
					corev1.ResourceMemory: resource.MustParse("64Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
			},
		},
		TokenExchange: TokenExchangeDefaults{
			DefaultScopes: []string{"openid"},
		},
		Spiffe: SpiffeConfig{
			TrustDomain: "cluster.local",
			SocketPath:  "unix:///spiffe-workload-api/spire-agent.sock",
		},
		Observability: ObservabilityConfig{
			LogLevel:      "info",
			EnableMetrics: true,
			EnableTracing: false,
		},
		Sidecars: SidecarDefaults{
			EnvoyProxy:         SidecarDefault{Enabled: true},
			SpiffeHelper:       SidecarDefault{Enabled: true},
			ClientRegistration: SidecarDefault{Enabled: true},
		},
	}
}
