package main

import (
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/rossoctl/cortex/core/config"
)

// Every socket is bound before anything on disk is opened, and that ordering is the point.
//
// The ports are what stop two proxies running one config, and a laptop's two proxies share
// ~/.cortex: the cost ledger and the session archive. Opening those is not read-only — the
// archive removes its trash and prunes on open, and is closed on the fatal path — so a
// second proxy that only met a taken port after opening them was writing files the running
// one was writing. That happened on every restart that outlived launchd's exit timeout:
// the old proxy was orphaned on its ports, and each new one replayed segments it had not
// finished (incompleteSegments=11) before dying on the bind. Bound first, a second proxy
// gives up before it has touched anything.
//
// It also turns the startup replay of the archive into a wait rather than a refusal: the
// kernel accepts connections on a bound socket nobody is serving yet, so a client that
// arrives during those seconds is answered late instead of refused.

// reservedListeners holds the bound sockets until the servers that serve them start.
type reservedListeners map[string]net.Listener

// reserveListeners binds every planned socket, or none: on a failure it closes what it
// bound and names the socket that could not be. The plan is config.ListenerPlan's; the
// failure is a *listenError, which keeps the same message and additionally carries the
// address, so the caller can classify a taken port and say who holds it — see
// portconflict.go.
func reserveListeners(plan []config.PlannedListener) (reservedListeners, error) {
	r := make(reservedListeners, len(plan))
	for _, p := range plan {
		ln, err := net.Listen("tcp", p.Addr)
		if err != nil {
			r.closeAll()
			return nil, &listenError{name: p.Name, addr: p.Addr, err: err}
		}
		r[p.Name] = ln
	}
	return r, nil
}

// take hands a reserved socket to the server that serves it. A name the plan did not
// reserve is a disagreement with the listener plan and fatal: binding it here instead would put
// it after the data again.
func (r reservedListeners) take(name string) net.Listener {
	ln, ok := r[name]
	if !ok {
		fatalf("internal: the %s listener was not reserved; the listener plan and main disagree", name)
	}
	delete(r, name)
	return ln
}

// takeTCP is take for the transparent listeners, which need the raw *net.TCPListener to
// recover SO_ORIGINAL_DST. A "tcp" net.Listen always returns one.
func (r reservedListeners) takeTCP(name string) *net.TCPListener {
	return r.take(name).(*net.TCPListener)
}

// claimed reports any socket reserved and never taken: bound, so connections to it are
// accepted, with nothing ever answering them.
func (r reservedListeners) claimed() error {
	if len(r) == 0 {
		return nil
	}
	left := make([]string, 0, len(r))
	for name := range r {
		left = append(left, name)
	}
	sort.Strings(left)
	return fmt.Errorf("internal: reserved but never served: %s; the listener plan and main disagree", strings.Join(left, ", "))
}

func (r reservedListeners) closeAll() {
	for name, ln := range r {
		_ = ln.Close()
		delete(r, name)
	}
}
