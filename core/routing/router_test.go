package routing

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolve_ExactMatch(t *testing.T) {
	r, err := NewRouter("passthrough", []Route{
		{Host: "auth-target-service", Audience: "auth-target", Scopes: "openid"},
	})
	if err != nil {
		t.Fatal(err)
	}

	resolved := r.Resolve("auth-target-service")
	if resolved == nil {
		t.Fatal("expected match")
	}
	if !resolved.Matched {
		t.Error("expected Matched=true for explicit route match")
	}
	if resolved.Audience != "auth-target" {
		t.Errorf("audience = %q, want %q", resolved.Audience, "auth-target")
	}
	if resolved.Scopes != "openid" {
		t.Errorf("scopes = %q, want %q", resolved.Scopes, "openid")
	}
}

func TestResolve_GlobMatch(t *testing.T) {
	r, _ := NewRouter("passthrough", []Route{
		{Host: "*.example.com", Audience: "example"},
	})

	if resolved := r.Resolve("api.example.com"); resolved == nil || resolved.Audience != "example" {
		t.Error("expected glob match for api.example.com")
	}
	// Single-level glob should NOT match nested
	if resolved := r.Resolve("api.sub.example.com"); resolved != nil {
		t.Error("single glob should not match nested subdomain")
	}
}

func TestResolve_PortStripping(t *testing.T) {
	r, _ := NewRouter("passthrough", []Route{
		{Host: "service", Audience: "svc"},
	})
	if resolved := r.Resolve("service:8081"); resolved == nil || resolved.Audience != "svc" {
		t.Error("expected match after port stripping")
	}
}

