package config

import (
	"fmt"
)

// Validate checks the top-level runtime config: mode, listener combo,
// and that the pipeline composition is populated. Plugin-specific
// validation (issuer, token URL, identity type, jwt_audience) lives
// inside each plugin's Configure and runs at pipeline build time.
//
// Empty pipelines are rejected. Under the per-plugin config shape,
// a valid runtime config always names at least one inbound plugin
// (jwt-validation) and one outbound plugin (token-exchange). Silently
// accepting empty pipelines caused the whole point of authbridge to
// disappear — inbound traffic passing without JWT validation, outbound
// passing without token exchange. Operators upgrading from the old
// top-level-block schema ("inbound:", "outbound:", etc.) whose YAML
// does not yet include a pipeline section fail loudly here rather
// than shipping an open proxy. See the schema migration note in
// cmd/authbridge/README.md.
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
	return validatePipeline(cfg)
}

func validatePipeline(cfg *Config) error {
	if len(cfg.Pipeline.Inbound.Plugins) == 0 {
		return fmt.Errorf("pipeline.inbound.plugins is empty; specify at least one plugin " +
			"(typically jwt-validation) — see cmd/authbridge/README.md. " +
			"If you see this after an upgrade, your config.yaml is using the old top-level shape " +
			"(inbound:, outbound:, identity:, bypass:, routes:) — move those settings under " +
			"pipeline.*.plugins[].config")
	}
	if len(cfg.Pipeline.Outbound.Plugins) == 0 {
		return fmt.Errorf("pipeline.outbound.plugins is empty; specify at least one plugin " +
			"(typically token-exchange) — see cmd/authbridge/README.md")
	}
	return nil
}

func validateListeners(cfg *Config) error {
	switch cfg.Mode {
	case ModeEnvoySidecar:
		if cfg.Listener.ReverseProxyAddr != "" {
			return fmt.Errorf("envoy-sidecar mode does not support reverse_proxy_addr (use proxy-sidecar mode)")
		}
		if cfg.Listener.ExtAuthzAddr != "" {
			return fmt.Errorf("envoy-sidecar mode does not support ext_authz_addr (use waypoint mode)")
		}
	case ModeWaypoint:
		if cfg.Listener.ExtProcAddr != "" {
			return fmt.Errorf("waypoint mode does not support ext_proc_addr (use envoy-sidecar mode)")
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
		if cfg.Listener.ReverseProxyBackend == "" {
			return fmt.Errorf("proxy-sidecar mode requires listener.reverse_proxy_backend")
		}
	}
	return nil
}
