package pricing

import (
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
)

// Config is the top-level `pricing:` section of an AuthBridge config.
//
// One section for the whole process, not a knob per plugin. Before this, rates lived
// in two plugins' configs — 12 on tool-prune and 4 on litellm-budget-track — and a
// deployment that wanted consistent cost had to keep them in agreement by hand.
type Config struct {
	// Bundled controls whether the shipped price table participates. Default TRUE:
	// covering internal usage with no manual setup is the point, and LiteLLM's
	// public rates are not a secret. Set false to price only what you configure.
	Bundled *bool `yaml:"bundled" json:"bundled,omitempty"`

	// Endpoints scopes rates per target. This is what lets one process price
	// several gateways and the vendor endpoint differently, which a single global
	// rate table cannot: the choice is per REQUEST, not per deployment.
	Endpoints []EndpointConfig `yaml:"endpoints" json:"endpoints,omitempty"`
}

// EndpointConfig prices the models reached through one endpoint.
type EndpointConfig struct {
	// Hosts are globs matched against the request's target host, port stripped.
	// An empty list, or any entry of "*", means any endpoint.
	//
	// A list rather than a single glob because gateways commonly share a rate card:
	// two replicas, or a service name and its external alias, bill identically, and
	// repeating the whole models block per host invites the two copies to drift.
	// Each host becomes its own table row.
	Hosts []string `yaml:"hosts" json:"hosts,omitempty"`

	// Models maps a model glob to its rates. Keys are matched
	// case-insensitively, and an exact key beats a glob.
	Models map[string]ModelConfig `yaml:"models" json:"models,omitempty"`

	// Multiplier scales every rate that resolves for these hosts. Absent means 1.0.
	//
	// A FRACTION of the resolved rate, so a 24% discount is 0.76. This is the whole
	// configuration most deployments need: a gateway discount is one scalar, and
	// stating it once tracks upstream repricing instead of freezing last quarter's
	// numbers into a copied rate card. See MultiplierRule.
	//
	// An endpoint with a multiplier needs no models block — it scales what already
	// resolves, including models no entry names.
	Multiplier *float64 `yaml:"multiplier" json:"multiplier,omitempty"`
}

// ModelConfig is one model pattern's rates, optionally with long-context
// overrides.
type ModelConfig struct {
	TierRates `yaml:",inline"`

	// Above overrides the base rates once a request's prompt exceeds a token
	// count. Crossing 200k prompt tokens is what a long agent session IS, and
	// pricing that traffic at the base rate understates it.
	Above []ThresholdConfig `yaml:"above" json:"above,omitempty"`
}

// ThresholdConfig is one long-context breakpoint.
type ThresholdConfig struct {
	// PromptTokens is the count the prompt must EXCEED for these rates to apply.
	PromptTokens int `yaml:"prompt_tokens" json:"prompt_tokens"`

	TierRates `yaml:",inline"`
}

// TierRates is the operator-facing rate block: every tier expressible in either
// unit.
//
// The per-million fields are the ones to reach for. Providers publish prices per
// million tokens ("$3.80 / Mtok"), so that is the unit an operator already has in
// hand — no dividing by a million by hand, and no 0.0000038-vs-0.000038 typo that
// misprices by 10x and looks plausible either way.
//
// The per-token fields stay accepted because LiteLLM's own
// model_prices_and_context_window.json is per-token, and rates get copied straight
// out of it.
type TierRates struct {
	InputCostPerMillion      float64 `yaml:"input_cost_per_million" json:"input_cost_per_million,omitempty"`
	CacheWriteCostPerMillion float64 `yaml:"cache_write_cost_per_million" json:"cache_write_cost_per_million,omitempty"`
	CacheReadCostPerMillion  float64 `yaml:"cache_read_cost_per_million" json:"cache_read_cost_per_million,omitempty"`
	OutputCostPerMillion     float64 `yaml:"output_cost_per_million" json:"output_cost_per_million,omitempty"`

	InputCostPerToken      float64 `yaml:"input_cost_per_token" json:"input_cost_per_token,omitempty"`
	CacheWriteCostPerToken float64 `yaml:"cache_write_cost_per_token" json:"cache_write_cost_per_token,omitempty"`
	CacheReadCostPerToken  float64 `yaml:"cache_read_cost_per_token" json:"cache_read_cost_per_token,omitempty"`
	OutputCostPerToken     float64 `yaml:"output_cost_per_token" json:"output_cost_per_token,omitempty"`
}

