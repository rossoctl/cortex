package pricing

import (
	"path"
	"strings"
)

// hostKey strips the port from a host[:port] authority, so a pattern need not
// mention one. The wire carries host:port for any gateway on a non-default port,
// and requiring that in the pattern would unprice exactly those deployments.
//
// IPv6 literals keep their brackets ("[::1]:8080" -> "[::1]"), so a bracketed
// pattern still matches and the colons inside the address are not mistaken for a
// port separator.
func hostKey(host string) string {
	if host == "" {
		return ""
	}
	if strings.HasPrefix(host, "[") {
		if i := strings.LastIndex(host, "]"); i >= 0 {
			return host[:i+1]
		}
		return host
	}
	if i := strings.LastIndex(host, ":"); i >= 0 {
		return host[:i]
	}
	return host
}

// anyHost reports whether a host pattern matches every endpoint. Both spellings
// are accepted because "" is what an omitted YAML key produces and "*" is what an
// operator writes when they mean it.
func anyHost(pattern string) bool { return pattern == "" || pattern == "*" }

// hostRankLen is the length used when ranking host patterns, zero for any spelling
// of a catch-all.
//
// "" and "*" are documented as identical, but len("*") is 1 and len("") is 0, and
// host length is compared before the model axis — so a {"*", "*"} row outranked a
// {"", "claude-opus-5"} row, letting a catch-all shadow an exact model.
func hostRankLen(host string) int {
	if anyHost(host) {
		return 0
	}
	return len(host)
}

// matchHost reports whether endpoint matches pattern, with the port stripped.
//
// path.Match rather than the model matcher's gobwas/glob, deliberately: host
// globs are the idiom already established at plugins/sparc/collect.go:148-156,
// where "/" is the only separator so "*" spans dot-separated labels. Routing host
// patterns through the model matcher instead would silently change behaviour for
// anyone carrying a host glob across from a sparc config.
//
// An empty endpoint never matches a named pattern. A session event with no Host
// must not be priced by a host-scoped row, which would attribute untraceable
// traffic to whichever gateway happened to sort first.
//
// PRECONDITION: pattern is already lower-cased. NewTable does that once at build
// time so the request path does not re-do it per row. Only the endpoint is folded
// here, so passing a mixed-case pattern directly will not match.
func matchHost(pattern, endpoint string) bool {
	if anyHost(pattern) {
		return true
	}
	key := strings.ToLower(hostKey(endpoint))
	if key == "" {
		return false
	}
	// A bracketed IPv6 literal is compared literally, never as a glob. path.Match
	// reads "[" as the start of a character class, so "[::1]" would parse as "one
	// of ':' or '1'" — a pattern that matches single characters and never the
	// address it plainly names. Nobody writes a host character class; everybody
	// who writes brackets means an address.
	if isIPv6Literal(pattern) {
		return pattern == key
	}
	ok, err := path.Match(pattern, key)
	return err == nil && ok
}

// isIPv6Literal reports whether pattern is a bracketed IPv6 address rather than a
// glob: brackets around something containing a colon.
func isIPv6Literal(pattern string) bool {
	return strings.HasPrefix(pattern, "[") &&
		strings.HasSuffix(pattern, "]") &&
		strings.Contains(pattern, ":")
}

// validHostPattern reports whether pattern is a well-formed path.Match glob.
//
// Checked at table-build time because path.Match reports ErrBadPattern only when
// it is called: a malformed pattern would otherwise never match, report nothing,
// and silently unprice the endpoint it was written for.
func validHostPattern(pattern string) error {
	if isIPv6Literal(pattern) {
		return nil // compared literally, so path.Match never sees it
	}
	_, err := path.Match(pattern, "probe")
	return err
}

// EndpointKey normalizes a request's target host the way rate resolution normalizes it:
// lower-cased, port stripped.
//
// Exported so callers that key their own state on an endpoint — the drift reporter's
// dedup set, for one — agree with matchHost about what counts as the same endpoint.
// Keeping a private copy of this rule is how "GW.internal:443" and "gw.internal" came to
// occupy two entries for one gateway.
func EndpointKey(endpoint string) string {
	return strings.ToLower(hostKey(endpoint))
}
