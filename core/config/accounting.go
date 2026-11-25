package config

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

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
	switch strings.ToLower(a.Enabled) {
	case AccountingTrue:
		return true
	case AccountingFalse:
		return false
	}
	return hasOperator
}

// Validate refuses a value of Enabled other than "", "auto", "true" and "false", in any case:
// YAML's own boolean spellings (True, TRUE, False, FALSE) decode into this string field verbatim
// (unlike a *bool field, which yaml.v3 already folds them into), so this compares case-
// insensitively to accept every spelling a YAML user might write for the same value. It also
// refuses a negative subscription weight. Safe on a nil receiver.
func (a *AccountingConfig) Validate() error {
	if a == nil {
		return nil
	}
	switch strings.ToLower(a.Enabled) {
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

// MetricsPushConfig is the top-level `metrics_push:` block: where Cortex pushes the accountant's
// llm.savings.* counters, over OTLP/HTTP, to the central collector (design §5.10).
//
//	metrics_push:
//	  enabled: true     # default true, but with no URL nothing is sent
//	  url: ""           # no default; METRICS_PUSH_URL overrides
//	  token: ""         # METRICS_PUSH_TOKEN; sent as Authorization: Bearer <token>
//	  interval: 60s     # METRICS_PUSH_INTERVAL
//
// There is no default URL in code: with no URL nothing leaves the machine. The built-in
// ~/.cortex/config.yaml (cmd/cortex/local.go) writes the central collector's URL. The block is the same in a cluster and on a laptop.
//
// NOT HOT-RELOADABLE: the push is started once at boot, so a reload that changes the block is
// refused (reloader.validateReloadable), as for listener.*. The token is never logged; /config
// serves it redacted (core/redact matches the key "token").
type MetricsPushConfig struct {
	// Enabled is nil for the default, true.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// URL is the collector's base URL; the exporter posts to <url>/v1/metrics. https, or http
	// to a loopback address only.
	URL string `yaml:"url,omitempty" json:"url,omitempty"`
	// Token is the collector's bearer token. Empty sends no Authorization header.
	Token string `yaml:"token,omitempty" json:"token,omitempty"`
	// Interval is how often the counters are pushed. Zero means DefaultMetricsPushInterval.
	Interval time.Duration `yaml:"interval,omitempty" json:"interval,omitempty"`
}

// DefaultMetricsPushInterval is metrics_push.interval when the block and the environment leave
// it unset.
const DefaultMetricsPushInterval = 60 * time.Second

// Resolve returns the block with the environment applied: METRICS_PUSH (a boolean),
// METRICS_PUSH_URL, METRICS_PUSH_TOKEN and METRICS_PUSH_INTERVAL (a Go duration) override the
// file, and an unset interval becomes DefaultMetricsPushInterval. Safe on a nil receiver; the
// receiver is not changed.
//
// A METRICS_PUSH that is not a boolean turns the push off, so a typo never sends anything. A
// METRICS_PUSH_INTERVAL that is not a positive duration keeps the configured interval.
func (m *MetricsPushConfig) Resolve(env func(string) string) MetricsPushConfig {
	var out MetricsPushConfig
	if m != nil {
		out = *m
		if m.Enabled != nil {
			v := *m.Enabled
			out.Enabled = &v
		}
	}
	if env == nil {
		env = func(string) string { return "" }
	}
	if v := strings.TrimSpace(env("METRICS_PUSH")); v != "" {
		on, err := strconv.ParseBool(v)
		if err != nil {
			on = false
		}
		out.Enabled = &on
	}
	if v := strings.TrimSpace(env("METRICS_PUSH_URL")); v != "" {
		out.URL = v
	}
	if v := env("METRICS_PUSH_TOKEN"); v != "" {
		out.Token = v
	}
	if v := strings.TrimSpace(env("METRICS_PUSH_INTERVAL")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			out.Interval = d
		}
	}
	if out.Interval == 0 {
		out.Interval = DefaultMetricsPushInterval
	}
	return out
}

// IsEnabled is Enabled with its default, true.
func (m MetricsPushConfig) IsEnabled() bool { return m.Enabled == nil || *m.Enabled }

// On reports whether anything is pushed: enabled, and a URL to push to.
func (m MetricsPushConfig) On() bool { return m.IsEnabled() && m.URL != "" }

// Validate refuses a URL that is not https, other than http to a loopback address, a URL with
// user info or a query string (neither is sent: the credential belongs in Token), and a
// negative interval. An empty URL is valid: nothing is pushed. The error never quotes the token,
// and quotes the URL only as SafeURL gives it.
//
// WHERE A BAD BLOCK IS CAUGHT: the block as written in the YAML is checked by config.Validate,
// so a bad one fails config.Load like any other block. The METRICS_PUSH_* environment is applied
// at runtime (Resolve) and checked again by push.Start; a bad METRICS_PUSH_URL only WARNs and
// turns the push off, and the proxy runs (accounting fails open).
func (m MetricsPushConfig) Validate() error {
	if m.Interval < 0 {
		return fmt.Errorf("metrics_push.interval: %v is negative", m.Interval)
	}
	if m.URL == "" {
		return nil
	}
	u, err := url.Parse(m.URL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("metrics_push.url: %q is not an absolute URL", SafeURL(m.URL))
	}
	// The exporter posts to SafeURL(url), which drops both, so a credential there would be lost
	// on the way out and the collector would answer 401 with nothing to say why.
	if u.User != nil || u.RawQuery != "" || u.ForceQuery {
		return fmt.Errorf("metrics_push.url: %q carries user info or a query string, which are not sent; "+
			"put the collector's credential in metrics_push.token (METRICS_PUSH_TOKEN)", SafeURL(m.URL))
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("metrics_push.url: %q is http to a non-loopback host; use https", SafeURL(m.URL))
	}
	return fmt.Errorf("metrics_push.url: scheme %q is not https", u.Scheme)
}

// SafeURL is u with no user info, query or fragment: scheme, host and path at most, the only
// parts of a metrics_push.url that may reach an error or a log. A credential can hide in the
// user info ("https://user:pass@host") or the query ("?token=..."). An unparseable URL gives a
// placeholder, never the input.
func SafeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "<unparseable URL>"
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
}

// isLoopbackHost is true for localhost and for a loopback IP literal.
func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