// tokensPerMillion converts the published unit to the per-token one all the
// downstream arithmetic uses.
const tokensPerMillion = 1_000_000

// BundledEnabled reports whether the shipped table participates, defaulting to
// true for a nil Config or an unset field.
func (c *Config) BundledEnabled() bool {
	if c == nil || c.Bundled == nil {
		return true
	}
	return *c.Bundled
}

// Build assembles a Table from config plus, unless disabled, the bundled slice.
//
// A nil Config is legal and yields the bundled table alone — the no-configuration
// case, which is meant to work.
//
// Configured rows are emitted at ProvConfigured so they outrank anything bundled
// at equal specificity: an override is an override.
func Build(cfg *Config) (*Table, error) {
	var entries []Entry
	var mults []MultiplierRule
	if cfg.BundledEnabled() {
		entries = append(entries, Bundled()...)
		// Shipped gateway discounts travel with the shipped rates: the rates are
		// vendor list, and for the gateways named here list is a third too high.
		// Disabling the bundled table disables both, which is the right pairing —
		// a multiplier on rates you did not ship scales somebody else's numbers.
		mults = append(mults, bundledMultipliers()...)
	}
	if cfg != nil {
		configured, err := cfg.entries()
		if err != nil {
			return nil, err
		}
		entries = append(entries, configured...)
		mults = append(mults, cfg.multipliers()...)
	}
	return NewTable(entries, mults...)
}

// multipliers converts the config's endpoint blocks into multiplier rules.
func (c *Config) multipliers() []MultiplierRule {
	if c == nil {
		return nil
	}
	var out []MultiplierRule
	for _, ep := range c.Endpoints {
		if ep.Multiplier == nil {
			continue
		}
		hosts := ep.Hosts
		if len(hosts) == 0 {
			hosts = []string{""}
		}
		for _, h := range hosts {
			out = append(out, MultiplierRule{Host: h, Factor: *ep.Multiplier, Prov: ProvConfigured})
		}
	}
	return out
}

// entries converts the config's endpoint/model blocks into table rows.
func (c *Config) entries() ([]Entry, error) {
	var out []Entry
	for i, ep := range c.Endpoints {
		where := fmt.Sprintf("pricing.endpoints[%d]", i)
		if len(ep.Hosts) > 0 {
			where = fmt.Sprintf("pricing.endpoints[%d] (hosts %v)", i, ep.Hosts)
		}
		// An empty list means "any endpoint", which one "" row expresses.
		hosts := ep.Hosts
		if len(hosts) == 0 {
			hosts = []string{""}
		}
		for _, h := range hosts {
			if strings.TrimSpace(h) == "" && len(ep.Hosts) > 0 {
				return nil, fmt.Errorf("%s: hosts contains an empty entry; omit the key entirely to mean any endpoint", where)
			}
		}
		if len(ep.Models) == 0 {
			if ep.Multiplier != nil {
				// A multiplier-only block scales what already resolves, so demanding
				// rates here would defeat the point of expressing a discount once.
				continue
			}
			return nil, fmt.Errorf("%s: no models or multiplier configured; an endpoint block with neither prices nothing", where)
		}
		// Sorted so a config with several faults reports the same one across
		// restarts, instead of whichever map iteration reached first.
		models := make([]string, 0, len(ep.Models))
		for pattern := range ep.Models {
			models = append(models, pattern)
		}
		sort.Strings(models)

		for _, pattern := range models {
			rates, err := ep.Models[pattern].rates(fmt.Sprintf("%s model %q", where, pattern))
			if err != nil {
				return nil, err
			}
			if !rates.any() {
				return nil, fmt.Errorf("%s model %q: no rate set for any tier", where, pattern)
			}
			for _, h := range hosts {
				out = append(out, Entry{
					Host:  h,
					Model: pattern,
					Rates: rates,
					Prov:  ProvConfigured,
				})
			}
		}
	}
	return out, nil
}

