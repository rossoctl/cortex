package httpx

import (
	"net"
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
