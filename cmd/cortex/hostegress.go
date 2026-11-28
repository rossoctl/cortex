//go:build include_driver_moca

package main

import (
	_ "embed"
	"fmt"
	"net"
	"strings"
)

//go:embed hostegress.yaml
var hostEgressConfig []byte

// builtinConfig is the config for a start with no --config: hostegress.yaml when
// MOCA_EGRESS_CONTROL_SOCK is set, which is how moca starts its egress proxy. As
// moca's proxy does, it needs MOCA_EGRESS_ALLOW, and it needs the host's own
// addresses, which the egress guard refuses as destinations.
func builtinConfig(getenv func(string) string, interfaceAddrs func() ([]net.Addr, error)) ([]byte, bool, error) {
	if strings.TrimSpace(getenv(envControlSock)) == "" {
		return nil, false, nil
	}
	if len(resolveEgressAllow(nil, getenv)) == 0 {
		return nil, true, fmt.Errorf("%s is required with %s and no --config", envEgressAllow, envControlSock)
	}
	if _, err := interfaceAddrs(); err != nil {
		return nil, true, fmt.Errorf("the egress guard cannot read this host's addresses: %w", err)
	}
	return hostEgressConfig, true, nil
}