// rates converts one model block, thresholds included.
func (m ModelConfig) rates(what string) (Rates, error) {
	base, set, err := m.TierRates.resolve(what)
	if err != nil {
		return Rates{}, err
	}
	r := Rates{Base: base, Set: set}
	for i, th := range m.Above {
		if th.PromptTokens <= 0 {
			return Rates{}, fmt.Errorf("%s: above[%d] needs a positive prompt_tokens (the count a prompt must exceed)", what, i)
		}
		rate, tset, err := th.TierRates.resolve(fmt.Sprintf("%s above[%d]", what, i))
		if err != nil {
			return Rates{}, err
		}
		var any bool
		for _, s := range tset {
			if s {
				any = true
				break
			}
		}
		if !any {
			return Rates{}, fmt.Errorf("%s: above[%d] sets no rate, so it would override nothing", what, i)
		}
		r.Thresholds = append(r.Thresholds, ContextThreshold{
			AbovePromptTokens: th.PromptTokens,
			Rate:              rate,
			Set:               tset,
		})
	}
	return r, nil
}

// resolve folds the two units into per-token rates.
//
// Setting BOTH units for the same tier is rejected rather than resolved by
// precedence. They differ by 10^6, so silently picking a winner either overstates
// a figure a millionfold or buries it below rounding — and the readout gives an
// operator no way to tell which unit was honoured. A startup error naming the tier
// is the only outcome that cannot be misread.
func (t TierRates) resolve(what string) (rate [numTiers]float64, set [numTiers]bool, err error) {
	for _, f := range []struct {
		tier    Tier
		name    string
		million float64
		token   float64
	}{
		{TierInput, "input", t.InputCostPerMillion, t.InputCostPerToken},
		{TierCacheWrite, "cache_write", t.CacheWriteCostPerMillion, t.CacheWriteCostPerToken},
		{TierCacheRead, "cache_read", t.CacheReadCostPerMillion, t.CacheReadCostPerToken},
		{TierOutput, "output", t.OutputCostPerMillion, t.OutputCostPerToken},
	} {
		for unit, v := range map[string]float64{
			f.name + "_cost_per_million": f.million,
			f.name + "_cost_per_token":   f.token,
		} {
			// Non-finite or negative rates are rejected at startup. A negative rate
			// makes a request's cost negative, which the accumulation path drops —
			// so the request would silently neither charge nor count.
			if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
				return rate, set, fmt.Errorf("%s: %s must be finite and >= 0", what, unit)
			}
		}
		if f.million > 0 && f.token > 0 {
			return rate, set, fmt.Errorf("%s: %s rate set as both %s_cost_per_million and %s_cost_per_token; set one",
				what, f.name, f.name, f.name)
		}
		switch {
		case f.million > 0:
			rate[f.tier], set[f.tier] = f.million/tokensPerMillion, true
		case f.token > 0:
			rate[f.tier], set[f.tier] = f.token, true
		}
	}
	return rate, set, nil
}

// WarnIfUnpinned logs once at startup when the process will price every endpoint from
// the bundled table.
//
// The bundled rates are VENDOR LIST. A deployment behind a gateway that bills below
// list is overstated — measurably so: the gateway these rates replaced billed at 0.76x
// vendor list, making every figure 1.32x high. Nothing in the running system says so,
// because an overstated figure looks exactly like an accurate one.
//
// Deliberately a WARN and not an error: pricing from list is a reasonable default and
// the correct answer for anyone talking straight to the vendor. It is only wrong
// silently, which is what this fixes.
func (c *Config) WarnIfUnpinned(log *slog.Logger) {
	if !c.BundledEnabled() {
		return // pricing only what was configured; nothing to warn about
	}
	if c != nil && len(c.Endpoints) > 0 {
		return // the operator has pinned something
	}
	log.Warn("pricing: every endpoint will price from the bundled table, which ships VENDOR LIST rates",
		"effect", "a gateway that bills below list is OVERSTATED — measured at 0.76x list on the shipped gateways, so ~1.32x high without a multiplier",
		"fix", "set pricing.endpoints[].multiplier for your gateway (one scalar; 0.76 means a 24% discount), or per-model rates; see docs/plugin-catalog.md",
		"note", "gateways matching the shipped rules already have a multiplier applied and need nothing",
		"check", "abctl annotates the cost total [bundled] rather than [configured]")
}
