package pricing

import (
	"strings"
	"testing"
)

func TestHostKey_StripsPort(t *testing.T) {
	for in, want := range map[string]string{
		"gw.internal":           "gw.internal",
		"gw.internal:4000":      "gw.internal",
		"api.anthropic.com:443": "api.anthropic.com",
		"[::1]:8080":            "[::1]",
		"[fd00::1]":             "[fd00::1]",
		"":                      "",
	} {
		if got := hostKey(in); got != want {
			t.Errorf("hostKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMatchHost_PortIsStripped(t *testing.T) {
	// An operator writes the host they know; the wire carries host:port. Requiring
	// the port in the pattern would unprice every gateway on a non-default port.
	if !matchHost("gw.internal", "gw.internal:4000") {
		t.Error("pattern without a port failed to match host:port")
	}
	if !matchHost("[::1]", "[::1]:8080") {
		t.Error("bracketed IPv6 pattern failed to match [host]:port")
	}
}

func TestMatchHost_Globs(t *testing.T) {
	for _, tc := range []struct {
		pattern, endpoint string
		want              bool
	}{
		{"*.internal", "gw.internal", true},
		{"*.internal", "gw-a.internal:4000", true},
		{"*.internal", "a.b.internal", true}, // path.Match: only "/" is a separator
		{"*.internal", "gw.external", false},
		{"gw-?.internal", "gw-a.internal", true},
		{"gw-?.internal", "gw-ab.internal", false},
		{"api.anthropic.com", "api.anthropic.com", true},
		{"api.anthropic.com", "api.openai.com", false},
	} {
		if got := matchHost(tc.pattern, tc.endpoint); got != tc.want {
			t.Errorf("matchHost(%q, %q) = %v, want %v", tc.pattern, tc.endpoint, got, tc.want)
		}
	}
}

func TestMatchHost_AnyPatternMatchesEverything(t *testing.T) {
	// How the bundled slice is scoped: a shipped table cannot know an operator's
	// gateway names, so it prices any endpoint until something more specific wins.
	for _, pattern := range []string{"", "*"} {
		for _, endpoint := range []string{"gw.internal", "api.anthropic.com:443", ""} {
			if !matchHost(pattern, endpoint) {
				t.Errorf("matchHost(%q, %q) = false, want true", pattern, endpoint)
			}
		}
	}
}

func TestMatchHost_EmptyEndpointDoesNotMatchNamedPattern(t *testing.T) {
	// An event with no Host must not be priced by a host-scoped row — that would
	// attribute untraceable traffic to whichever gateway sorted first.
	if matchHost("gw.internal", "") {
		t.Error("empty endpoint matched a named host pattern")
	}
	if matchHost("*.internal", "") {
		t.Error("empty endpoint matched a host glob")
	}
}

func TestResolve_LongerHostGlobWins(t *testing.T) {
	tab := mustTable(t,
		Entry{Host: "*.internal", Model: "*", Rates: rate(1.00), Prov: ProvConfigured},
		Entry{Host: "gw-a.internal", Model: "*", Rates: rate(2.00), Prov: ProvConfigured},
	)
	if got := inputPerMillion(first(tab.Resolve("gw-a.internal:4000", "m", 0))); got != 2.00 {
		t.Errorf("rate = %v, want 2.00 (exact host beats glob)", got)
	}
	if got := inputPerMillion(first(tab.Resolve("gw-b.internal", "m", 0))); got != 1.00 {
		t.Errorf("rate = %v, want 1.00 (host glob)", got)
	}
}

func TestNewTable_RejectsBadHostGlob(t *testing.T) {
	// A malformed path.Match pattern never matches and reports no error at match
	// time, so it would silently unprice the endpoint it was written for.
	_, err := NewTable([]Entry{{Host: "gw-[", Model: "*", Rates: rate(1), Prov: ProvConfigured}})
	if err == nil {
		t.Fatal("NewTable accepted a malformed host pattern")
	}
	if !strings.Contains(err.Error(), "host pattern") {
		t.Errorf("error %q does not mention the host pattern", err)
	}
}
