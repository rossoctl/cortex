package redact

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/cost/pricing"
)

// The cases from #1564: real secret-bearing shapes that JSON used to pass
// through untouched. Each row must come out with the secret value absent.
func TestJSON_RedactsNonStringAndPluralSecrets(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		probe string // substring that must NOT survive redaction
	}{
		{
			// static-inject's config block: host -> credential, nested object.
			name:  "static-inject mappings",
			in:    `{"source":"mappings","mappings":{"api.openai.com":"sk-live-123"}}`,
			probe: "sk-live-123",
		},
		{
			// Plural key — suffix matching on the singular never fires.
			name:  "plural key",
			in:    `{"credentials":"c"}`,
			probe: `"c"`,
		},
		{
			// Plural key with an array value — neither the plural nor the
			// non-string value was redacted.
			name:  "plural key, array value",
			in:    `{"api_keys":["k1"]}`,
			probe: "k1",
		},
		{
			// Non-string scalar under a sensitive singular key.
			name:  "numeric secret",
			in:    `{"client_secret":12345}`,
			probe: "12345",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := JSON(json.RawMessage(tc.in))
			if strings.Contains(string(out), tc.probe) {
				t.Errorf("redacted output still contains %q: %s", tc.probe, out)
			}
		})
	}
}

// Token-COUNT fields end in "tokens", the plural of a sensitive stem, but a
// count is not a secret — and plugin config carrying them (session-budget's
// ceilings, context-guru's completion cap) is served redacted on /v1/pipeline
// and /config. The plural arm skips "token" for exactly this reason.
func TestJSON_TokenCountFieldsSurvive(t *testing.T) {
	in := json.RawMessage(`{"plugins":[{"name":"session-budget","config":{"max_tokens":100000,"max_input_tokens":50000,"total_tokens":7}}]}`)
	out := JSON(in)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	cfg := m["plugins"].([]any)[0].(map[string]any)["config"].(map[string]any)
	for k, want := range map[string]float64{
		"max_tokens":       100000,
		"max_input_tokens": 50000,
		"total_tokens":     7,
	} {
		if got, ok := cfg[k].(float64); !ok || got != want {
			t.Errorf("%s = %v, want %d (token counts must survive redaction)", k, cfg[k], int(want))
		}
	}
}

// Per-token rate fields end in "token", the singular stem, but a rate is not
// a secret — and /config serves the typed config redacted, which agentop exec
// decodes back into config.Config where the rates are float64. A "[REDACTED]"
// string there fails the decode, so the per_token suffix is exempt the way the
// tokens plural is.
func TestJSON_PerTokenRatesSurvive(t *testing.T) {
	in := json.RawMessage(`{"pricing":{"endpoints":[{"models":{"gpt-x":{"input_cost_per_token":0.000005,"cache_read_cost_per_token":0.0000005,"cache_write_cost_per_token":0.00000625,"output_cost_per_token":0.000015}}}]}}`)
	out := JSON(in)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	model := m["pricing"].(map[string]any)["endpoints"].([]any)[0].(map[string]any)["models"].(map[string]any)["gpt-x"].(map[string]any)
	for k, want := range map[string]float64{
		"input_cost_per_token":       0.000005,
		"cache_read_cost_per_token":  0.0000005,
		"cache_write_cost_per_token": 0.00000625,
		"output_cost_per_token":      0.000015,
	} {
		if got, ok := model[k].(float64); !ok || got != want {
			t.Errorf("%s = %v, want %v (per-token rates must survive redaction)", k, model[k], want)
		}
	}
}

// agentop's runningConfig (cmd/agentop/local_endpoint.go) fetches /config and
// unmarshals the redacted body into the typed config.Config. This pins that
// contract for the non-string fields that carry sensitive stems today — the
// per-token rates — via a Marshal → redact.JSON → Unmarshal round trip.
func TestJSON_ExecRoundTripThroughTypedConfig(t *testing.T) {
	cfg := &config.Config{
		Pricing: &pricing.Config{
			Endpoints: []pricing.EndpointConfig{{
				Models: map[string]pricing.ModelConfig{
					"gpt-x": {TierRates: pricing.TierRates{
						InputCostPerToken:  0.000005,
						OutputCostPerToken: 0.000015,
					}},
				},
			}},
		},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var back config.Config
	if uerr := json.Unmarshal(JSON(raw), &back); uerr != nil {
		t.Fatalf("redacted /config body no longer decodes into config.Config (agentop exec would break): %v", uerr)
	}
}
