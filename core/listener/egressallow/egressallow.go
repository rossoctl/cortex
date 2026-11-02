// Package egressallow implements the forward proxy's deny-by-default
// destination allow list (#1483, MOCA MI1 §8.5): a sandbox may reach only
// the host:port entries the operator lists, and a request for anything
// else is refused by name — never left to time out — before DNS
// resolution and before any dial.
//
// Matching is exact, host:port against host:port. That is deliberate and
// narrower than every other host list in this tree: skip_hosts and
// authproxy-routes take globs because they route, but an allowlist is a
// security boundary, and a wildcard in it is one typo away from "allow
// everything" — so the shapes that would widen the boundary are rejected
// at construction instead of interpreted.
//
// Host normalization mirrors forwardproxy.splitAuthority so a request and
// an entry written in different but equivalent spellings agree: hosts
// compare lowercased without a trailing dot, IPv6 literals by their
// unmapped value, and a destination that names no port defaults to the
// scheme's (80 for http, 443 for everything else, CONNECT included).
//
// The package depends on nothing inside core, so the listener and the
// binary that arms it can import it without risking an import cycle, and
// tests can exercise the matcher in isolation from the listener machinery.
package egressallow

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// Matcher answers "is this destination on the allow list?". A nil Matcher
// allows everything — the feature off, today's behavior — so callers gate
// on the nil check rather than on a configured-but-empty distinction.
type Matcher struct {
	entries map[string]struct{}
}

// New builds a Matcher from raw "host:port" entries. Construction-time
// guards reject the entries that would silently widen the boundary:
//
//   - empty / whitespace-only entries: no intent expressed.
//   - portless entries: the contract is host:port, and guessing a port
//     (or matching any) would widen the boundary the list exists to hold.
//     A portless entry is almost certainly a typo, so it is a startup
//     error, mirroring skiphost's rejection of colon-bearing patterns —
//     the same "would never do what the operator meant" shape.
//   - non-numeric or out-of-range ports, and an empty host.
//   - "*" / "**" hosts: exact match means these could only ever match a
//     literal host of that name, which is not what anyone means by them.
//   - unbracketed IPv6 literals ("2001:db8::1"): the last colon reads as
//     the port separator, leaving an entry no destination can match. That
//     empties the boundary rather than widening it, but it is just as
//     wrong, so it is an error naming "[<address>]:<port>".
//
// Hosts are normalized (see the package comment), so "Example.COM:443"
// and "example.com:443" are one entry. An empty input is valid and yields
// a Matcher that allows nothing — but note cmd/cortex only builds a
// Matcher when the resolved entry list is non-empty, so an empty config
// keeps the feature off rather than refusing all egress.
func New(entries []string) (*Matcher, error) {
	m := &Matcher{entries: make(map[string]struct{}, len(entries))}
	for _, e := range entries {
		key, err := normalizeEntry(e)
		if err != nil {
			return nil, err
		}
		m.entries[key] = struct{}{}
	}
	return m, nil
}

// normalizeEntry validates one raw entry and returns its match key.
func normalizeEntry(e string) (string, error) {
	trimmed := strings.TrimSpace(e)
	if trimmed == "" {
		return "", fmt.Errorf("egressallow: empty entry in the allow list")
	}
	host, port, err := splitHostPort(trimmed)
	if err != nil {
		return "", fmt.Errorf("egressallow: entry %q: %w", e, err)
	}
	if port == 0 {
		return "", fmt.Errorf("egressallow: entry %q names no port; the contract is host:port, "+
			"and a guessed port would widen the boundary the list exists to hold", e)
	}
	if host == "" {
		return "", fmt.Errorf("egressallow: entry %q has no host", e)
	}
	if host == "*" || host == "**" {
		return "", fmt.Errorf("egressallow: entry %q is a wildcard; the allow list is exact-match "+
			"by design — list each host:port the sandbox may reach", e)
	}
	// A colon left in the host of an entry that was NOT bracketed means an
	// unbracketed IPv6 literal: "2001:db8::1" splits at its last colon into
	// host "2001:db8:" and port "1" — a valid-looking entry that no
	// destination can ever match, because a request carries a v6 literal
	// bracketed and that normalizes to "2001:db8::1". The operator would
	// believe they allowed a host and have allowed nothing. It fails closed,
	// but an entry that silently empties the boundary is a startup error
	// here just as a portless or wildcard one is — named, with the spelling
	// that works. The bracketed form is excluded because its host keeps its
	// colons legitimately: "[::1]:443" → host "::1", port 443.
	if !strings.HasPrefix(trimmed, "[") && strings.Contains(host, ":") {
		return "", fmt.Errorf("egressallow: entry %q looks like an unbracketed IPv6 literal; "+
			"write it as [<address>]:<port> — unbracketed, the last colon is read as the "+
			"port separator and the entry can never match", e)
	}
	return host + ":" + strconv.Itoa(port), nil
}

