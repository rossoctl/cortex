package config

import (
	"fmt"

	"github.com/rossoctl/cortex/core/pipeline"
)

// AccountingConfig is the top-level `accounting:` block: whether the process hosts the
// accountant (core/accounting), which prices what the context operators saved with the same
// rules as the context-guru proxy (design §5.3).
//
//	accounting:
//	  enabled: auto   # auto | true | false
//
// The block is the same in a cluster and on a laptop; only the Tracker snapshot is
// laptop-only.
//
// NOT HOT-RELOADABLE in its resolved answer. The host is built once at boot, and the listeners
// keep the client body only when it exists, so a reload that would turn accounting on or off is
// refused (reloader.validateReloadable). An edit that resolves the same is accepted.
type AccountingConfig struct {
	// Enabled is "auto" (the default, also spelled ""), "true" or "false". A string, not a
	// *bool, because it has three values; YAML's true and false decode into it as "true" and
	// "false".
	Enabled string `yaml:"enabled" json:"enabled"`
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

// Validate refuses a value of Enabled other than "", "auto", "true" and "false". Safe on a nil
// receiver.
func (a *AccountingConfig) Validate() error {
	if a == nil {
		return nil
	}
	switch a.Enabled {
	case "", AccountingAuto, AccountingTrue, AccountingFalse:
		return nil
	}
	return fmt.Errorf("accounting.enabled: %q is not one of auto, true, false", a.Enabled)
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
