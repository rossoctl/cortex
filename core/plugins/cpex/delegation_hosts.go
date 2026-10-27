package cpex

import (
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"github.com/gobwas/glob"

	"github.com/rossoctl/cortex/core/internal/hostglob"
)

// codeDelegationDestinationMismatch is the violation code for a request
// whose delegate() step minted a credential its destination host is not
// bound to in delegated_credential_hosts.
const codeDelegationDestinationMismatch = "delegation_destination_mismatch"

// credentialHosts binds each delegation target to the hosts its
// credentials may be sent to: `.`-delimited globs, as in skip_hosts and
// authproxy-routes, matched against the host without its port, and IP
// addresses or CIDRs, matched against a host that is a canonical IP
// literal (see hostSet.matchExact).
type credentialHosts map[string]hostSet

// hostPatterns is the compiled strict_hosts list (see hostSet.matchAny).
type hostPatterns = hostSet

// hostSet is a compiled list of host patterns: `.`-delimited name globs,
// and IP prefixes. A host that reads as an address is matched against the
// prefixes only, never the globs, and a name against the globs only, so a
// glob cannot be satisfied by a numeric spelling the dialer reads as an
// address, and a name that resolves to a listed address is not taken for
// it.
type hostSet struct {
	names    []*glob.Pattern
	prefixes []netip.Prefix
}

// newHostPatterns compiles the strict_hosts patterns.
func newHostPatterns(name string, patterns []string) (hostPatterns, error) {
	return compileHostSet(name, patterns)
}

// compileHostSet compiles the patterns of the config key key. Every
// entry is classified as exactly one of three things, and anything that
// fits none is refused rather than kept as a pattern that never matches:
//
//   - a CIDR: it contains "/", and must be a valid prefix with no host
//     bits set ("10.96.0.0/12", "fd00:10:96::/112");
//   - an IP address: a canonical IP literal netip parses, an IPv6 one
//     optionally in brackets ("10.96.0.10", "fd00::a", "[fd00::a]");
//   - a name glob: letters, digits, "-", "_", "." and the wildcards "*"
//     and "?" only, with at least one label that is not a number or a
//     wildcard, so it can match only a name (see hostSet).
//
// Refused too: an empty entry, a port, a zone, and an entry matching every
// host or every address.
func compileHostSet(key string, patterns []string) (hostSet, error) {
	var out hostSet
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			return hostSet{}, fmt.Errorf("%s: empty pattern", key)
		}
		if prefix, ok, err := parseIPPattern(p); ok {
			if err != nil {
				return hostSet{}, fmt.Errorf("%s: invalid address or CIDR %q: %w", key, p, err)
			}
			if prefix.Bits() == 0 {
				return hostSet{}, fmt.Errorf("%s: pattern %q matches every address", key, p)
			}
			out.prefixes = append(out.prefixes, prefix)
			continue
		}
		if strings.Contains(p, ":") {
			return hostSet{}, fmt.Errorf("%s: pattern %q must not carry a port", key, p)
		}
		if err := nameGlobShape(p); err != nil {
			return hostSet{}, fmt.Errorf("%s: pattern %q %w", key, p, err)
		}
		g, err := hostglob.Compile(p)
		if err != nil {
			return hostSet{}, fmt.Errorf("%s: invalid pattern %q: %w", key, p, err)
		}
		if hostglob.MatchesEverySingleLabel(g) {
			return hostSet{}, fmt.Errorf("%s: pattern %q matches every host", key, p)
		}
		out.names = append(out.names, g)
	}
	return out, nil
}

// nameGlobShape refuses a name glob that uses characters other than
// [a-z0-9-_.*?], or whose every label is a number or a wildcard: the shape
// of an address ("10.96.*.*", "10.1", "*.*"), which name globs never match.
func nameGlobShape(p string) error {
	for _, c := range p {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.*?", c)) {
			return fmt.Errorf("may use only letters, digits, '-', '_', '.', '*' and '?'; list an IP address or CIDR for an address")
		}
	}
	unwild := strings.NewReplacer("*", "", "?", "")
	for _, label := range strings.Split(p, ".") {
		if bare := unwild.Replace(label); bare != "" && !numericLabel(bare) {
			return nil
		}
	}
	return fmt.Errorf("names no label a host name could have; list an IP address or CIDR for an address")
}