func TestResolve_FirstMatchWins(t *testing.T) {
	// Two patterns that genuinely overlap without either being dead:
	// "svc-*" covers svc-prod and svc-dev, "*-prod" covers svc-prod and
	// api-prod, and only svc-prod hits both. Neither is unreachable, so
	// NewRouter accepts the pair.
	//
	// This used to configure the host "service" twice, which is dead config
	// — the second route can never fire — and NewRouter now rejects it. The
	// property under test is unchanged; only the fixture is honest about
	// being reachable.
	r, err := NewRouter("passthrough", []Route{
		{Host: "svc-*", Audience: "first"},
		{Host: "*-prod", Audience: "second"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved := r.Resolve("svc-prod")
	if resolved == nil || resolved.Audience != "first" {
		t.Error("expected first-match-wins for a host both patterns match")
	}
	if resolved := r.Resolve("api-prod"); resolved == nil || resolved.Audience != "second" {
		t.Error("second route must stay reachable for hosts the first does not match")
	}
}

func TestNewRouter_RejectsDuplicateHost(t *testing.T) {
	_, err := NewRouter("passthrough", []Route{
		{Host: "service", Audience: "first"},
		{Host: "service", Audience: "second"},
	})
	if err == nil {
		t.Error("expected error: the second route repeats a host and can never fire")
	}
}

// TestNewRouter_RejectsShadowedRoute covers the hazard the unreachable-route
// check exists for: resolution is first-match-wins, so a broad early pattern
// silently swallows everything configured beneath it. A "***" typo or a bare
// "*" at the top of authproxy-routes disables every route below — and if the
// shadowing route is a passthrough, token exchange is off for hosts the
// operator explicitly listed.
func TestNewRouter_RejectsShadowedRoute(t *testing.T) {
	for _, tc := range []struct {
		shadowing string
		shadowed  string
	}{
		// Total match-all: nothing after it is reachable, FQDN or not.
		{"**", "api.example.com"},
		{"***", "api.example.com"},
		{"{**}", "api.example.com"},
		{"{*,**}", "api.example.com"},
		// A bare "*" is confined to one label, so it shadows a short
		// in-cluster service name but NOT an FQDN — see
		// TestNewRouter_AcceptsSingleStarBeforeFQDN.
		{"*", "github-tool-mcp"},
		// An earlier pattern that simply covers the later literal.
		{"api.example.com", "api.example.com"},
		{"*.example.com", "api.example.com"},
	} {
		_, err := NewRouter("passthrough", []Route{
			{Host: tc.shadowing, Action: "passthrough"},
			{Host: tc.shadowed, Audience: "shadowed"},
		})
		if err == nil {
			t.Errorf("expected error: leading route %q makes the %q route unreachable",
				tc.shadowing, tc.shadowed)
		}
	}
}

// TestNewRouter_AcceptsSingleStarBeforeFQDN pins the precision of the check.
// With '.' as the separator a bare "*" matches one label only, so an FQDN
// route after it is genuinely still reachable and must not be rejected —
// over-rejection here would fail the pod at boot.
func TestNewRouter_AcceptsSingleStarBeforeFQDN(t *testing.T) {
	r, err := NewRouter("passthrough", []Route{
		{Host: "*", Audience: "single-label"},
		{Host: "api.example.com", Audience: "fqdn"},
	})
	if err != nil {
		t.Fatalf("single-star before an FQDN route must be accepted, got err = %v", err)
	}
	if resolved := r.Resolve("api.example.com"); resolved == nil || resolved.Audience != "fqdn" {
		t.Error("FQDN route must stay reachable behind a single-label wildcard")
	}
}

// TestNewRouter_AcceptsTrailingCatchAll is the deliberate difference from
// skip_hosts, which rejects match-all outright. Here a match-all as the last
// route is a legitimate explicit catch-all — equivalent to defaultAction —
// because nothing follows it to shadow.
func TestNewRouter_AcceptsTrailingCatchAll(t *testing.T) {
	r, err := NewRouter("passthrough", []Route{
		{Host: "api.example.com", Audience: "specific"},
		{Host: "**", Audience: "catch-all"},
	})
	if err != nil {
		t.Fatalf("trailing catch-all must be accepted, got err = %v", err)
	}
	if resolved := r.Resolve("api.example.com"); resolved == nil || resolved.Audience != "specific" {
		t.Error("specific route must win over the trailing catch-all")
	}
	if resolved := r.Resolve("anything-else"); resolved == nil || resolved.Audience != "catch-all" {
		t.Error("trailing catch-all must match everything the specific route does not")
	}
}

func TestNewRouter_RejectsEmptyHost(t *testing.T) {
	_, err := NewRouter("passthrough", []Route{
		{Host: "", Audience: "nowhere"},
	})
	if err == nil {
		t.Error("expected error: an empty host pattern matches only an empty Host header, never real traffic")
	}
}

// TestResolve_EmptyHost pins the empty-host defence. A bare "*" matches the
// empty string under gobwas/glob, so without the guard an unset Host header
// would select this route and mint a token for a destination we cannot
// identify.
func TestResolve_EmptyHost(t *testing.T) {
	r, err := NewRouter("passthrough", []Route{
		{Host: "*-anything", Audience: "should-not-be-used"},
		{Host: "*", Audience: "should-not-be-used-either"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved := r.Resolve(""); resolved != nil {
		t.Errorf("empty host must take the no-route path, got %+v", resolved)
	}
}

func TestResolve_NoMatch_Passthrough(t *testing.T) {
	r, _ := NewRouter("passthrough", []Route{
		{Host: "known-service", Audience: "known"},
	})
	if resolved := r.Resolve("unknown-service"); resolved != nil {
		t.Error("expected nil for unmatched host with passthrough default")
	}
}

func TestResolve_NoMatch_Exchange(t *testing.T) {
	r, _ := NewRouter("exchange", []Route{})
	resolved := r.Resolve("any-service")
	if resolved == nil {
		t.Fatal("expected non-nil for exchange default")
	}
	if resolved.Matched {
		t.Error("expected Matched=false for default action fallback")
	}
	if resolved.Passthrough {
		t.Error("expected passthrough=false for exchange default")
	}
}

func TestResolve_PassthroughAction(t *testing.T) {
	r, _ := NewRouter("passthrough", []Route{
		{Host: "internal-svc", Action: "passthrough"},
	})
	resolved := r.Resolve("internal-svc")
	if resolved == nil || !resolved.Passthrough {
		t.Error("expected passthrough=true for passthrough action")
	}
}

func TestNewRouter_InvalidPattern(t *testing.T) {
	_, err := NewRouter("passthrough", []Route{
		{Host: "[invalid"},
	})
	if err == nil {
		t.Error("expected error for invalid glob pattern")
	}
}

func TestLoadRoutes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "routes.yaml")
	content := `- host: "auth-target"
  target_audience: "auth-target"
  token_scopes: "openid"
- host: "internal"
  action: "passthrough"
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	routes, err := LoadRoutes(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(routes) != 2 {
		t.Fatalf("got %d routes, want 2", len(routes))
	}
	if routes[0].Audience != "auth-target" {
		t.Errorf("route[0].Audience = %q, want %q", routes[0].Audience, "auth-target")
	}
}

func TestLoadRoutes_FileNotFound(t *testing.T) {
	routes, err := LoadRoutes("/nonexistent/routes.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if routes != nil {
		t.Error("expected nil routes for missing file")
	}
}
