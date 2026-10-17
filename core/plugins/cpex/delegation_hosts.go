package cpex

import (
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/gobwas/glob"

	"github.com/rossoctl/cortex/core/internal/hostglob"
)

// codeDelegationDestinationMismatch is the violation code for a request
// whose delegate() step minted a credential its destination host is not
// bound to in delegated_credential_hosts.
const codeDelegationDestinationMismatch = "delegation_destination_mismatch"

// credentialHosts binds each delegation target to the hosts its
// credentials may be sent to. Patterns are `.`-delimited globs, as in
// skip_hosts and authproxy-routes, matched against the host without its
// port.
type credentialHosts map[string][]*glob.Pattern

// newCredentialHosts compiles delegated_credential_hosts. Every target
// needs at least one pattern; a pattern that matches every in-cluster
// host, or carries a port, is refused.
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
		for _, p := range patterns {
			p = strings.ToLower(strings.TrimSpace(p))
			switch {
			case p == "":
				return nil, fmt.Errorf("delegated_credential_hosts: target %q: empty pattern", target)
			case strings.Contains(p, ":"):
				return nil, fmt.Errorf("delegated_credential_hosts: target %q: pattern %q must not carry a port", target, p)
			}
			g, err := hostglob.Compile(p)
			if err != nil {
				return nil, fmt.Errorf("delegated_credential_hosts: target %q: invalid pattern %q: %w", target, p, err)
			}
			if hostglob.MatchesEverySingleLabel(g) {
				return nil, fmt.Errorf("delegated_credential_hosts: target %q: pattern %q matches every host", target, p)
			}
			out[target] = append(out[target], g)
		}
	}
	return out, nil
}

// allows reports whether a credential for target may be sent to host.
func (h credentialHosts) allows(target, host string) bool {
	if target == "" || host == "" {
		return false
	}
	if hh, _, err := net.SplitHostPort(host); err == nil {
		host = hh
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, g := range h[target] {
		if g.Match(host) {
			return true
		}
	}
	return false
}

// credentialTarget is the key a delegated credential is bound by: the
// target its delegate() step named, else its audience.
func credentialTarget(target, audience string) string {
	if target != "" {
		return target
	}
	return audience
}
