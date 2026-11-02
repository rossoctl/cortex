package config

import "fmt"

// PlannedListener is one socket the proxy serves: Name is how callers ask for it and how
// an error names it, Addr as configured.
type PlannedListener struct {
	Name string
	Addr string
}

// ListenerPlan lists every socket the proxy serves, under the same conditions it serves
// them — the one list both the proxy (cmd/cortex's reserveListeners binds it, and take
// and claimed make a disagreement with what main serves fatal at boot) and anything that
// wants those sockets free before it starts (agentop setup's port preflight) work from.
//
// local is the local-install flag: a config under ~/.cortex, whichever flag started it.
// A local install has no iptables to REDIRECT to the transparent listeners, so it never
// binds them.
func ListenerPlan(cfg *Config, local bool) ([]PlannedListener, error) {
	var plan []PlannedListener
	roles := cfg.Listener.ActiveRoles()
	if roles[RoleReverse] {
		switch {
		case !cfg.Listener.InboundTransparent():
			plan = append(plan, PlannedListener{Name: "reverse-proxy", Addr: cfg.Listener.ReverseProxyAddr})
		case local:
			// No iptables on a local install to REDIRECT to it; main says so and skips it.
		case cfg.Listener.TransparentInboundAddr == "":
			// The bind used to refuse this; net.Listen would take "" as a random port.
			return nil, fmt.Errorf("transparent-inbound: transparent_inbound_addr is empty but inbound_interception is transparent")
		default:
			plan = append(plan, PlannedListener{Name: "transparent-inbound", Addr: cfg.Listener.TransparentInboundAddr})
		}
	}
	if roles[RoleForward] {
		plan = append(plan, PlannedListener{Name: "forward-proxy", Addr: cfg.Listener.ForwardProxyAddr})
		if !local && cfg.Listener.TransparentProxyAddr != "" {
			plan = append(plan, PlannedListener{Name: "transparent-proxy", Addr: cfg.Listener.TransparentProxyAddr})
		}
	}
	plan = append(plan, PlannedListener{Name: "stats", Addr: cfg.Stats.StatsAddress})
	if cfg.Session.SessionEnabled() && cfg.Listener.SessionAPIAddr != "" {
		plan = append(plan, PlannedListener{Name: "session-api", Addr: cfg.Listener.SessionAPIAddr})
	}
	plan = append(plan, PlannedListener{Name: "health", Addr: cfg.Listener.HealthAddr})
	return plan, nil
}
