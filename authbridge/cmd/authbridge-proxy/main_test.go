package main

import (
	"encoding/json"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/config"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins/tokenexchange"
)

func identityConfig(idType string) json.RawMessage {
	return json.RawMessage(`{"identity":{"type":"` + idType + `"}}`)
}

// TestSpiffeIdentityTypeMatchesTokenExchange guards against drift between
// main.go's local spiffeIdentityType constant and the token-exchange plugin's
// canonical SpiffeIdentity value. main.go intentionally does NOT import the
// plugin package (so token-exchange stays build-tag excludable via
// plugins_tokenexchange.go); this test re-couples the two at test time — a
// test file may import the package unconditionally without pulling it into the
// tag-gated production binary.
func TestSpiffeIdentityTypeMatchesTokenExchange(t *testing.T) {
	if spiffeIdentityType != tokenexchange.SpiffeIdentity {
		t.Errorf("spiffeIdentityType = %q but tokenexchange.SpiffeIdentity = %q; "+
			"the identity.type=spiffe convention has drifted — update spiffeIdentityType in main.go",
			spiffeIdentityType, tokenexchange.SpiffeIdentity)
	}
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
		{"malformed plugin config", outbound(config.PluginEntry{
			Name: "bad-plugin", Config: json.RawMessage(`{not valid json`),
		}), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := spiffeProviderNeeded(tt.cfg); got != tt.want {
				t.Errorf("spiffeProviderNeeded() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestProviderConsumersCoveredByPredicate enforces the invariant that ties
// spiffeProviderNeeded to plugins.BuildWithSPIFFE: the predicate detects a
// plugin's need via identity.type=spiffe, which covers token-exchange — the
// only spiffe.ProviderConsumer today. If a new ProviderConsumer is registered,
// this fails so the author confirms spiffeProviderNeeded detects its need;
// otherwise that plugin would silently receive a nil Provider on a SPIRE-less
// cluster. The main package blank-imports every plugin, so the registry here is
// the full production set.
func TestProviderConsumersCoveredByPredicate(t *testing.T) {
	// Plugins whose SPIFFE need spiffeProviderNeeded is known to detect.
	covered := map[string]bool{"token-exchange": true}
	for _, name := range plugins.SPIFFEConsumerPlugins() {
		// Tripwire: a new consumer must be consciously reviewed and listed.
		if !covered[name] {
			t.Errorf("plugin %q implements spiffe.ProviderConsumer but is not covered by "+
				"spiffeProviderNeeded; make it signal its need via identity.type=spiffe (or "+
				"extend the predicate), then add %q to this set", name, name)
		}
		// Functional: the predicate must actually fire for that consumer's
		// spiffe config, so it never receives a nil Provider.
		cfg := &config.Config{Pipeline: config.PipelineConfig{
			Outbound: config.PipelineStageConfig{Plugins: []config.PluginEntry{
				{Name: name, Config: identityConfig("spiffe")},
			}},
		}}
		if !spiffeProviderNeeded(cfg) {
			t.Errorf("spiffeProviderNeeded must return true for consumer %q with identity.type=spiffe", name)
		}
	}
}
