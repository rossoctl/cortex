package pricing

import (
	"log/slog"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func mustYAML(t *testing.T, src string) *Config {
	t.Helper()
	var c Config
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	return &c
}

func TestBuild_NilConfigIsBundledOnly(t *testing.T) {
	// An operator who writes no pricing: section still gets the shipped table —
	// the explicit decision that internal usage works with no manual setup.
	tab, err := Build(nil)
	if err != nil {
		t.Fatalf("Build(nil): %v", err)
	}
	if _, p := tab.Resolve("api.anthropic.com", "claude-opus-5", 0); p != ProvBundled {
		t.Errorf("provenance = %s, want bundled", p)
	}
}

func TestBuild_BundledCanBeDisabled(t *testing.T) {
	tab, err := Build(mustYAML(t, "bundled: false\n"))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, p := tab.Resolve("api.anthropic.com", "claude-opus-5", 0); p != ProvNone {
		t.Errorf("provenance = %s, want none with bundled disabled", p)
	}
}

func TestBuild_ConfiguredOutranksBundled(t *testing.T) {
	tab, err := Build(mustYAML(t, `
endpoints:
  - hosts: [gw.internal]
    models:
      "*claude-opus-*":
        input_cost_per_million: 3.80
        cache_write_cost_per_million: 4.75
        cache_read_cost_per_million: 0.38
        output_cost_per_million: 19.00
`))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	r, p := tab.Resolve("gw.internal:4000", "claude-opus-5", 0)
	if p != ProvConfigured {
		t.Errorf("provenance = %s, want configured", p)
	}
	if got := inputPerMillion(r); got != 3.80 {
		t.Errorf("input rate = %v, want 3.80 (the gateway's, not vendor list)", got)
	}
	// Traffic to the vendor endpoint still gets vendor list from bundled.
	if _, p := tab.Resolve("api.anthropic.com", "claude-opus-5", 0); p != ProvBundled {
		t.Errorf("other endpoint provenance = %s, want bundled", p)
	}
}

func TestBuild_TwoGatewaysPricedDifferently(t *testing.T) {
	// The requirement the endpoint dimension exists for: one laptop, several
	// endpoints, each with its own pricing.
	tab, err := Build(mustYAML(t, `
endpoints:
  - hosts: [gw-a.internal]
    models:
      "*": {input_cost_per_million: 3.80}
  - hosts: [gw-b.internal]
    models:
      "*": {input_cost_per_million: 7.60}
`))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for host, want := range map[string]float64{"gw-a.internal": 3.80, "gw-b.internal": 7.60} {
		if got := inputPerMillion(first(tab.Resolve(host, "some-model", 0))); got != want {
			t.Errorf("%s input rate = %v, want %v", host, got, want)
		}
	}
}

func TestBuild_PerTokenUnitAccepted(t *testing.T) {
	// LiteLLM's own map is per-token, so rates get copied straight out of it.
	tab, err := Build(mustYAML(t, `
bundled: false
endpoints:
  - hosts: ["*"]
    models:
      "*": {input_cost_per_token: 0.0000038}
`))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := inputPerMillion(first(tab.Resolve("h", "m", 0))); got != 3.80 {
		t.Errorf("input rate = %v, want 3.80", got)
	}
}

func TestBuild_BothUnitsForOneTierIsAnError(t *testing.T) {
	// Rejected rather than resolved by precedence: the units differ by 10^6, so
	// silently picking a winner either overstates a figure a millionfold or buries
	// it below rounding, and the readout gives no way to tell which was honoured.
	_, err := Build(mustYAML(t, `
endpoints:
  - hosts: ["*"]
    models:
      "claude-opus-5":
        input_cost_per_million: 5.00
        input_cost_per_token: 0.000005
`))
	if err == nil {
		t.Fatal("Build accepted both units for one tier")
	}
	for _, want := range []string{"input", "claude-opus-5", "set one"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestBuild_RejectsNegativeAndNonFiniteRates(t *testing.T) {
	for name, src := range map[string]string{
		"negative": `
endpoints:
  - hosts: ["*"]
    models:
      "m": {input_cost_per_million: -1}
`,
		"nan": `
endpoints:
  - hosts: ["*"]
    models:
      "m": {input_cost_per_million: .nan}
`,
		"inf": `
endpoints:
  - hosts: ["*"]
    models:
      "m": {input_cost_per_million: .inf}
`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Build(mustYAML(t, src)); err == nil {
				t.Fatalf("Build accepted a %s rate", name)
			}
		})
	}
}

func TestBuild_ContextThresholds(t *testing.T) {
	tab, err := Build(mustYAML(t, `
bundled: false
endpoints:
  - hosts: ["*"]
    models:
      "*":
        input_cost_per_million: 3.00
        above:
          - prompt_tokens: 200000
            input_cost_per_million: 6.00
`))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := inputPerMillion(first(tab.Resolve("h", "m", 100_000))); got != 3.00 {
		t.Errorf("below threshold = %v, want 3.00", got)
	}
	if got := inputPerMillion(first(tab.Resolve("h", "m", 300_000))); got != 6.00 {
		t.Errorf("above threshold = %v, want 6.00", got)
	}
}

func TestBuild_ThresholdNeedsAPositivePromptTokens(t *testing.T) {
	_, err := Build(mustYAML(t, `
endpoints:
  - hosts: ["*"]
    models:
      "m":
        input_cost_per_million: 3.00
        above:
          - input_cost_per_million: 6.00
`))
	if err == nil {
		t.Fatal("Build accepted a threshold with no prompt_tokens")
	}
	if !strings.Contains(err.Error(), "prompt_tokens") {
		t.Errorf("error %q does not mention prompt_tokens", err)
	}
}

func TestBuild_RejectsAModelThatPricesNothing(t *testing.T) {
	// An empty model block matches traffic and then resolves it as unpriced, which
	// is indistinguishable from having no entry and hides the typo.
	_, err := Build(mustYAML(t, `
endpoints:
  - hosts: ["*"]
    models:
      "claude-opus-5": {}
`))
	if err == nil {
		t.Fatal("Build accepted a model with no rates")
	}
	if !strings.Contains(err.Error(), "claude-opus-5") {
		t.Errorf("error %q does not name the offending model", err)
	}
}

func TestBuild_RejectsAnEndpointWithNoModels(t *testing.T) {
	_, err := Build(mustYAML(t, `
endpoints:
  - hosts: [gw.internal]
`))
	if err == nil {
		t.Fatal("Build accepted an endpoint with no models")
	}
	if !strings.Contains(err.Error(), "gw.internal") {
		t.Errorf("error %q does not name the endpoint", err)
	}
}

func TestBuild_ErrorNamesTheEndpointAndModel(t *testing.T) {
	// An operator editing YAML needs to be told which row is wrong.
	_, err := Build(mustYAML(t, `
endpoints:
  - hosts: [gw.internal]
    models:
      "claude-[": {input_cost_per_million: 1}
`))
	if err == nil {
		t.Fatal("Build accepted a malformed model glob")
	}
	if !strings.Contains(err.Error(), "claude-[") {
		t.Errorf("error %q does not name the offending pattern", err)
	}
}

// The 1.32x overstatement on a discounted gateway is silent: an overstated figure looks
// exactly like an accurate one. A boot warning is the only thing that makes it visible.
func TestWarnIfUnpinned(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *Config
		warn bool
	}{
		{"no pricing section at all", nil, true},
		{"bundled on, nothing pinned", &Config{}, true},
		{"an endpoint pinned", &Config{Endpoints: []EndpointConfig{{Hosts: []string{"gw"}}}}, false},
		{"bundled disabled", &Config{Bundled: func() *bool { b := false; return &b }()}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf strings.Builder
			log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
			tc.cfg.WarnIfUnpinned(log)
			got := buf.String()
			if tc.warn && got == "" {
				t.Error("no warning for a deployment pricing everything from vendor list")
			}
			if !tc.warn && got != "" {
				t.Errorf("warned unnecessarily: %s", got)
			}
			if tc.warn {
				// The direction of the error and the remedy both have to be in it, or an
				// operator cannot act on it.
				for _, want := range []string{"VENDOR LIST", "OVERSTATED", "pricing.endpoints"} {
					if !strings.Contains(got, want) {
						t.Errorf("warning omits %q: %s", want, got)
					}
				}
			}
		})
	}
}
