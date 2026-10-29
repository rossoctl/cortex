package main

import (
	"fmt"
	"strconv"
	"strings"
)

// The MOCA SBX VM control channel (#1477), configured env-only: the conformance
// harness drives it through MOCA_EGRESS_CONTROL_SOCK / _UIDS, and operator-facing
// YAML fields follow once there is an operator to configure them. Off unless
// MOCA_EGRESS_CONTROL_SOCK names a path, so a proxy with no MOCA around it keeps
// today's behavior byte for byte.

const (
	envTenancy     = "MOCA_TENANCY"
	envControlSock = "MOCA_EGRESS_CONTROL_SOCK"
	envControlUIDs = "MOCA_EGRESS_CONTROL_UIDS"
)

// controlConfig is the resolved VM control channel; a zero Socket means off.
type controlConfig struct {
	Socket      string
	AllowedUIDs []int
}

// vmControlServer is the subset of the control server main needs — closing on
// shutdown, which unregisters every VM and waits for their aborted audit lines.
// An interface so the non-unix stub can exist without the package.
type vmControlServer interface{ Close() error }

// resolveControlConfig reads the VM control channel's environment through
// getenv. Every error names its variable; an unset MOCA_EGRESS_CONTROL_SOCK
// turns the whole channel off without looking at anything else, so the
// tenancy check only fires where MOCA actually deployed this proxy.
func resolveControlConfig(getenv func(string) string) (controlConfig, error) {
	var cc controlConfig
	cc.Socket = strings.TrimSpace(getenv(envControlSock))
	if cc.Socket == "" {
		return cc, nil // feature off
	}
	// Tenancy is decided once, here, not per request: a Register carrying a
	// grant is refused by the registry with the same answer (MI1 S2).
	switch t := getenv(envTenancy); t {
	case "", "single":
	case "multi":
		return cc, fmt.Errorf("%s=multi: single-tenant only (MI1 S2)", envTenancy)
	default:
		return cc, fmt.Errorf("%s=%q: want unset or \"single\" (MI1 S2)", envTenancy, t)
	}
	// No default uid list: a control socket that admits nobody is a
	// misconfiguration, and one that admits everybody would be worse.
	raw := strings.TrimSpace(getenv(envControlUIDs))
	if raw == "" {
		return cc, fmt.Errorf("%s is required when %s is set", envControlUIDs, envControlSock)
	}
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		uid, err := strconv.Atoi(item)
		if err != nil || uid < 0 {
			return cc, fmt.Errorf("%s: %q is not a non-negative integer", envControlUIDs, item)
		}
		cc.AllowedUIDs = append(cc.AllowedUIDs, uid)
	}
	return cc, nil
}
