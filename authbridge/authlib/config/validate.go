package config

import (
	"fmt"

	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
)

// Validate checks the top-level runtime config: mode and listener combo.
// Plugin-specific validation (issuer, token URL, identity type,
// jwt_audience) lives inside each plugin's Configure and runs at
// pipeline build time.
//
// Empty pipelines are permitted: AuthBridge will run as a pass-through
// on any stage with no plugins. This supports testing scenarios and
// asymmetric deployments (e.g. inbound auth only, no outbound token
// exchange). Operators get a startup WARN per empty stage from
// WarnEmptyPipelines so the open-proxy condition is visible in logs.
func Validate(cfg *Config) error {
	switch cfg.Mode {
	case ModeEnvoySidecar, ModeWaypoint, ModeProxySidecar:
		// valid
	case "":
		return fmt.Errorf("mode is required (envoy-sidecar, waypoint, or proxy-sidecar)")
	default:
		return fmt.Errorf("unknown mode %q (valid: envoy-sidecar, waypoint, proxy-sidecar)", cfg.Mode)
	}
	if err := validateListeners(cfg); err != nil {
		return err
	}
	if err := validateCostLedger(cfg); err != nil {
		return err
	}
	return validatePricing(cfg)
}

// validateCostLedger refuses `cost_ledger.enabled: true` alongside
// `session.enabled: false`.
//
// The ledger is not an independent subsystem: it records by being added to the
// session store as a Recorder (sessions.AddRecorder(costLedger)), so with the store
// absent there is no path by which an event can reach it. The whole ledger block in
// authbridge-proxy's main is nested inside `if cfg.Session.SessionEnabled()`, which
// meant this pair produced no error, no warning, and no log line naming the ledger at
// all — the operator who turned it on got silence, and the only way to find out was
// to read main.go.
//
// REFUSED rather than warned, because the combination cannot be made to work by any
// deployment decision: it is not "on but degraded", it is two settings that
// contradict each other. It is also refused on the EXPLICIT true only. An absent
// `enabled` means "the caller's default", which is on for a local install and off in
// Kubernetes (see CostLedgerConfig) — a deployment that legitimately wants sessions
// off has not asked for a ledger there, and failing its startup over a default it
// never wrote would be the wrong trade. That case gets a Warn at the call site that
// knows which default applies, since only the binary knows.
func validateCostLedger(cfg *Config) error {
	if cfg.CostLedger == nil || cfg.CostLedger.Enabled == nil || !*cfg.CostLedger.Enabled {
		return nil
	}
	if cfg.Session.SessionEnabled() {
		return nil
	}
	return fmt.Errorf("cost_ledger.enabled: true requires session tracking, but session.enabled is false: " +
		"the ledger records through the session store (it is registered as a Recorder on it), so with the " +
		"store off nothing can reach it and no cost history would be written — set session.enabled: true, " +
		"or drop cost_ledger.enabled")
}

// validatePricing builds the rate table and discards it, so a fault in the
// `pricing:` section fails at startup beside every other config error.
//
// Building is the validation: the table's constructor is what rejects a malformed
// glob, both units set for one tier, a non-finite rate, or a row that prices
// nothing. Deferring to first use would surface those as silently unpriced traffic
// instead of an error, and unpriced traffic looks exactly like a deployment with no
// rates configured.
func validatePricing(cfg *Config) error {
	if _, err := pricing.Build(cfg.Pricing); err != nil {
		return err
	}
	return nil
}

func validateListeners(cfg *Config) error {
	switch cfg.Mode {
	case ModeEnvoySidecar:
		if cfg.Listener.ReverseProxyAddr != "" {
			return fmt.Errorf("envoy-sidecar mode does not support reverse_proxy_addr (use proxy-sidecar mode)")
		}
		if cfg.Listener.InboundInterception != "" {
			return fmt.Errorf("envoy-sidecar mode does not support inbound_interception (Envoy already intercepts inbound transparently)")
		}
		if cfg.Listener.ExtAuthzAddr != "" {
			return fmt.Errorf("envoy-sidecar mode does not support ext_authz_addr (use waypoint mode)")
		}
	case ModeWaypoint:
		if cfg.Listener.ExtProcAddr != "" {
			return fmt.Errorf("waypoint mode does not support ext_proc_addr (use envoy-sidecar mode)")
		}
		if cfg.Listener.InboundInterception != "" {
			return fmt.Errorf("waypoint mode does not support inbound_interception (the waypoint owns inbound)")
		}
		if cfg.Listener.ReverseProxyAddr != "" {
			return fmt.Errorf("waypoint mode does not support reverse_proxy_addr")
		}
	case ModeProxySidecar:
		if cfg.Listener.ExtProcAddr != "" {
			return fmt.Errorf("proxy-sidecar mode does not support ext_proc_addr (use envoy-sidecar mode)")
		}
		if cfg.Listener.ExtAuthzAddr != "" {
			return fmt.Errorf("proxy-sidecar mode does not support ext_authz_addr (use waypoint mode)")
		}
		for _, r := range cfg.Listener.Roles {
			if r != RoleReverse && r != RoleForward {
				return fmt.Errorf("listener.roles: %q is not a valid role (use %q and/or %q)", r, RoleReverse, RoleForward)
			}
		}
		switch cfg.Listener.InboundInterception {
		case "", InboundInterceptionReverseProxy, InboundInterceptionTransparent:
			// valid
		default:
			return fmt.Errorf("listener.inbound_interception: %q is not valid (use %q or %q)",
				cfg.Listener.InboundInterception, InboundInterceptionReverseProxy, InboundInterceptionTransparent)
		}
		roles := cfg.Listener.ActiveRoles()
		if cfg.Listener.InboundTransparent() && !roles[RoleReverse] {
			return fmt.Errorf("listener.inbound_interception: transparent requires the %q role (it selects how inbound reaches the inbound pipeline)", RoleReverse)
		}
		// The reverse proxy forwards inbound traffic to reverse_proxy_backend,
		// so it's required only when the reverse role is active. A forward-only
		// deployment needs no backend, and transparent interception derives the
		// backend per connection from SO_ORIGINAL_DST rather than from config.
		if roles[RoleReverse] && !cfg.Listener.InboundTransparent() && cfg.Listener.ReverseProxyBackend == "" {
			return fmt.Errorf("proxy-sidecar mode with the reverse role requires listener.reverse_proxy_backend")
		}
		// The TLS bridge only rewrites outbound (forward-proxy) traffic; enabling
		// it without the forward role would be a silent no-op.
		if cfg.TLSBridge != nil && cfg.TLSBridge.Mode == "enabled" && !roles[RoleForward] {
			return fmt.Errorf("tls_bridge requires the forward role (it only affects outbound traffic)")
		}
	}
	return nil
}
