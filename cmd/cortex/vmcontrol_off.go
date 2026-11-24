//go:build !include_driver_moca

package main

import "github.com/rossoctl/cortex/core/listener/forwardproxy"

// A build without the moca driver has no VM control channel and ignores its
// settings, MOCA_EGRESS_CONTROL_* among them. These stand in for what main.go
// calls.

// controlConfig is always zero here, so the channel is always off.
type controlConfig struct{ Socket string }

type vmControlServer interface{ Close() error }

// resolveControlConfig reads no environment.
func resolveControlConfig(func(string) string) (controlConfig, error) {
	return controlConfig{}, nil
}

// startVMControl is unreachable: main.go calls it only for a non-empty Socket.
func startVMControl(controlConfig, *forwardproxy.Server) vmControlServer { return nil }

func checkVMControlStarted(controlConfig, vmControlServer) {}
