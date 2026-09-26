// Package hostglob is the one place cortex turns an operator-supplied host
// pattern into a matcher, and the one place that decides what counts as a
// dangerously broad pattern.
//
// Three packages match a destination Host against operator patterns:
//
//   - listener/skiphost      — a match bypasses the pipeline AND session recording
//   - routing                — a match selects token-exchange parameters
//   - plugins/tokenbroker    — a match selects broker parameters
//
// Each of them used to call glob.Compile(pattern, '.') itself and reason about
// breadth on its own. That duplication is why skiphost's footgun guard could
// compare against the literal strings "*" and "**" and miss "***", "{**}",
// "{*,**}" and "?*", and why the other two had no breadth check at all. One
// definition here means a fix or an upgrade lands once.
//
// The package deliberately depends on nothing inside core, so every
// consumer — listener, routing, plugins — can import it without any risk of
// an import cycle.
package hostglob

import (
	"strings"

	"github.com/gobwas/glob"
)

// Separator is the glob separator for host patterns. With '.', a single "*"
// is confined to one DNS label ("*.svc.cluster.local" matches
// "otel.svc.cluster.local" but not "otel.ns.svc.cluster.local") while "**"
// crosses labels. Every consumer must compile with the same separator or
// identical patterns would mean different things in different packages.
const Separator = '.'

// Compile compiles an operator-supplied host pattern.
func Compile(pattern string) (glob.Glob, error) {
	return glob.Compile(pattern, Separator)
}

// singleLabelProbes are short single-label hostnames of varying length. This
// is the shape in-cluster traffic actually arrives with: the Host header of a
// request to a Kubernetes Service is usually the bare service name
// ("Host: github-tool-mcp", "Host: otel-collector"), not an FQDN.
//
// A pattern matching every one of these covers every in-cluster destination,
// which is why bare "*" is dangerous in skip_hosts even though it matches no
// FQDN at all.
var singleLabelProbes = []string{
	"a",
	"svc",
	"otel-collector",
	"github-tool-mcp",
}

// fqdnProbes are multi-label hostnames: in-cluster FQDNs and a public domain.
// A pattern matching these as well as every singleLabelProbe is match-all in
// the strongest sense — no host escapes it.
var fqdnProbes = []string{
	"a.b",
	"otel-collector.rossoctl-system.svc.cluster.local",
	"api.anthropic.com",
}

// MatchesEverySingleLabel reports whether g matches every single-label probe,
// i.e. whether the pattern exempts (or captures) every in-cluster
// short-service-name destination.
//
// This is a behavioural probe rather than a comparison against known-bad
// strings on purpose. Under '.'-separated globs all of these are equally
// broad, and a spelling check catches only the ones someone thought of:
//
//	"*"                       — every single label
//	"**", "***", and longer runs of stars
//	"{**}", "{*,**}"          — super-star in braces or alternation
//	"?*"                      — one character then anything
func MatchesEverySingleLabel(g glob.Glob) bool {
	return matchesAll(g, singleLabelProbes)
}

// IsMatchAll reports whether g matches every probe host, single-label and
// FQDN alike — the strongest form of "this pattern matches everything".
//
// Callers deciding whether an earlier entry makes a later one unreachable
// want this rather than MatchesEverySingleLabel: "*" matches every single
// label but no FQDN, so it shadows a later "github-tool-mcp" while leaving a
// later "*.svc.cluster.local" perfectly reachable.
func IsMatchAll(g glob.Glob) bool {
	return matchesAll(g, singleLabelProbes) && matchesAll(g, fqdnProbes)
}

func matchesAll(g glob.Glob, hosts []string) bool {
	for _, h := range hosts {
		if !g.Match(h) {
			return false
		}
	}
	return true
}

// metacharacters are the glob syntax characters. A pattern containing none of
// them matches exactly one host, which is what lets IsLiteral support exact
// shadowing detection.
const metacharacters = `*?[]{}\`

// IsLiteral reports whether pattern contains no glob syntax, i.e. it matches
// exactly itself and nothing else.
//
// This is what makes unreachable-route detection exact for the common case: a
// literal later route is shadowed by an earlier pattern if and only if that
// pattern matches the literal. For a later route that is itself a wildcard,
// no such exact test exists (it would require deciding glob subsumption), so
// callers fall back to IsMatchAll on the earlier entry.
func IsLiteral(pattern string) bool {
	return !strings.ContainsAny(pattern, metacharacters)
}

// Shadows reports whether an earlier entry in a first-match-wins list makes a
// later one unreachable, so callers can reject dead configuration at boot
// rather than silently ignoring it.
//
// Both route lists in core are first-match-wins, which makes a broad
// early pattern quietly destructive: a "***" typo at the top of
// authproxy-routes, or a plain "*", swallows every carefully-configured route
// beneath it, and if the shadowing route is a passthrough then token exchange
// is silently off for hosts the operator explicitly listed.
//
// Deliberately conservative — it answers yes only when unreachability is
// certain:
//
//   - earlier is match-all, so nothing after it can ever be reached; or
//   - later is a literal host and earlier already matches that exact host.
//
// A later wildcard shadowed by a narrower earlier wildcard (say "*.a.com"
// after "*.*.com") is not reported. Deciding that in general means deciding
// glob subsumption, and a false positive here fails the pod at boot — so the
// check stays sound rather than complete.
func Shadows(earlier glob.Glob, laterPattern string) bool {
	if IsMatchAll(earlier) {
		return true
	}
	if IsLiteral(laterPattern) {
		return earlier.Match(laterPattern)
	}
	return false
}
