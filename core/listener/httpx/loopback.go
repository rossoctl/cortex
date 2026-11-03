package httpx

import (
	"net"
	"net/http"
	"strings"
)

// LoopbackHost reports whether hostport names exactly localhost, 127.0.0.1 or
// ::1, with or without a port. Not 127.0.0.0/8 as a whole and not anything
// ending in localhost — the point is to refuse a name an attacker controls,
// and a short list cannot be matched by one.
//
// ONE definition for the loopback-Host checks (#1608): the session API's erase
// guards, the stats server's pprof endpoints and the agentop viewer each
// carried their own copy, and a fix to one — closing a rebinding shape, adding
// an address form — reached the others only by re-editing every copy.
func LoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// LoopbackOnly answers 403 to a request whose Host is not a loopback name (this
// defeats DNS rebinding) or that carries an Origin header (a browser always sends
// one, and a no-cors GET needs no preflight), and passes every other request to h.
// It is the browser defense for a server that only this machine may read.
func LoopbackOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !LoopbackHost(r.Host) {
			http.Error(w, "served only for a loopback Host (localhost, 127.0.0.1 or [::1])", http.StatusForbidden)
			return
		}
		if r.Header.Get("Origin") != "" {
			http.Error(w, "refused from a browser: the request carries an Origin header", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}