// Allows reports whether dest (a "host:port" dial target, or a bare host
// whose port defaults to the scheme's) is on the list. A nil Matcher
// allows everything; a Matcher built from entries allows only what was
// listed — the deny-by-default contract. scheme defaults the port when
// dest names none: "http" means 80, anything else 443 (CONNECT tunnels
// are TLS, so 443 is the honest default for "tcp").
//
// A dest that cannot be parsed (empty host, bad port) is NOT allowed:
// deny-by-default means an unparseable destination is refused, not
// guessed at.
func (m *Matcher) Allows(dest, scheme string) bool {
	if m == nil {
		return true
	}
	host, port, err := splitHostPort(strings.TrimSpace(dest))
	if err != nil || host == "" {
		return false
	}
	// A destination that names no port is read at its scheme's — the
	// same default splitAuthority gives a Host header.
	if port == 0 {
		port = defaultPort(scheme)
	}
	_, ok := m.entries[host+":"+strconv.Itoa(port)]
	return ok
}

// defaultPort is the port a destination without one is read as.
func defaultPort(scheme string) int {
	if strings.EqualFold(scheme, "http") {
		return 80
	}
	return 443
}

// splitHostPort splits "host:port" into a normalized host and a numeric
// port. The port may be absent (port == 0, no error) — the caller decides
// what that means. IPv6 literals carry their brackets, which
// net.SplitHostPort understands but a bare strings.Split would not.
func splitHostPort(s string) (host string, port int, err error) {
	host, p, serr := splitPort(s)
	if serr != nil {
		return "", 0, serr
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if ip, perr := netip.ParseAddr(host); perr == nil {
		host = ip.Unmap().String()
	}
	if p == "" {
		return host, 0, nil
	}
	n, aerr := strconv.Atoi(p)
	if aerr != nil || n < 1 || n > 65535 {
		return "", 0, fmt.Errorf("port %q is not a number 1-65535", p)
	}
	return host, n, nil
}

// splitPort separates the port from the host, tolerating bracketed IPv6
// literals ("[::1]:443" → "::1", "443"; "[::1]" → "::1", "") and returning
// the host WITHOUT its brackets, so netip.ParseAddr in the caller can
// canonicalize it.
//
// The bracketed branch is taken only on a LEADING "[", never on a stray
// "]" anywhere in the string. The destination here is client-chosen — a
// CONNECT authority or an absolute-form URL host — so "]:443" arrives from
// the wire, and reading it as a bracketed literal would index s[1:0] and
// panic. A parser on a trust boundary does not get to panic: a stray "]"
// is simply not host:port, and falls through to be denied like any other
// unparseable destination.
func splitPort(s string) (host, port string, err error) {
	// A leading bracket means the host part is a bracketed IPv6 literal;
	// everything after its closing colon is the port.
	if strings.HasPrefix(s, "[") {
		i := strings.LastIndex(s, "]")
		if i < 0 {
			return "", "", fmt.Errorf("%q is not host:port", s)
		}
		host = s[1:i] // strip the brackets
		rest := s[i+1:]
		if rest == "" {
			return host, "", nil
		}
		if !strings.HasPrefix(rest, ":") {
			return "", "", fmt.Errorf("%q is not host:port", s)
		}
		return host, rest[1:], nil
	}
	if strings.Contains(s, "]") {
		return "", "", fmt.Errorf("%q is not host:port", s)
	}
	if i := strings.LastIndex(s, ":"); i >= 0 {
		return s[:i], s[i+1:], nil
	}
	return s, "", nil
}
