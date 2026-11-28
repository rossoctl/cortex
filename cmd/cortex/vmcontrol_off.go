//go:build !include_driver_moca

package main

import (
	"log/slog"
	"net"

	"github.com/rossoctl/cortex/core/listener/forwardproxy"
)

// A build without the moca driver has no VM control channel and ignores its
// settings, MOCA_EGRESS_CONTROL_* among them. These stand in for what main.go
// calls.

// controlConfig is always zero here, so the channel is always off.
type controlConfig struct{ Socket string }

type vmControlServer interface{ Close() error }

// resolveControlConfig warns when the socket is set, which names the wrong
// binary on a MOCA host.
func resolveControlConfig(getenv func(string) string) (controlConfig, error) {
	if getenv("MOCA_EGRESS_CONTROL_SOCK") != "" {
		slog.Warn("vm control: MOCA_EGRESS_CONTROL_SOCK is set but this build has no moca driver (build the hostegress profile); ignoring it")
	}
	return controlConfig{}, nil
}

// startVMControl is unreachable: main.go calls it only for a non-empty Socket.
func startVMControl(controlConfig, *forwardproxy.Server) vmControlServer { return nil }

func checkVMControlStarted(controlConfig, vmControlServer) {}

// builtinConfig has no config to offer, so --config stays required.
func builtinConfig(func(string) string, func() ([]net.Addr, error)) ([]byte, bool, error) {
	return nil, false, nil
}
