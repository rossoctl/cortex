package hostglob

import "testing"

func mustCompile(t *testing.T, pattern string) interface{ Match(string) bool } {
	t.Helper()
	g, err := Compile(pattern)
	if err != nil {
		t.Fatalf("Compile(%q) err = %v", pattern, err)
	}
	return g
}

// TestMatchesEverySingleLabel_BroadSpellings covers what a spelling check
// misses. skiphost's guard used to compare against the literal strings "*"
// and "**"; every other way of writing the same breadth slipped through and
// silently bypassed the pipeline for every host.
func TestMatchesEverySingleLabel_BroadSpellings(t *testing.T) {
	for _, p := range []string{
		"*",      // every single label
		"**",     // match-all
		"***",    // runs of three or more stars behave as "**"
		"****",   //
		"{**}",   // super-star in braces
		"{*,**}", // alternation containing a super-star
		"?*",     // one character then anything
	} {
		if !MatchesEverySingleLabel(mustCompile(t, p)) {
			t.Errorf("MatchesEverySingleLabel(%q) = false, want true", p)
		}
	}
}

// TestMatchesEverySingleLabel_NarrowPatterns is the over-rejection guard.
// Every pattern here needs a separator or a fixed prefix/suffix, so none
// covers all in-cluster destinations and all must keep working — a wrongly
// rejected pattern fails the pod at boot.
func TestMatchesEverySingleLabel_NarrowPatterns(t *testing.T) {
	for _, p := range []string{
		"*.*",
		"*.svc.cluster.local",
		"service-*",
		"otel-*",
		"otel-collector*",
		"*-anything",
		"otel-collector.*.svc.cluster.local",
		"**.svc.cluster.local",
		"?",
		"**.**",
		"api.anthropic.com",
	} {
		if MatchesEverySingleLabel(mustCompile(t, p)) {
			t.Errorf("MatchesEverySingleLabel(%q) = true, want false", p)
		}
	}
}

// TestIsMatchAll_DistinguishesSingleLabelFromTotal pins the difference
// between the two predicates. A bare "*" covers every single label but no
// FQDN, so it is broad enough to reject in skip_hosts yet does not make a
// later FQDN route unreachable.
func TestIsMatchAll_DistinguishesSingleLabelFromTotal(t *testing.T) {
	star := mustCompile(t, "*")
	if !MatchesEverySingleLabel(star) {
		t.Error(`MatchesEverySingleLabel("*") = false, want true`)
	}
	if IsMatchAll(star) {
		t.Error(`IsMatchAll("*") = true, want false ("*" matches no FQDN)`)
	}

	for _, p := range []string{"**", "***", "{**}", "{*,**}"} {
		if !IsMatchAll(mustCompile(t, p)) {
			t.Errorf("IsMatchAll(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"*.*", "*.svc.cluster.local", "service-*", "**.svc.cluster.local"} {
		if IsMatchAll(mustCompile(t, p)) {
			t.Errorf("IsMatchAll(%q) = true, want false", p)
		}
	}
}

func TestIsLiteral(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		want    bool
	}{
		{"api.example.com", true},
		{"github-tool-mcp", true},
		{"", true},
		{"*", false},
		{"*.example.com", false},
		{"?", false},
		{"{a,b}", false},
		{"[a-z]", false},
		{`a\*b`, false},
	} {
		if got := IsLiteral(tc.pattern); got != tc.want {
			t.Errorf("IsLiteral(%q) = %t, want %t", tc.pattern, got, tc.want)
		}
	}
}

// TestShadows covers the first-match-wins hazard: an earlier entry that makes
// a later one unreachable is dead configuration, and both route lists reject
// it at boot rather than silently ignoring the later entry.
func TestShadows(t *testing.T) {
	for _, tc := range []struct {
		earlier string
		later   string
		want    bool
		why     string
	}{
		// A total match-all shadows anything.
		{"**", "api.example.com", true, "match-all shadows every later route"},
		{"***", "*.example.com", true, "match-all shadows later wildcards too"},
		{"{*,**}", "github-tool-mcp", true, "brace alternation match-all"},

		// Exact coverage of a later literal.
		{"api.example.com", "api.example.com", true, "duplicate host"},
		{"*.example.com", "api.example.com", true, "wildcard covers the later literal"},
		{"*", "github-tool-mcp", true, "single-label wildcard covers a short service name"},

		// Not shadowed: "*" is confined to one label.
		{"*", "api.example.com", false, `"*" matches no FQDN`},
		{"*.example.com", "api.sub.example.com", false, "single label does not span two"},

		// Not shadowed: unrelated patterns.
		{"otel-*", "github-tool-mcp", false, "prefix does not match"},
		{"*.metrics.local", "api.example.com", false, "different suffix"},

		// Deliberately not reported: later is a wildcard and earlier is not
		// match-all. Deciding glob subsumption in general is out of scope, and
		// a false positive would fail the pod at boot.
		{"*.example.com", "api.example.com*", false, "sound, not complete"},
	} {
		g := mustCompile(t, tc.earlier)
		if got := Shadows(g, tc.later); got != tc.want {
			t.Errorf("Shadows(%q, %q) = %t, want %t (%s)", tc.earlier, tc.later, got, tc.want, tc.why)
		}
	}
}

// TestCompile_Separator pins the separator every consumer must share. If this
// drifts, identical patterns would mean different things in skiphost, routing
// and tokenbroker.
func TestCompile_Separator(t *testing.T) {
	if Separator != '.' {
		t.Fatalf("Separator = %q, want '.'", Separator)
	}
	g := mustCompile(t, "*.example.com")
	if !g.Match("api.example.com") {
		t.Error("one label before the suffix must match")
	}
	if g.Match("api.sub.example.com") {
		t.Error("* must not cross a separator")
	}
}
