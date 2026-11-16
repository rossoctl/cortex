// Package bypass provides path pattern matching for skipping JWT validation
// on public endpoints (e.g., health probes, agent card discovery).
package bypass

import (
	"fmt"
	"path"
	"strings"
)

// DefaultPatterns are paths that skip inbound JWT validation by default.
var DefaultPatterns = []string{"/.well-known/*", "/healthz", "/readyz", "/livez", "/metrics"}

// Matcher checks request paths against a set of bypass patterns.
// Patterns use Go's path.Match syntax (e.g., "/.well-known/*").
// Note: path.Match's "*" does NOT cross "/" separators, so "/.well-known/*"
// matches "/.well-known/agent.json" but NOT "/.well-known/foo/bar".
type Matcher struct {
	patterns []string
}

// NewMatcher creates a Matcher from the given patterns.
// Returns an error if any pattern has invalid path.Match syntax.
func NewMatcher(patterns []string) (*Matcher, error) {
	clean := make([]string, len(patterns))
	for i, p := range patterns {
		trimmed := strings.TrimSpace(p)
		if _, err := path.Match(trimmed, "/"); err != nil {
			return nil, fmt.Errorf("invalid bypass pattern %q: %w", p, err)
		}
		if trimmed == "" || trimmed == "*" || trimmed == "/*" {
			return nil, fmt.Errorf("bypass pattern %q is too broad; use specific path globs", p)
		}
		clean[i] = trimmed
	}
	return &Matcher{patterns: clean}, nil
}

// Match reports whether requestPath, a decoded path with no query, is a
// canonical path that matches a pattern as written. The app gets the path as
// sent and may route any other form elsewhere, so a dot segment, a doubled
// slash, or a ';', '\', '%' or '?' never matches, and a path with a trailing
// slash matches only a pattern that ends in one.
func (m *Matcher) Match(requestPath string) bool {
	clean := path.Clean(requestPath)
	trailingSlash := clean != "/" && clean+"/" == requestPath
	if (clean != requestPath && !trailingSlash) || strings.ContainsAny(requestPath, `;\%?`) {
		return false
	}
	for _, pattern := range m.patterns {
		if trailingSlash && !strings.HasSuffix(pattern, "/") {
			continue
		}
		if matched, _ := path.Match(pattern, requestPath); matched {
			return true
		}
	}
	return false
}
