//go:build unix

package main

import (
	"log/slog"
	"os"

	"github.com/rossoctl/cortex/core/listener/forwardproxy"
	"github.com/rossoctl/cortex/core/listener/vmcontrol"
)

// startVMControl brings up the VM control channel: the control socket vmpool
// hands per-VM egress listeners over, a Registry binding each to fp, and the
// trusted-session hook that makes which-socket-accepted-the-connection the
// session identity for everything a VM sends.
func startVMControl(cfg controlConfig, fp *forwardproxy.Server) vmControlServer {
	// The listener's own answer, never the request's: see TrustedSessionID.
	fp.TrustedSessionID = vmcontrol.SessionIDFromRequest
	// A TLS-bridged tunnel's decrypted requests are parsed fresh and bypass the
	// per-VM wrapper, so the binding has to be re-stamped onto each of them or
	// the guest's own session headers decide. See AdoptBridgedRequest.
	fp.AdoptBridgedRequest = vmcontrol.AdoptBridgedRequest
	reg := vmcontrol.NewRegistry(fp, vmcontrol.NewJSONSink(os.Stdout))
	srv, err := vmcontrol.NewServer(cfg.Socket, cfg.AllowedUIDs, reg)
	if err != nil {
		fatalf("vm control: %v", err)
	}
	go func() {
		if err := srv.Serve(); err != nil {
			fatalf("vm control: %v", err)
		}
	}()
	slog.Info("vm control listening — MOCA SBX per-VM egress sockets",
		"socket", cfg.Socket, "allowed_uids", cfg.AllowedUIDs)
	return srv
}
