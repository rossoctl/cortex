//go:build !unix

package main

import (
	"github.com/rossoctl/cortex/core/listener/forwardproxy"
)

// startVMControl refuses: the per-VM socket handover needs SCM_RIGHTS and peer
// credentials, so a configured channel on a non-Unix platform fails closed
// rather than running silently without it.
func startVMControl(cfg controlConfig, fp *forwardproxy.Server) vmControlServer {
	fatalf("vm control: %s: per-VM socket handover requires a Unix platform (SCM_RIGHTS)", envControlSock)
	return nil
}
