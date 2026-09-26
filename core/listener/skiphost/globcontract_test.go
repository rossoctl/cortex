package skiphost

import (
	"testing"

	"github.com/gobwas/glob"
)

// Three packages compile operator-supplied host patterns the same way —
// glob.Compile(pattern, '.') then Match(host):
//
//   - listener/skiphost/skiphost.go       (bypasses the whole pipeline)
//   - routing/router.go                   (token-exchange routing)
//   - plugins/tokenbroker/plugin.go       (broker routing)
//
// None of them owns that behaviour; gobwas/glob does. skiphost.New's doc
// comment then makes load-bearing claims about it — that "*" matches every
// single-label host, that "**" is match-all, and that "*.*",
// "*.svc.cluster.local" and "service-*" are safe to accept precisely because
// they are NOT match-all. Those claims decide which patterns the boot-time
// guard rejects, and a wrong one means an operator pattern silently exempts
// traffic from enforcement.
//
// Nothing pinned those claims to the library, so a glob upgrade could quietly
// invalidate them. gobwas/glob v1.0.0 is described by its own release notes as
// "rewrite glob for a much simpler and _correct_ engine", which is exactly the
// kind of change that would. This file pins the contract directly against the
// library so the next bump has to prove it still holds.
//
// Deliberately not covered: the empty host. Match strips it before it reaches
// glob (MatchPattern returns early on host == ""), and it is the only input
// whose result differs between v0.2.3 and v1.0.0 — "?" matched "" in v0.2.3
// and does not in v1.0.0, and a run of three or more stars flipped the other
// way. Both need a degenerate pattern to reach, and skiphost never asks glob
// about an empty host, so pinning those cells would pin noise.

// TestGlobContract_SeparatorSemantics pins the single property every host
// pattern in the repo depends on: a lone "*" is confined to one dot-separated
// label, and "**" is not.
func TestGlobContract_SeparatorSemantics(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		host    string
		want    bool
		why     string
	}{
		// "*" stays within one label.
		{"*", "otel-collector", true, "single-label host is one label"},
		{"*", "otel-collector.svc", false, "* must not cross a separator"},
		{"*.example.com", "a.example.com", true, "one label before the suffix"},
		{"*.example.com", "a.b.example.com", false, "* must not span two labels"},
		{"*.example.com", "example.com", false, "* requires a label to be present"},
		{"otel-*", "otel-collector", true, "suffix wildcard within a label"},
		{"otel-*", "otel-collector.svc", false, "suffix wildcard stops at separator"},

		// "**" crosses separators.
		{"**", "otel-collector", true, "** matches a single label"},
		{"**", "a.b.c.d.e", true, "** crosses every separator"},
		{"**.svc.cluster.local", "a.b.svc.cluster.local", true, "** spans two labels"},

		// Fixed label count / fixed suffix keep these from being match-all.
		{"*.*", "a.b", true, "exactly two labels"},
		{"*.*", "a", false, "*.* requires a separator"},
		{"*.*", "a.b.c", false, "*.* is pinned to two labels"},
		{"service-*", "service-foo", true, "fixed prefix, one label"},
		{"service-*", "otel-collector", false, "fixed prefix must match"},
		{"service-*", "service-foo.bar", false, "fixed prefix does not cross separator"},
		{"*.svc.cluster.local", "otel.svc.cluster.local", true, "one label plus fixed suffix"},
		{"*.svc.cluster.local", "otel.ns.svc.cluster.local", false, "suffix pattern is label-exact"},
		{"otel-collector.*.svc.cluster.local", "otel-collector.ns.svc.cluster.local", true, "middle wildcard label"},
		{"otel-collector.*.svc.cluster.local", "otel-collector.a.b.svc.cluster.local", false, "middle wildcard is one label"},
	} {
		g, err := glob.Compile(tc.pattern, '.')
		if err != nil {
			t.Fatalf("glob.Compile(%q, '.') err = %v", tc.pattern, err)
		}
		if got := g.Match(tc.host); got != tc.want {
			t.Errorf("glob.Compile(%q, '.').Match(%q) = %t, want %t (%s)",
				tc.pattern, tc.host, got, tc.want, tc.why)
		}
	}
}

// TestGlobContract_GuardedPatternsAreMatchAll pins the premise behind New's
// rejection of "*" and "**": both would exempt the hosts the listener actually
// sees. "*" is rejected because every short in-cluster service name is a
// single label, which it matches.
func TestGlobContract_GuardedPatternsAreMatchAll(t *testing.T) {
	// Short Kubernetes service names — the case New's doc comment names.
	singleLabel := []string{"github-tool-mcp", "otel-collector", "x"}
	for _, host := range singleLabel {
		g, err := glob.Compile("*", '.')
		if err != nil {
			t.Fatalf(`glob.Compile("*", '.') err = %v`, err)
		}
		if !g.Match(host) {
			t.Errorf(`"*" no longer matches single-label host %q; New rejects "*" on the premise that it does`, host)
		}
	}

	// "**" must match everything, single-label and FQDN alike.
	g, err := glob.Compile("**", '.')
	if err != nil {
		t.Fatalf(`glob.Compile("**", '.') err = %v`, err)
	}
	for _, host := range append(singleLabel,
		"otel-collector.rossoctl-system.svc.cluster.local",
		"api.anthropic.com",
	) {
		if !g.Match(host) {
			t.Errorf(`"**" no longer matches %q; New rejects "**" as the unambiguous match-all`, host)
		}
	}
}

// TestGlobContract_AcceptedPatternsAreNotMatchAll is the converse guard: the
// three patterns New's doc comment explicitly accepts as safe must not match
// an unrelated host. If a future engine widened any of them to match-all, the
// guard would be waving through a full enforcement bypass.
func TestGlobContract_AcceptedPatternsAreNotMatchAll(t *testing.T) {
	unrelated := []string{
		"api.anthropic.com",
		"github-tool-mcp",
		"otel-collector.rossoctl-system.svc.cluster.local",
	}
	for _, pattern := range []string{"*.*", "*.svc.cluster.local", "service-*"} {
		g, err := glob.Compile(pattern, '.')
		if err != nil {
			t.Fatalf("glob.Compile(%q, '.') err = %v", pattern, err)
		}
		matched := 0
		for _, host := range unrelated {
			if g.Match(host) {
				matched++
			}
		}
		if matched == len(unrelated) {
			t.Errorf("pattern %q now matches every sample host; New accepts it on the premise that it is not match-all", pattern)
		}
	}
}
