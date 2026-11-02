// Package pprofx mounts Go's net/http/pprof handlers behind the browser
// defenses. It is a leaf of core/listener/httpx so that pprof's import side
// effect reaches only the programs that mount the surface: net/http/pprof's
// init registers /debug/pprof/* on http.DefaultServeMux, so living in httpx
// itself would hand that registration to every listener helper's consumer —
// forwardproxy, reverseproxy, extproc, sessionapi — that never serves pprof.
package pprofx

import (
	"net/http"
	"net/http/pprof"

	"github.com/rossoctl/cortex/core/listener/httpx"
)

// RegisterPprof mounts Go's net/http/pprof handlers under /debug/pprof/ on mux,
// each behind the browser defenses: a request whose Host is not exactly a
// loopback name (httpx.LoopbackHost — this defeats DNS rebinding) or that
// carries an Origin header (a browser always sends one; a no-cors GET needs no
// preflight) is answered 403. The profiles expose the process's internals —
// allocation stack traces, argv, and a caller's ability to start CPU/trace
// profiling — so callers mount them only on loopback-bound servers: the stats
// server behind listener.bind_loopback_only, agentop behind
// AGENTOP_PPROF_ADDR.
//
// Side effect of the import: net/http/pprof's init also registers
// /debug/pprof/* on http.DefaultServeMux in every binary linking this
// package. No program linking this package serves that mux — an http.Server
// built with a nil Handler would, and would expose these ungated, so none
// may be. (Programs elsewhere in this tree do serve a nil Handler, but they
// link no core package, so this import never reaches them.)
//
// ONE definition for the guarded surface (#1608): the stats server (#1603)
// and the agentop viewer each carried their own copy of the guard and the five
// registrations.
func RegisterPprof(mux *http.ServeMux) {
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !httpx.LoopbackHost(r.Host) {
				http.Error(w, "pprof is served only for a loopback Host", http.StatusForbidden)
				return
			}
			if r.Header.Get("Origin") != "" {
				http.Error(w, "pprof is refused from a browser: the request carries an Origin header", http.StatusForbidden)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("/debug/pprof/", guard(pprof.Index))
	mux.HandleFunc("/debug/pprof/cmdline", guard(pprof.Cmdline))
	mux.HandleFunc("/debug/pprof/profile", guard(pprof.Profile))
	mux.HandleFunc("/debug/pprof/symbol", guard(pprof.Symbol))
	mux.HandleFunc("/debug/pprof/trace", guard(pprof.Trace))
}
