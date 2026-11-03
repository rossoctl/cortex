// Package hostmatch matches a request host against the bypass_hosts globs
// that ibac, sparc, cpex and lineage share.
package hostmatch

import (
	"net"
	"path"
	"strings"
)

// MatchesAny reports whether host matches any glob in patterns.
//
// Matching is path.Match against the host with its port stripped, so
// "keycloak.*" matches "keycloak.foo:8081" and a bare "otel-collector"
// matches only that name. The port is split with net.SplitHostPort so an IPv6
// literal ([::1]:4317) survives, and matching is case-folded because an
// authority is case-insensitive (RFC 3986).
func MatchesAny(patterns []string, host string) bool {
	if host == "" {
		return false
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(host)
	for _, p := range patterns {
		if matched, _ := path.Match(strings.ToLower(p), host); matched {
			return true
		}
	}
	return false
}
