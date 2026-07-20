// Package skiphost matches an outbound destination Host against an
// operator-configured pattern list to decide whether the listener should
// short-circuit — bypassing the plugin pipeline and session recording —
// and forward the request as a transparent proxy.
//
// Matcher semantics intentionally mirror authlib/routing: gobwas/glob with
// `.` as the separator so "*.svc.cluster.local" matches a single label and
// "service-*" matches anything starting with "service-". Port is stripped
// before matching so operators write patterns against the hostname alone
// regardless of which port the upstream listens on.
//
// The package is a leaf — no dependencies inside authlib — so both
// listener implementations (extproc, forwardproxy) can import it without
// risking an import cycle, and tests can exercise the matcher in
// isolation from the listener machinery.
package skiphost

import (
	"fmt"
	"net"
	"strings"

	"github.com/gobwas/glob"
)

// Matcher answers "does this host match any configured skip pattern?".
// A nil Matcher matches nothing (zero value is safe to call).
type Matcher struct {
	patterns []compiled
}

type compiled struct {
	raw  string
	glob glob.Glob
}

// New compiles a skip-host matcher from raw glob patterns. Returns an
// error identifying the first invalid pattern so misconfigurations
// surface at startup rather than at first request. An empty input is
// valid and yields a Matcher that matches nothing.
//
// Construction-time guards reject footgun patterns that would silently
// disable the entire outbound enforcement pipeline (skip_hosts bypasses
// plugins AND session recording for matched traffic):
//
//   - empty / whitespace-only patterns: trivially-true matches with no
//     intent expressed.
//   - "*" — under our `.`-delimited glob semantics, matches every
//     single-label hostname, which is how every short Kubernetes
//     service name reaches the listener (`Host: github-tool-mcp`,
//     `Host: otel-collector`, etc.). One wildcard would silently
//     exempt every in-cluster outbound from IBAC + token-exchange.
//   - "**" — the unambiguous match-all under gobwas/glob.
//   - patterns containing ":" — Match strips the port from the
//     incoming host before comparing, so colon-bearing patterns
//     compile but never match. Almost certainly an operator typo.
//
// Mirrors the bypass-pattern guard added to ibac in #496. Operators
// that mean to disable enforcement should remove the relevant plugin
// from the pipeline rather than wildcarding it away here.
//
// Patterns like "*.*", "*.svc.cluster.local", or "service-*" are NOT
// match-all under `.`-delimited glob (they require a fixed label
// count or fixed suffix) and are accepted normally.
func New(patterns []string) (*Matcher, error) {
	if len(patterns) == 0 {
		return &Matcher{}, nil
	}
	out := make([]compiled, 0, len(patterns))
	for _, p := range patterns {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			return nil, fmt.Errorf("skiphost: empty pattern in skip_hosts list")
		}
		if trimmed == "*" || trimmed == "**" {
			return nil, fmt.Errorf("skiphost: pattern %q matches everything; "+
				"if you mean to disable outbound enforcement, remove the "+
				"relevant plugins from the pipeline instead", p)
		}
		if strings.Contains(p, ":") {
			return nil, fmt.Errorf("skiphost: pattern %q must not contain a port "+
				"(Match strips the port from the incoming host before comparing, "+
				"so a port-bearing pattern would never match)", p)
		}
		g, err := glob.Compile(p, '.')
		if err != nil {
			return nil, fmt.Errorf("skiphost: invalid pattern %q: %w", p, err)
		}
		out = append(out, compiled{raw: p, glob: g})
	}
	return &Matcher{patterns: out}, nil
}

// Match reports whether host matches any configured pattern. Strips
// the port (everything from the first colon) before comparing so
// operators can write "otel-collector.rossoctl-system.svc.cluster.local"
// without worrying which port the upstream listens on. Returns false
// for the nil Matcher and for the empty host.
//
// Wraps MatchPattern so callers that only want the boolean don't pay
// for the unused string allocation in their hot path.
func (m *Matcher) Match(host string) bool {
	_, matched := m.MatchPattern(host)
	return matched
}

// MatchPattern reports whether host matches any configured pattern,
// and if it does, returns the raw operator-supplied pattern that
// matched. The matched pattern is used by listeners to attribute
// audit logs / counters — "this host was skipped because pattern P
// matched" — so an operator reviewing logs can correlate skips back
// to entries in their skip_hosts list. First-match-wins; iteration
// order is the order patterns were configured.
//
// Returns ("", false) for the nil Matcher, the empty pattern list,
// the empty host, and unmatched hosts.
func (m *Matcher) MatchPattern(host string) (string, bool) {
	if m == nil || len(m.patterns) == 0 || host == "" {
		return "", false
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	for _, p := range m.patterns {
		if p.glob.Match(host) {
			return p.raw, true
		}
	}
	return "", false
}
