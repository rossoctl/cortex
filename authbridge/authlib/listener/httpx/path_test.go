package httpx

import (
	"strings"
	"testing"
)

// TestPathOnly pins the query-stripping invariant pipeline.Context.Path
// promises. PathOnly is the single chokepoint upholding it for every
// Envoy-fed listener (four ext_proc sites plus ext_authz), and that path now
// reaches operators as SessionEvent.HTTPPath — so a regression here would put
// query strings on the timeline and into anything exported from it.
func TestPathOnly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
		want   string
	}{
		{"plain path", "/v1/models", "/v1/models"},
		{"strips query", "/v1/models?stream=true", "/v1/models"},
		{"strips credential-bearing query", "/v1/models?access_token=shhh", "/v1/models"},
		{"strips bare question mark", "/v1/models?", "/v1/models"},
		{"strips multi-param query", "/a/b?x=1&y=2", "/a/b"},
		{"absolute-form target", "http://api.example.com/v1/models?k=v", "/v1/models"},
		{"root", "/", "/"},
		{"percent-decodes, matching net/http", "/a%2Fb", "/a/b"},
		// Fragments are NOT split: url.ParseRequestURI treats a request
		// target as having no fragment, because a client never sends one.
		// Pinned as-is rather than "fixed" — net/http gives the proxy
		// listeners the same answer, and cross-mode parity is the point.
		{"fragment stays in the path", "/v1/models#frag", "/v1/models#frag"},
		{"empty target", "", ""},

		// Unparseable targets take the fallback branch: a plain query strip
		// rather than a 400, because the Envoy-fed listeners have already
		// accepted the request by the time PathOnly runs. Documented in
		// PathOnly's own comment; asserted here so the fallback cannot
		// silently start returning the query too.
		{"unparseable keeps path, drops query", "not a uri?token=shhh", "not a uri"},
		{"unparseable without query is unchanged", "not a uri", "not a uri"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PathOnly(tc.target); got != tc.want {
				t.Errorf("PathOnly(%q) = %q, want %q", tc.target, got, tc.want)
			}
		})
	}
}

// TestPathOnlyNeverReturnsAQuery is the property the invariant actually rests
// on, stated independently of the table above: whatever the input shape, a
// "?" must never survive into pctx.Path. Cheaper to reason about than
// enumerating every malformed target someone might send.
func TestPathOnlyNeverReturnsAQuery(t *testing.T) {
	for _, target := range []string{
		"/v1/models?access_token=shhh",
		"/v1/models?",
		"//host/path?a=b",
		"http://example.com/p?a=b",
		"not a uri?token=shhh",
		"/weird%zz?token=shhh",
		"*?a=b",
	} {
		if got := PathOnly(target); strings.Contains(got, "?") {
			t.Errorf("PathOnly(%q) = %q, which still carries a query string", target, got)
		}
	}
}
