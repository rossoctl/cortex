package config

// ApplyPreset fills in mode-specific defaults for listener addresses.
// Plugin-specific defaults live inside each plugin's Configure (see
// docs/plugin-reference.md); the runtime config no
// longer shapes plugin behavior.
func ApplyPreset(cfg *Config) {
	switch cfg.Mode {
	case ModeEnvoySidecar:
		setDefault(&cfg.Listener.ExtProcAddr, ":9090")

	case ModeProxySidecar:
		// Fill an addr default only for an active role (empty Roles => both),
		// so a forward-only or reverse-only deployment doesn't bind the proxy
		// it didn't ask for. main.go starts a proxy iff its role is active.
		roles := cfg.Listener.ActiveRoles()
		if roles[RoleReverse] {
			// The two inbound mechanisms are mutually exclusive: transparent
			// interception REDIRECTs to its own port and leaves the agent on the
			// port it already binds, so filling reverse_proxy_addr there would
			// bind a port nothing routes to (and, if it collided with the agent's
			// own port, would break the pod).
			if cfg.Listener.InboundTransparent() {
				setDefault(&cfg.Listener.TransparentInboundAddr, ":8083")
			} else {
				setDefault(&cfg.Listener.ReverseProxyAddr, ":8080")
			}
		}
		if roles[RoleForward] {
			setDefault(&cfg.Listener.ForwardProxyAddr, ":8081")
			// Outbound transparent listener for enforce-redirect mode. Binding it
			// is harmless when nothing is redirected here (cooperative HTTP_PROXY
			// deployments simply never receive connections on it).
			setDefault(&cfg.Listener.TransparentProxyAddr, ":8082")
		}
	}

	// Session events API is default-on for every mode. Operators who
	// want to turn it off can disable session tracking entirely via
	// session.enabled: false — main.go skips the API server when the
	// store itself is nil.
	setDefault(&cfg.Listener.SessionAPIAddr, ":9094")

	// Health server is default-on for every mode; ":9091" is what the operator's
	// probe config and the container images expect.
	setDefault(&cfg.Listener.HealthAddr, ":9091")

	// LAST, so it wins over every default set above. This must live in ApplyPreset
	// and not in Load: the binaries call Load first and ApplyPreset second, so a
	// rule placed in Load runs BEFORE the wildcard defaults exist and silently
	// protects nothing — which is precisely the health_addr / transparent_proxy_addr
	// case it is here to fix.
	if cfg.Listener.BindLoopbackOnly {
		for _, addr := range []*string{
			&cfg.Listener.ExtProcAddr,
			&cfg.Listener.ForwardProxyAddr,
			&cfg.Listener.ReverseProxyAddr,
			&cfg.Listener.TransparentInboundAddr,
			&cfg.Listener.TransparentProxyAddr,
			&cfg.Listener.SessionAPIAddr,
			&cfg.Listener.HealthAddr,
			&cfg.Stats.StatsAddress,
		} {
			*addr = forceLocalhost(*addr)
		}
	}
}

func setDefault(field *string, value string) {
	if *field == "" {
		*field = value
	}
}
