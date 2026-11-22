package config

import (
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
	"gopkg.in/yaml.v3"
)

func TestAccountingConfig_On(t *testing.T) {
	cases := []struct {
		name        string
		a           *AccountingConfig
		hasOperator bool
		want        bool
	}{
		{"absent block, operator", nil, true, true},
		{"absent block, no operator", nil, false, false},
		{"empty is auto, operator", &AccountingConfig{}, true, true},
		{"empty is auto, no operator", &AccountingConfig{}, false, false},
		{"auto, operator", &AccountingConfig{Enabled: "auto"}, true, true},
		{"auto, no operator", &AccountingConfig{Enabled: "auto"}, false, false},
		{"true with no operator", &AccountingConfig{Enabled: "true"}, false, true},
		{"false with an operator", &AccountingConfig{Enabled: "false"}, true, false},
	}
	for _, c := range cases {
		if got := c.a.On(c.hasOperator); got != c.want {
			t.Errorf("%s: On = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAccountingConfig_ValidateRefusesOtherValues(t *testing.T) {
	for _, v := range []string{"", "auto", "true", "false", "TRUE", "False", "AUTO"} {
		if err := (&AccountingConfig{Enabled: v}).Validate(); err != nil {
			t.Errorf("%q: %v", v, err)
		}
	}
	for _, v := range []string{"yes", "on", "1"} {
		err := (&AccountingConfig{Enabled: v}).Validate()
		if err == nil || !strings.Contains(err.Error(), "accounting.enabled") {
			t.Errorf("%q: err = %v, want a refusal naming accounting.enabled", v, err)
		}
	}
	var nilBlock *AccountingConfig
	if err := nilBlock.Validate(); err != nil {
		t.Errorf("absent block: %v", err)
	}
}

// Review (r4663002497): yaml.v3 decodes YAML's True/TRUE/False/FALSE into this string field
// verbatim, so On and Validate must compare case-insensitively for them to mean what a *bool
// field would already accept.
func TestAccountingConfig_OnIsCaseInsensitive(t *testing.T) {
	if !(&AccountingConfig{Enabled: "TRUE"}).On(false) {
		t.Error(`Enabled: "TRUE" should force accounting on`)
	}
	if (&AccountingConfig{Enabled: "False"}).On(true) {
		t.Error(`Enabled: "False" should force accounting off`)
	}
}

// A YAML boolean must read as the string the field holds, so `enabled: true` and
// `enabled: "true"` mean the same.
func TestAccountingConfig_YAML(t *testing.T) {
	for doc, want := range map[string]string{
		"accounting:\n  enabled: true\n":     "true",
		"accounting:\n  enabled: false\n":    "false",
		"accounting:\n  enabled: auto\n":     "auto",
		"accounting:\n  enabled: \"true\"\n": "true",
	} {
		var c Config
		if err := yaml.Unmarshal([]byte(doc), &c); err != nil {
			t.Fatalf("%q: %v", doc, err)
		}
		if c.Accounting == nil || c.Accounting.Enabled != want {
			t.Errorf("%q: Enabled = %+v, want %q", doc, c.Accounting, want)
		}
	}
}

func TestHasContextOperator(t *testing.T) {
	entries := func(es ...PluginEntry) *Config {
		c := &Config{}
		c.Pipeline.Outbound.Plugins = es
		return c
	}
	cases := []struct {
		name string
		c    *Config
		want bool
	}{
		{"none", entries(PluginEntry{Name: "inference-parser"}), false},
		{"tool-prune", entries(PluginEntry{Name: "inference-parser"}, PluginEntry{Name: "tool-prune"}), true},
		{"context-guru", entries(PluginEntry{Name: "context-guru"}), true},
		{"an operator turned off is not one", entries(PluginEntry{Name: "tool-prune", OnError: pipeline.ErrorPolicyOff}), false},
		{"an operator under observe still reports", entries(PluginEntry{Name: "tool-prune", OnError: pipeline.ErrorPolicyObserve}), true},
	}
	for _, c := range cases {
		if got := HasContextOperator(c.c); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	inbound := &Config{}
	inbound.Pipeline.Inbound.Plugins = []PluginEntry{{Name: "tool-prune"}}
	if HasContextOperator(inbound) {
		t.Error("an inbound operator counts: only the outbound chain carries inference requests")
	}
	if !entries(PluginEntry{Name: "tool-prune"}).AccountingOn() {
		t.Error("AccountingOn: auto with tool-prune is off")
	}
}

func TestValidate_RefusesABadAccountingValue(t *testing.T) {
	c := &Config{Mode: ModeProxySidecar, Accounting: &AccountingConfig{Enabled: "sometimes"}}
	ApplyPreset(c)
	c.Listener.ReverseProxyBackend = "http://127.0.0.1:8000"
	if err := Validate(c); err == nil || !strings.Contains(err.Error(), "accounting.enabled") {
		t.Errorf("Validate = %v, want a refusal naming accounting.enabled", err)
	}
}

// accounting.subscription_weights (design §5.7a): absent means the defaults, the API rate ratios;
// a key that is set replaces only its own weight, and 0 is a weight like any other.
func TestAccountingConfig_SubscriptionWeights(t *testing.T) {
	defaults := SubscriptionWeights{Input: 1, CacheWrite5m: 1.25, CacheWrite1h: 2, CacheRead: 0.1, Output: 5}
	if got := DefaultSubscriptionWeights(); got != defaults {
		t.Errorf("DefaultSubscriptionWeights = %+v, want %+v", got, defaults)
	}
	var nilBlock *AccountingConfig
	if got := nilBlock.Weights(); got != defaults {
		t.Errorf("absent block: Weights = %+v, want the defaults", got)
	}
	if got := (&AccountingConfig{}).Weights(); got != defaults {
		t.Errorf("no subscription_weights: Weights = %+v, want the defaults", got)
	}

	doc := "accounting:\n  subscription_weights:\n    cache_read: 0\n    output: 4\n"
	var c Config
	if err := yaml.Unmarshal([]byte(doc), &c); err != nil {
		t.Fatal(err)
	}
	want := defaults
	want.CacheRead, want.Output = 0, 4
	if got := c.Accounting.Weights(); got != want {
		t.Errorf("Weights = %+v, want %+v", got, want)
	}

	full := "accounting:\n  subscription_weights:\n    input: 2\n    cache_write_5m: 3\n    cache_write_1h: 4\n    cache_read: 0.5\n    output: 6\n"
	c = Config{}
	if err := yaml.Unmarshal([]byte(full), &c); err != nil {
		t.Fatal(err)
	}
	if got := c.Accounting.Weights(); got != (SubscriptionWeights{2, 3, 4, 0.5, 6}) {
		t.Errorf("every key set: Weights = %+v", got)
	}
}

func TestAccountingConfig_ValidateRefusesANegativeWeight(t *testing.T) {
	for _, key := range []string{"input", "cache_write_5m", "cache_write_1h", "cache_read", "output"} {
		var c Config
		doc := "accounting:\n  subscription_weights:\n    " + key + ": -1\n"
		if err := yaml.Unmarshal([]byte(doc), &c); err != nil {
			t.Fatal(err)
		}
		err := c.Accounting.Validate()
		if err == nil || !strings.Contains(err.Error(), "accounting.subscription_weights."+key) {
			t.Errorf("%s: -1: err = %v, want a refusal naming accounting.subscription_weights.%s", key, err, key)
		}
	}
	if err := (&AccountingConfig{SubscriptionWeights: &SubscriptionWeights{}}).Validate(); err != nil {
		t.Errorf("all-zero weights: %v", err)
	}
}
