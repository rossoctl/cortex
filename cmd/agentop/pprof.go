package main

import (
	"fmt"
	"net"
	"net/http"
	"os"

	"github.com/rossoctl/cortex/core/listener/httpx/pprofx"
)

// pprofAddrEnv names the env var that serves Go's pprof endpoints from the
// viewer. Off unless set: the profiles expose the process's internals —
// allocation stack traces, argv, caller-started CPU/trace profiling — and the
// viewer's heap holds the session content it is displaying. Point it at a
// loopback address and read it with
// `go tool pprof http://127.0.0.1:47603/debug/pprof/heap`.
const pprofAddrEnv = "AGENTOP_PPROF_ADDR"

// startPprof serves the profile mux on addr and returns the address it landed
// on, with a :0 port resolved to the real one. The returned shutdown closes the
// listener; process exit closes it too, so a caller that forgets the shutdown
// loses nothing but a port.
//
// Loopback only, checked on the bound address rather than the string: the Host
// and Origin guards stop browsers, not a curl that sets Host: localhost, so a
// bind on every interface (":47603", "0.0.0.0:47603") would hand the heap to
// anyone on the network. #1603 gates its pprof the same way, on the stats
// port's bind_loopback_only.
func startPprof(addr string) (bound string, shutdown func(), err error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", nil, err
	}
	if tcp, ok := ln.Addr().(*net.TCPAddr); !ok || !tcp.IP.IsLoopback() {
		ln.Close()
		return "", nil, fmt.Errorf("%s did not land on a loopback address; pprof is served on loopback only", addr)
	}
	// Handler is set, never nil: net/http/pprof's init also registers
	// /debug/pprof/* on http.DefaultServeMux, which a nil Handler would serve
	// ungated.
	srv := &http.Server{Handler: pprofMux()}
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String(), func() { _ = srv.Close() }, nil
}

// pprofMux is the viewer's /debug/pprof surface: pprofx.RegisterPprof's guarded
// handlers, which refuse any non-loopback Host and any request carrying
// Origin — the browser defenses shared with the proxy's stats server (#1603)
// and the session API's erase guards.
func pprofMux() *http.ServeMux {
	mux := http.NewServeMux()
	pprofx.RegisterPprof(mux)
	return mux
}

// maybeServePprof starts the pprof listener when pprofAddrEnv is set, printing
// where it landed (or why it did not) on stderr, before the TUI takes the alt
// screen and stderr stops reaching the user. A refusal to bind is not fatal:
// the viewer's job does not depend on being profiled. The caller owns the
// returned shutdown and defers it — deferring here would stop the listener the
// moment this helper returns.
func maybeServePprof() (shutdown func()) {
	addr := os.Getenv(pprofAddrEnv)
	if addr == "" {
		return nil
	}
	bound, stop, err := startPprof(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentop: not serving pprof on %s: %v\n", addr, err)
		return nil
	}
	fmt.Fprintf(os.Stderr, "agentop: serving pprof on http://%s/debug/pprof/\n", bound)
	return stop
}
