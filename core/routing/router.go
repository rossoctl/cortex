// Package routing provides host-to-audience routing for token exchange.
// Routes map destination hosts (with glob patterns) to token exchange parameters.
package routing

import (
	"fmt"
	"net"
	"strings"

	"github.com/gobwas/glob"

	"github.com/rossoctl/cortex/core/internal/hostglob"
)

// Route defines token exchange parameters for requests to a matching host.
type Route struct {
	Host          string `yaml:"host"`
	Audience      string `yaml:"target_audience,omitempty"`
	Scopes        string `yaml:"token_scopes,omitempty"`
	TokenEndpoint string `yaml:"token_url,omitempty"`
	Action        string `yaml:"action,omitempty"` // "exchange" or "passthrough"; defaults to "exchange"
}

// ResolvedRoute is the result of resolving a host against the router.
type ResolvedRoute struct {
	Matched       bool // true if a configured route matched; false for default action fallback
	Audience      string
	Scopes        string
	TokenEndpoint string
	Passthrough   bool
}

type compiledRoute struct {
	pattern string
	glob    glob.Glob
	route   Route
}

// Router resolves destination hosts to token exchange configuration.
// Uses first-match-wins semantics with gobwas/glob patterns.
type Router struct {
	routes        []compiledRoute
	defaultAction string // "exchange" or "passthrough"
}

// NewRouter creates a router from the given routes.
// defaultAction is "exchange" or "passthrough" (applied when no route matches).
//
// Rejects configuration that cannot do what it appears to say, so the
// mistake surfaces at boot instead of as traffic quietly taking the wrong
// path:
//
//   - an empty or whitespace-only host pattern. A route whose `host:` key is
//     missing from routes.yaml compiles into a pattern that matches only the
//     empty Host header, i.e. never matches real traffic.
//   - a route made unreachable by an earlier one. Resolution is
//     first-match-wins, so a broad early pattern — a "***" typo, or a plain
//     "*", which matches every short in-cluster service name — swallows every
//     route beneath it. If the shadowing route is a passthrough, token
//     exchange is silently off for hosts the operator explicitly listed.
//
// A match-all pattern is NOT rejected outright the way skip_hosts rejects
// one: as the last route it is a legitimate catch-all, equivalent to
// defaultAction but stated explicitly. Only shadowing is an error, which
// means this check can never reject a config that was working — a shadowed
// route was already dead.
func NewRouter(defaultAction string, rules []Route) (*Router, error) {
	if defaultAction == "" {
		defaultAction = "passthrough"
	}
	compiled := make([]compiledRoute, 0, len(rules))
	for i, r := range rules {
		if strings.TrimSpace(r.Host) == "" {
			return nil, fmt.Errorf("route %d has an empty host pattern; "+
				"it would match only an empty Host header, never real traffic", i)
		}
		g, err := hostglob.Compile(r.Host)
		if err != nil {
			return nil, fmt.Errorf("invalid route pattern %q: %w", r.Host, err)
		}
		compiled = append(compiled, compiledRoute{
			pattern: r.Host,
			glob:    g,
			route:   r,
		})
	}
	for j := range compiled {
		for i := 0; i < j; i++ {
			if hostglob.Shadows(compiled[i].glob, compiled[j].pattern) {
				return nil, fmt.Errorf("route %d (%q) is unreachable: earlier route %d (%q) "+
					"already matches every host it would match, and resolution is "+
					"first-match-wins; reorder them or narrow the earlier pattern",
					j, compiled[j].pattern, i, compiled[i].pattern)
			}
		}
	}
	return &Router{routes: compiled, defaultAction: defaultAction}, nil
}

// Resolve returns the exchange configuration for the given host.
// Returns nil if no route matches and the default action is "passthrough".
// Port is stripped from the host before matching.
//
// An empty host takes the no-route path rather than being offered to the
// patterns. A bare "*" matches the empty string under gobwas/glob, so an
// unset Host header would otherwise select a "*" route and mint a token for
// a destination we cannot identify. Falling through to defaultAction is the
// safer reading of an unidentifiable request, and mirrors the empty-host
// defence in listener/skiphost.
func (r *Router) Resolve(host string) *ResolvedRoute {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "" {
		if r.defaultAction == "exchange" {
			return &ResolvedRoute{Matched: false}
		}
		return nil
	}
	for _, entry := range r.routes {
		if entry.glob.Match(host) {
			action := entry.route.Action
			if action == "" {
				action = "exchange"
			}
			return &ResolvedRoute{
				Matched:       true,
				Audience:      entry.route.Audience,
				Scopes:        entry.route.Scopes,
				TokenEndpoint: entry.route.TokenEndpoint,
				Passthrough:   action == "passthrough",
			}
		}
	}
	if r.defaultAction == "exchange" {
		return &ResolvedRoute{Matched: false}
	}
	return nil
}
