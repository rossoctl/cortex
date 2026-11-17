package facts

import (
	"testing"
	"time"

	"github.com/rossoctl/accountant"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name, host, path string
		want             Provider
	}{
		{"anthropic api", "api.anthropic.com", "/v1/messages", ProviderAnthropic},
		{"openai api", "api.openai.com", "/v1/chat/completions", ProviderOpenAI},
		// A LiteLLM gateway is classified by the wire dialect of the path it was called on, not
		// by its own hostname, because the dialect is what decides which provider's cache rules
		// apply on the wire.
		{"litellm anthropic dialect", "litellm.local:4000", "/v1/messages", ProviderAnthropic},
		{"unknown", "gateway.example.com", "/v1/embeddings", ProviderUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.host, c.path); got != c.want {
				t.Errorf("Classify(%q, %q) = %v, want %v", c.host, c.path, got, c.want)
			}
		})
	}
}

func TestFor_Anthropic(t *testing.T) {
	cases := []struct {
		name         string
		model        string
		breakpoints  []accountant.Breakpoint
		wantTTL      time.Duration
		wantMinCache int
	}{
		{"sonnet, no breakpoints", "claude-sonnet-4-5", nil, 5 * time.Minute, minCacheableDefault},
		{
			"sonnet, 1h breakpoint", "claude-sonnet-4-5",
			[]accountant.Breakpoint{{Pos: accountant.Position{Section: accountant.SectionSystem}, TTL: "1h"}},
			time.Hour, minCacheableDefault,
		},
		{"haiku, no breakpoints", "claude-haiku-4-5", nil, 5 * time.Minute, minCacheableHaiku},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := For(ProviderAnthropic, c.model, c.breakpoints)
			if got.TTL != c.wantTTL {
				t.Errorf("TTL = %v, want %v", got.TTL, c.wantTTL)
			}
			if got.Lifetime != accountant.LifetimeExact {
				t.Errorf("Lifetime = %v, want %v", got.Lifetime, accountant.LifetimeExact)
			}
			if got.MinCacheable != c.wantMinCache {
				t.Errorf("MinCacheable = %d, want %d", got.MinCacheable, c.wantMinCache)
			}
			if got.Provider.ReadGranularity != 1 {
				t.Errorf("ReadGranularity = %d, want 1", got.Provider.ReadGranularity)
			}
			wantTTLs := []string{"5m", "1h"}
			if len(got.Provider.TTLs) != len(wantTTLs) || got.Provider.TTLs[0] != wantTTLs[0] || got.Provider.TTLs[1] != wantTTLs[1] {
				t.Errorf("Provider.TTLs = %v, want %v", got.Provider.TTLs, wantTTLs)
			}
		})
	}
}

func TestFor_Unknown(t *testing.T) {
	got := For(ProviderUnknown, "whatever-model", nil)
	if got.Lifetime != accountant.LifetimeUnknown {
		t.Errorf("Lifetime = %v, want %v", got.Lifetime, accountant.LifetimeUnknown)
	}
	if got.TTL != 5*time.Minute {
		t.Errorf("TTL = %v, want 5m", got.TTL)
	}
}

func TestFor_OpenAI(t *testing.T) {
	got := For(ProviderOpenAI, "gpt-5", nil)
	if got.Lifetime != accountant.LifetimeUnknown {
		t.Errorf("Lifetime = %v, want %v", got.Lifetime, accountant.LifetimeUnknown)
	}
	if got.Provider.ReadGranularity != 128 {
		t.Errorf("ReadGranularity = %d, want 128", got.Provider.ReadGranularity)
	}
}
