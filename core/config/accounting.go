package config

import (
	"fmt"

	"github.com/rossoctl/cortex/core/pipeline"
	"gopkg.in/yaml.v3"
)

// AccountingConfig is the top-level `accounting:` block: whether the process hosts the
// accountant (core/accounting), which prices what the context operators saved with the same
// rules as the context-guru proxy (design §5.3).
//
//	accounting:
//	  enabled: auto   # auto | true | false
//	  subscription_weights:   # optional; defaults shown (design §5.7a)
//	    input: 1
//	    cache_write_5m: 1.25
//	    cache_write_1h: 2
//	    cache_read: 0.1
//	    output: 5
//
// The block is the same in a cluster and on a laptop; only the Tracker snapshot is
// laptop-only.
//
// NOT HOT-RELOADABLE from off to on: the host is built once at boot, so a reload that would turn
// accounting on when the process booted with it off is refused (reloader.validateAccounting).
// Turning it off is accepted on purpose, as a kill-switch for on_error: off, and the host goes
// idle. An edit that resolves the same is accepted either way.
type AccountingConfig struct {
	// Enabled is "auto" (the default, also spelled ""), "true" or "false". A string, not a
	// *bool, because it has three values; YAML's true and false decode into it as "true" and
	// "false".
	Enabled string `yaml:"enabled" json:"enabled"`

	// SubscriptionWeights value a saving on a subscription login in usage-limit units: tokens
	// times these weights. Nil means DefaultSubscriptionWeights. Read once, when the accountant
	// host is built, so a reload that changes them is refused (reloader.validateReloadable).
	SubscriptionWeights *SubscriptionWeights `yaml:"subscription_weights,omitempty" json:"subscription_weights,omitempty"`
}

// SubscriptionWeights are the usage-limit weight of one token in each tier (accountant design
// §7.1). A subscription bills no dollars per token; it draws down a usage limit, and the weights
// say how much of it a token of each tier uses. The defaults are the API rate ratios.
type SubscriptionWeights struct {
	Input        float64 `yaml:"input" json:"input"`
	CacheWrite5m float64 `yaml:"cache_write_5m" json:"cache_write_5m"`
	CacheWrite1h float64 `yaml:"cache_write_1h" json:"cache_write_1h"`
	CacheRead    float64 `yaml:"cache_read" json:"cache_read"`
	Output       float64 `yaml:"output" json:"output"`
}

// DefaultSubscriptionWeights are the API rate ratios: input 1, cache write 5m 1.25, cache write
// 1h 2, cache read 0.1, output 5 (design §5.7a).
func DefaultSubscriptionWeights() SubscriptionWeights {
	return SubscriptionWeights{Input: 1, CacheWrite5m: 1.25, CacheWrite1h: 2, CacheRead: 0.1, Output: 5}
}

// UnmarshalYAML starts from the defaults, so a key the block leaves out keeps its default weight
// and an explicit 0 stays 0.
func (w *SubscriptionWeights) UnmarshalYAML(n *yaml.Node) error {
	type plain SubscriptionWeights
	v := plain(DefaultSubscriptionWeights())
	if err := n.Decode(&v); err != nil {
		return err
	}
	*w = SubscriptionWeights(v)
	return nil
}

// Weights returns the configured subscription weights, or the defaults. Safe on a nil receiver.
func (a *AccountingConfig) Weights() SubscriptionWeights {
	if a == nil || a.SubscriptionWeights == nil {
		return DefaultSubscriptionWeights()
	}
	return *a.SubscriptionWeights
}

// The values AccountingConfig.Enabled takes.
const (
	AccountingAuto  = "auto"
	AccountingTrue  = "true"
	AccountingFalse = "false"
)

// On reports whether accounting runs. hasOperator is whether the outbound pipeline has a
// context operator (HasContextOperator). Under auto, accounting is on exactly when there is
// one: a pipeline with no operator has nothing to account for, so it pays nothing — no client
// body is kept and no buffering changes. "true" forces it on, for the cache figures alone.
// A nil receiver is auto. A value Validate refuses reads as auto.
func (a *AccountingConfig) On(hasOperator bool) bool {
	if a == nil {
		return hasOperator
	}
	switch a.Enabled {
	case AccountingTrue:
		return true
	case AccountingFalse:
		return false
	}
	return hasOperator
}

// Validate refuses a value of Enabled other than "", "auto", "true" and "false", and a negative
// subscription weight. Safe on a nil receiver.
func (a *AccountingConfig) Validate() error {
	if a == nil {
		return nil
	}
	switch a.Enabled {
	case "", AccountingAuto, AccountingTrue, AccountingFalse:
	default:
		return fmt.Errorf("accounting.enabled: %q is not one of auto, true, false", a.Enabled)
	}
	if w := a.SubscriptionWeights; w != nil {
		for _, f := range []struct {
			key string
			v   float64
		}{
			{"input", w.Input}, {"cache_write_5m", w.CacheWrite5m}, {"cache_write_1h", w.CacheWrite1h},
			{"cache_read", w.CacheRead}, {"output", w.Output},
		} {
			// !(v >= 0) also refuses NaN.
			if !(f.v >= 0) {
				return fmt.Errorf("accounting.subscription_weights.%s: %v is negative; a weight is 0 or more", f.key, f.v)
			}
		}
	}
	return nil
}

// contextOperators are the plugins that report context operations to the accountant
// (pctx.ReportOps). A new operator joins this list, or auto will not turn accounting on for it.
var contextOperators = map[string]bool{
	"context-guru": true,
	"tool-prune":   true,
}

// HasContextOperator reports whether c's outbound pipeline has a context operator that runs.
// Outbound only: inference requests leave through it. An entry turned off with on_error: off is
// not built, so it does not count; one under observe still reports its ops, as dry runs.
func HasContextOperator(c *Config) bool {
	if c == nil {
		return false
	}
	for _, e := range c.Pipeline.Outbound.Plugins {
		if contextOperators[e.Name] && e.OnError.Resolved() != pipeline.ErrorPolicyOff {
			return true
		}
	}
	return false
}

// AccountingOn is c.Accounting.On(HasContextOperator(c)): whether a process started from c hosts
// the accountant.
func (c *Config) AccountingOn() bool {
	return c.Accounting.On(HasContextOperator(c))
}