// numericLabel reports whether label is an inet_aton number part:
// decimal or octal digits, or 0x and hex digits.
func numericLabel(label string) bool {
	digits := label
	hex := strings.HasPrefix(label, "0x")
	if hex {
		digits = label[2:]
	}
	if digits == "" {
		return hex
	}
	for _, c := range digits {
		if !(c >= '0' && c <= '9' || hex && c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// parseIPPattern reads p as an IP address or CIDR. ok is false when p is
// neither and should be read as a host glob; err is set when p is meant as
// one (it contains "/", "%" or brackets) but is not a canonical address or
// a prefix without host bits. An IPv4-mapped IPv6 prefix is returned as
// the IPv4 prefix it covers, which is how a host is matched against it.
func parseIPPattern(p string) (netip.Prefix, bool, error) {
	if strings.Contains(p, "%") {
		return netip.Prefix{}, true, fmt.Errorf("must not carry a zone")
	}
	if strings.Contains(p, "/") {
		prefix, err := netip.ParsePrefix(p)
		if err != nil {
			return netip.Prefix{}, true, err
		}
		if prefix != prefix.Masked() {
			return netip.Prefix{}, true, fmt.Errorf("has host bits set (the network is %s)", prefix.Masked())
		}
		return unmapPrefix(prefix), true, nil
	}
	bracketed := strings.HasPrefix(p, "[") || strings.HasSuffix(p, "]")
	if bracketed {
		if len(p) < 2 || !strings.HasPrefix(p, "[") || !strings.HasSuffix(p, "]") {
			return netip.Prefix{}, true, fmt.Errorf("unbalanced brackets")
		}
		p = p[1 : len(p)-1]
	}
	addr, err := netip.ParseAddr(p)
	if err != nil {
		if bracketed {
			return netip.Prefix{}, true, err
		}
		return netip.Prefix{}, false, nil
	}
	if bracketed && !addr.Is6() {
		return netip.Prefix{}, true, fmt.Errorf("only an IPv6 address is written in brackets")
	}
	addr = addr.Unmap()
	return netip.PrefixFrom(addr, addr.BitLen()), true, nil
}

func unmapPrefix(p netip.Prefix) netip.Prefix {
	if p.Addr().Is4In6() && p.Bits() >= 96 {
		return netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
	}
	return p
}

// hostKind is how a request's host reads to the dialer.
type hostKind int

const (
	hostName      hostKind = iota // a name, resolved by DNS
	hostCanonical                 // an IP literal netip parses
	hostAton                      // a numeric spelling inet_aton reads as an IPv4 address
	hostOdd                       // none of these: not a name a resolver should accept, nor an address
)

// classifyHost removes the port, IPv6 brackets and a zone ("%eth0", or
// "%25eth0" as a URL spells it) from host and reads what remains. A
// trailing dot is kept for addresses: "10.96.0.10." does not parse as an
// address, so the dialer may look it up as a name, and it is classified
// as an inet_aton spelling instead. A host the matcher cannot read
// (characters outside a host name, unbalanced or misused brackets, an
// all-numeric name inet_aton refuses such as "10.96.0.256") is hostOdd.
func classifyHost(host string) (hostKind, netip.Addr, string) {
	if hh, _, err := net.SplitHostPort(host); err == nil {
		host = hh
	}
	host = strings.ToLower(host)
	bracketed := strings.HasPrefix(host, "[") || strings.HasSuffix(host, "]")
	if bracketed {
		if len(host) < 2 || !strings.HasPrefix(host, "[") || !strings.HasSuffix(host, "]") {
			return hostOdd, netip.Addr{}, host
		}
		host = host[1 : len(host)-1]
	}
	literal, zoned := host, false
	if i := strings.IndexByte(literal, '%'); i >= 0 {
		literal, zoned = literal[:i], true
	}
	if addr, err := netip.ParseAddr(literal); err == nil {
		switch {
		case bracketed && !addr.Is6():
			return hostOdd, netip.Addr{}, host
		case !zoned || addr.Is6():
			return hostCanonical, addr.Unmap(), host
		}
		return hostAton, addr, host
	}
	if bracketed {
		return hostOdd, netip.Addr{}, host
	}
	if addr, ok := parseInetAton(strings.TrimSuffix(literal, ".")); ok {
		return hostAton, addr, host
	}
	name := strings.TrimSuffix(host, ".")
	if zoned || !hostNameShape(name) {
		return hostOdd, netip.Addr{}, host
	}
	return hostName, netip.Addr{}, name
}

// hostNameShape reports whether name is made of non-empty labels of
// letters, digits, "-" and "_", with at least one label that is not a
// number: a name a resolver looks up rather than reads as an address.
func hostNameShape(name string) bool {
	if name == "" {
		return false
	}
	named := false
	for _, label := range strings.Split(name, ".") {
		if label == "" {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
		if !numericLabel(label) {
			named = true
		}
	}
	return named
}

// matchAny reports whether host may reach a listed destination: a name
// matching a glob, or any spelling of an address the dialer may connect
// to (an IP literal in any case, with leading zeros, IPv4-mapped, with a
// zone or a trailing dot, or a numeric inet_aton form such as
// "0x0a.0x60.0.1", "012.0140.0.1", "10.6291457" or "174063617") inside a
// listed prefix. A host it cannot read at all matches whenever the list is
// not empty, so an odd spelling fails closed. strict_hosts uses it: it
// widens a check, so every spelling that may arrive there must be caught.
// A name that DNS resolves to a listed address is not caught; list it by
// name.
func (h hostSet) matchAny(host string) bool {
	if host == "" {
		return false
	}
	switch kind, addr, name := classifyHost(host); kind {
	case hostName:
		return h.matchName(name)
	case hostOdd:
		return len(h.names) > 0 || len(h.prefixes) > 0
	default:
		return h.matchAddr(addr)
	}
}

// matchExact reports whether host is certainly a listed destination: a
// name matching a glob, or a canonical IP literal inside a listed prefix.
// An inet_aton spelling or a host it cannot read matches nothing, since
// what it reaches depends on the resolver. delegated_credential_hosts uses
// it: it decides where a credential may go, so a spelling it cannot be
// sure of withholds it.
func (h hostSet) matchExact(host string) bool {
	if host == "" {
		return false
	}
	switch kind, addr, name := classifyHost(host); kind {
	case hostName:
		return h.matchName(name)
	case hostCanonical:
		return h.matchAddr(addr)
	}
	return false
}

// match is matchAny, the strict_hosts rule.
func (h hostSet) match(host string) bool { return h.matchAny(host) }

func (h hostSet) matchName(name string) bool {
	for _, g := range h.names {
		if g.Match(name) {
			return true
		}
	}
	return false
}

func (h hostSet) matchAddr(addr netip.Addr) bool {
	for _, p := range h.prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// parseInetAton reads the IPv4 spellings inet_aton accepts: one to four
// parts, each decimal, octal (leading 0) or hex (0x), the last filling
// the remaining bytes.
func parseInetAton(s string) (netip.Addr, bool) {
	if s == "" {
		return netip.Addr{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) > 4 {
		return netip.Addr{}, false
	}
	vals := make([]uint64, len(parts))
	for i, part := range parts {
		if !numericLabel(part) {
			return netip.Addr{}, false
		}
		base, digits := 10, part
		switch {
		case strings.HasPrefix(part, "0x"):
			base, digits = 16, part[2:]
		case len(part) > 1 && part[0] == '0':
			base, digits = 8, part[1:]
		}
		if digits == "" {
			digits = "0"
		}
		v, err := strconv.ParseUint(digits, base, 32)
		if err != nil {
			return netip.Addr{}, false
		}
		vals[i] = v
	}
	var n uint64
	for i, v := range vals[:len(vals)-1] {
		if v > 0xff {
			return netip.Addr{}, false
		}
		n |= v << (8 * (3 - i))
	}
	last := vals[len(vals)-1]
	if last >= 1<<(8*(5-len(vals))) {
		return netip.Addr{}, false
	}
	n |= last
	return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}), true
}

// newCredentialHosts compiles delegated_credential_hosts. Every target
// needs at least one pattern; patterns are refused as compileHostSet
// refuses them.
func newCredentialHosts(m map[string][]string) (credentialHosts, error) {
	if len(m) == 0 {
		return nil, nil
	}
	targets := make([]string, 0, len(m))
	for t := range m {
		targets = append(targets, t)
	}
	sort.Strings(targets)
	out := make(credentialHosts, len(m))
	for _, target := range targets {
		patterns := m[target]
		if strings.TrimSpace(target) == "" {
			return nil, fmt.Errorf("delegated_credential_hosts: empty target")
		}
		if len(patterns) == 0 {
			return nil, fmt.Errorf("delegated_credential_hosts: target %q names no hosts", target)
		}
		set, err := compileHostSet(fmt.Sprintf("delegated_credential_hosts: target %q", target), patterns)
		if err != nil {
			return nil, err
		}
		out[target] = set
	}
	return out, nil
}

// allows reports whether a credential for target may be sent to host
// (see hostSet.matchExact).
func (h credentialHosts) allows(target, host string) bool {
	if target == "" || host == "" {
		return false
	}
	set, ok := h[target]
	return ok && set.matchExact(host)
}

// credentialTarget is the key a delegated credential is bound by: the
// target its delegate() step named, else its audience.
func credentialTarget(target, audience string) string {
	if target != "" {
		return target
	}
	return audience
}
