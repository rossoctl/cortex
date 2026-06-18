package main

import (
	"encoding/json"
	"testing"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/config"
)

func identityConfig(idType string) json.RawMessage {
	return json.RawMessage(`{"identity":{"type":"` + idType + `"}}`)
}

// TestSpiffeProviderNeeded pins the need-driven gate: the SPIFFE provider is
// built only when mTLS is on or a plugin selects the spiffe identity scheme.
// A bare `spiffe: {}` block with neither must NOT trigger it — that is what
// lets the TLS bridge (cert-manager CA, no SVID) boot without SPIRE.
func TestSpiffeProviderNeeded(t *testing.T) {
	outbound := func(entries ...config.PluginEntry) *config.Config {
		return &config.Config{Pipeline: config.PipelineConfig{
			Outbound: config.PipelineStageConfig{Plugins: entries},
		}}
	}
	inbound := func(entries ...config.PluginEntry) *config.Config {
		return &config.Config{Pipeline: config.PipelineConfig{
			Inbound: config.PipelineStageConfig{Plugins: entries},
		}}
	}

	tests := []struct {
		name string
		cfg  *config.Config
		want bool
	}{
		{"empty config", &config.Config{}, false},
		{"mtls present", &config.Config{MTLS: &config.MTLSConfig{}}, true},
		{"token-exchange client-secret", outbound(config.PluginEntry{
			Name: "token-exchange", Config: identityConfig("client-secret"),
		}), false},
		{"token-exchange spiffe (outbound)", outbound(config.PluginEntry{
			Name: "token-exchange", Config: identityConfig("spiffe"),
		}), true},
		{"spiffe identity (inbound)", inbound(config.PluginEntry{
			Name: "some-plugin", Config: identityConfig("spiffe"),
		}), true},
		{"plugin with no config", outbound(config.PluginEntry{Name: "jwt-validation"}), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := spiffeProviderNeeded(tt.cfg); got != tt.want {
				t.Errorf("spiffeProviderNeeded() = %v, want %v", got, tt.want)
			}
		})
	}
}
