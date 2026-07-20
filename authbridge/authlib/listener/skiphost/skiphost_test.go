package skiphost

import "testing"

func TestNew_EmptyList(t *testing.T) {
	m, err := New(nil)
	if err != nil {
		t.Fatalf("New(nil) err = %v", err)
	}
	if m.Match("any-host") {
		t.Error("empty matcher matched a host; should match nothing")
	}
}

func TestNew_RejectsEmptyPattern(t *testing.T) {
	if _, err := New([]string{""}); err == nil {
		t.Error("New([\"\"]) returned nil error; empty pattern must be rejected at boot")
	}
}

func TestNew_RejectsInvalidPattern(t *testing.T) {
	if _, err := New([]string{"["}); err == nil {
		t.Error("New([\"[\"]) returned nil error; malformed glob must surface at boot")
	}
}

func TestMatch_NilMatcher(t *testing.T) {
	var m *Matcher
	if m.Match("host") {
		t.Error("nil matcher matched; zero value must be safe and match nothing")
	}
}

func TestMatch_EmptyHost(t *testing.T) {
	// "*-anything" matches every single-label host that ends in
	// "-anything" — broad enough that the empty-host defense is the
	// only thing keeping Match from returning true on an unset Host.
	// We can't use bare "*" anymore (rejected by New as match-all).
	m, _ := New([]string{"some-host"})
	if m.Match("") {
		t.Error("matcher matched empty host; empty host must never match (defensive against unset Host header)")
	}
}

func TestNew_RejectsMatchAllStar(t *testing.T) {
	if _, err := New([]string{"*"}); err == nil {
		t.Error("New([\"*\"]) must reject match-all (every single-label hostname would skip enforcement)")
	}
}

func TestNew_RejectsMatchAllDoubleStar(t *testing.T) {
	if _, err := New([]string{"**"}); err == nil {
		t.Error("New([\"**\"]) must reject the unambiguous match-all glob")
	}
}

func TestNew_RejectsWhitespaceOnly(t *testing.T) {
	if _, err := New([]string{"   "}); err == nil {
		t.Error("New([\"   \"]) must reject whitespace-only patterns (trivial-true)")
	}
}

func TestNew_RejectsPortInPattern(t *testing.T) {
	if _, err := New([]string{"otel-collector:8335"}); err == nil {
		t.Error("New([\"otel-collector:8335\"]) must reject port-bearing patterns; Match strips ports before comparing so they would never match")
	}
}

func TestNew_AcceptsLeadingStar(t *testing.T) {
	// "*.something" is NOT match-all under .-delimited glob — it
	// requires the fixed suffix. Explicit guard against a future
	// rewrite that over-rejects and breaks operator-typical FQDN
	// patterns.
	cases := []string{
		"*.svc.cluster.local",
		"*.metrics.local",
		"otel-collector.*.svc.cluster.local",
		"otel-collector*",
	}
	for _, p := range cases {
		if _, err := New([]string{p}); err != nil {
			t.Errorf("New([%q]) returned err = %v; non-match-all pattern must be accepted", p, err)
		}
	}
}

func TestMatch_StripsPort(t *testing.T) {
	m, _ := New([]string{"otel-collector.rossoctl-system.svc.cluster.local"})
	if !m.Match("otel-collector.rossoctl-system.svc.cluster.local:8335") {
		t.Error("port-stripping failed: pattern without :port should match host with :port")
	}
}

func TestMatch_GlobSingleLabel(t *testing.T) {
	// `*` with `.` separator matches a single DNS label, not multi-label.
	m, _ := New([]string{"otel-collector*"})
	cases := []struct {
		host string
		want bool
	}{
		{"otel-collector", true},
		{"otel-collector-v2", true},
		{"otel-collector.rossoctl-system.svc.cluster.local", false}, // separator stops at .
		{"foo-otel-collector", false},
	}
	for _, tc := range cases {
		if got := m.Match(tc.host); got != tc.want {
			t.Errorf("Match(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

func TestMatch_GlobLeadingWildcard(t *testing.T) {
	// `*.svc.cluster.local` → single-label prefix on a fixed suffix.
	m, _ := New([]string{"*.rossoctl-system.svc.cluster.local"})
	if !m.Match("otel-collector.rossoctl-system.svc.cluster.local") {
		t.Error("leading-* should match a single-label prefix on the FQDN")
	}
	if m.Match("a.b.rossoctl-system.svc.cluster.local") {
		t.Error("leading-* must NOT match a two-label prefix (separator semantics)")
	}
}

func TestMatch_MultiplePatterns_FirstMatchWins(t *testing.T) {
	m, _ := New([]string{"never-matches", "otel-*", "another"})
	if !m.Match("otel-collector") {
		t.Error("matcher with multiple patterns should match if any pattern matches")
	}
}

func TestMatch_NoMatch(t *testing.T) {
	m, _ := New([]string{"otel-collector*", "*.metrics.local"})
	if m.Match("github-tool-mcp") {
		t.Error("Match returned true for an unrelated host")
	}
}
