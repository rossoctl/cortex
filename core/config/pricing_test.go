package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestConfig_PricingSectionParses(t *testing.T) {
	var c Config
	src := `
mode: proxy-sidecar
pricing:
  bundled: false
  endpoints:
    - hosts: [gw.internal]
      models:
        "*claude-opus-*":
          input_cost_per_million: 3.80
          output_cost_per_million: 19.00
`
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	if c.Pricing == nil {
		t.Fatal("pricing section did not parse")
	}
	if c.Pricing.BundledEnabled() {
		t.Error("bundled: false did not take effect")
	}
	if len(c.Pricing.Endpoints) != 1 || len(c.Pricing.Endpoints[0].Hosts) != 1 || c.Pricing.Endpoints[0].Hosts[0] != "gw.internal" {
		t.Fatalf("endpoints = %+v", c.Pricing.Endpoints)
	}
	m, ok := c.Pricing.Endpoints[0].Models["*claude-opus-*"]
	if !ok {
		t.Fatalf("model key missing: %+v", c.Pricing.Endpoints[0].Models)
	}
	if m.InputCostPerMillion != 3.80 || m.OutputCostPerMillion != 19.00 {
		t.Errorf("rates = %+v", m)
	}
}

func TestConfig_PricingAbsentIsNil(t *testing.T) {
	// Absent block means bundled-only, which BundledEnabled reports for nil.
	var c Config
	if err := yaml.Unmarshal([]byte("mode: proxy-sidecar\n"), &c); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	if c.Pricing != nil {
		t.Errorf("Pricing = %+v, want nil when the block is absent", c.Pricing)
	}
	if !c.Pricing.BundledEnabled() {
		t.Error("a nil pricing section should still enable the bundled table")
	}
}

func TestValidate_RejectsAnUnbuildablePricingSection(t *testing.T) {
	// Pricing faults must fail at startup, beside the other config validation,
	// rather than at first priced request — where the symptom would be silently
	// unpriced traffic rather than an error.
	var c Config
	src := `
mode: proxy-sidecar
listener:
  roles: [forward]
pricing:
  endpoints:
    - hosts: [gw.internal]
      models:
        "claude-opus-5":
          input_cost_per_million: 5.00
          input_cost_per_token: 0.000005
`
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	err := Validate(&c)
	if err == nil {
		t.Fatal("Validate accepted a pricing section that cannot build")
	}
	for _, want := range []string{"pricing", "claude-opus-5", "set one"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestValidate_AcceptsAValidPricingSection(t *testing.T) {
	var c Config
	src := `
mode: proxy-sidecar
listener:
  roles: [forward]
pricing:
  endpoints:
    - hosts: [gw.internal]
      models:
        "*": {input_cost_per_million: 3.80}
`
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	if err := Validate(&c); err != nil {
		t.Fatalf("Validate rejected a valid pricing section: %v", err)
	}
}

func TestValidate_AcceptsNoPricingSection(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("mode: proxy-sidecar\nlistener:\n  roles: [forward]\n"), &c); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	if err := Validate(&c); err != nil {
		t.Fatalf("Validate rejected a config with no pricing section: %v", err)
	}
}
