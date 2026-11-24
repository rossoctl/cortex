package facts

import (
	"testing"
	"time"

	"github.com/rossoctl/accountant"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name, path string
		want       Provider
	}{
		{"anthropic api", "/v1/messages", ProviderAnthropic},
		{"openai api", "/v1/chat/completions", ProviderOpenAI},
		// A LiteLLM gateway is classified by the wire dialect of the path it was called on, not
		// by its own hostname, because the dialect is what decides which provider's cache rules
		// apply on the wire.
		{"litellm anthropic dialect", "/v1/messages", ProviderAnthropic},
		{"unknown path", "/v1/embeddings", ProviderUnknown},
		// Classify shares wire.FormatOf's table, so these two endpoints — which a narrower,
		// package-local path table previously missed — are recognized here too.
		{"azure chat completions", "/openai/deployments/gpt4/chat/completions", ProviderOpenAI},
		{"codex responses", "/backend-api/codex/responses", ProviderOpenAI},
		// Legacy completions: wire.FormatOf deliberately excludes these (no messages array, no
		// cache_control), so Classify gives them ProviderUnknown rather than guessing OpenAI.
		{"legacy completions", "/v1/completions", ProviderUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.path); got != c.want {
				t.Errorf("Classify(%q) = %v, want %v", c.path, got, c.want)
			}
		})
	}
}

// TestFor_Anthropic pins entryFacts' CacheLifetimeExact branch: only TTL and Lifetime are set,
// never MinCacheable or Provider — the proxy's entryFacts does not set those fields either.
func TestFor_Anthropic(t *testing.T) {
	cases := []struct {
		name          string
		allMarksAsk1h bool
		wantTTL       time.Duration
	}{
		{"no marks ask 1h", false, 5 * time.Minute},
		{"every mark asks 1h", true, time.Hour},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := For(ProviderAnthropic, "claude-sonnet-4-5", c.allMarksAsk1h)
			if got.TTL != c.wantTTL {
				t.Errorf("TTL = %v, want %v", got.TTL, c.wantTTL)
			}
			if got.Lifetime != accountant.LifetimeExact {
				t.Errorf("Lifetime = %v, want %v", got.Lifetime, accountant.LifetimeExact)
			}
			if got.MinCacheable != 0 {
				t.Errorf("MinCacheable = %d, want 0 (the proxy's entryFacts never sets it)", got.MinCacheable)
			}
			if got.Provider.ReadGranularity != 0 {
				t.Errorf("Provider.ReadGranularity = %d, want 0", got.Provider.ReadGranularity)
			}
			if got.Provider.TTLs != nil {
				t.Errorf("Provider.TTLs = %v, want nil", got.Provider.TTLs)
			}
		})
	}
}

func TestFor_Unknown(t *testing.T) {
	got := For(ProviderUnknown, "whatever-model", false)
	if got.Lifetime != accountant.LifetimeUnknown {
		t.Errorf("Lifetime = %v, want %v", got.Lifetime, accountant.LifetimeUnknown)
	}
	if got.TTL != 5*time.Minute {
		t.Errorf("TTL = %v, want 5m", got.TTL)
	}
}

// TestFor_OpenAI pins modelinfo.OpenAI30MinuteCache's gate: GPT-5.6 and later get the documented
// 30-minute minimum; everything else — including a model this table cannot parse at all — gets
// the undocumented, unproven retention (LifetimeUnknown). Neither branch sets MinCacheable or
// Provider, matching entryFacts.
func TestFor_OpenAI(t *testing.T) {
	cases := []struct {
		name         string
		model        string
		wantLifetime accountant.LifetimeKind
		wantTTL      time.Duration
	}{
		{"gpt-5.6, documented 30-minute minimum", "gpt-5.6", accountant.LifetimeMinimum, 30 * time.Minute},
		{"gpt-5.6 with a routing prefix", "azure/gpt-5.6-luna", accountant.LifetimeMinimum, 30 * time.Minute},
		{"gpt-6, also covered by the 5.6+ gate", "gpt-6", accountant.LifetimeMinimum, 30 * time.Minute},
		{"gpt-5, older than the gate", "gpt-5", accountant.LifetimeUnknown, 5 * time.Minute},
		{"an unknown model name", "mystery-model", accountant.LifetimeUnknown, 5 * time.Minute},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := For(ProviderOpenAI, c.model, false)
			if got.Lifetime != c.wantLifetime {
				t.Errorf("Lifetime = %v, want %v", got.Lifetime, c.wantLifetime)
			}
			if got.TTL != c.wantTTL {
				t.Errorf("TTL = %v, want %v", got.TTL, c.wantTTL)
			}
			if got.MinCacheable != 0 {
				t.Errorf("MinCacheable = %d, want 0", got.MinCacheable)
			}
			if got.Provider.ReadGranularity != 0 {
				t.Errorf("Provider.ReadGranularity = %d, want 0", got.Provider.ReadGranularity)
			}
		})
	}
}
