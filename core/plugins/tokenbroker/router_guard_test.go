package tokenbroker

import "testing"

// newBrokerRouter rejects configuration that cannot mean what it says, so an
// operator mistake surfaces at boot instead of as traffic quietly skipping the
// broker. Mirrors the guards in core/routing.

func TestNewBrokerRouter_RejectsEmptyHost(t *testing.T) {
	if _, err := newBrokerRouter("passthrough", []tokenBrokerRoute{
		{Host: "", Action: "broker"},
	}); err == nil {
		t.Error("expected error: an empty host pattern matches only an empty Host header, never real traffic")
	}
}

// TestNewBrokerRouter_RejectsShadowedRoute covers the first-match-wins
// hazard. A broad early pattern swallows every route beneath it, and if that
// early route is a passthrough then brokering is silently off for hosts the
// operator explicitly listed.
func TestNewBrokerRouter_RejectsShadowedRoute(t *testing.T) {
	for _, tc := range []struct {
		shadowing string
		shadowed  string
	}{
		{"**", "api.example.com"},
		{"***", "api.example.com"},
		{"{**}", "api.example.com"},
		{"{*,**}", "api.example.com"},
		{"*", "internal-svc"}, // "*" covers a short service name, not an FQDN
		{"api.example.com", "api.example.com"},
		{"*.example.com", "api.example.com"},
	} {
		_, err := newBrokerRouter("passthrough", []tokenBrokerRoute{
			{Host: tc.shadowing, Action: "passthrough"},
			{Host: tc.shadowed, Action: "broker"},
		})
		if err == nil {
			t.Errorf("expected error: leading route %q makes the %q route unreachable",
				tc.shadowing, tc.shadowed)
		}
	}
}

// TestNewBrokerRouter_AcceptsReachableRoutes is the over-rejection guard: a
// wrongly rejected route list fails the pod at boot, so patterns that are
// genuinely all reachable must be accepted — including a match-all in the
// final position, where nothing follows it to shadow.
func TestNewBrokerRouter_AcceptsReachableRoutes(t *testing.T) {
	for _, rules := range [][]tokenBrokerRoute{
		{{Host: "api.example.com", Action: "broker"}, {Host: "other.example.com", Action: "passthrough"}},
		{{Host: "*", Action: "broker"}, {Host: "api.example.com", Action: "broker"}},
		{{Host: "svc-*", Action: "broker"}, {Host: "*-prod", Action: "passthrough"}},
		{{Host: "api.example.com", Action: "broker"}, {Host: "**", Action: "passthrough"}},
		{{Host: "*.metrics.local", Action: "passthrough"}, {Host: "*.svc.cluster.local", Action: "broker"}},
	} {
		if _, err := newBrokerRouter("passthrough", rules); err != nil {
			t.Errorf("newBrokerRouter(%+v) returned err = %v; all routes are reachable", rules, err)
		}
	}
}

// TestBrokerRouter_ResolveEmptyHost pins the empty-host defence. A bare "*"
// matches the empty string under gobwas/glob, so without the guard an unset
// Host header would select this route and broker a token for a destination we
// cannot identify.
func TestBrokerRouter_ResolveEmptyHost(t *testing.T) {
	r, err := newBrokerRouter("passthrough", []tokenBrokerRoute{
		{Host: "*", Action: "broker", TokenEndpoint: "https://should-not-be-used"},
	})
	if err != nil {
		t.Fatal(err)
	}
	shouldBroker, authEndpoint, tokenEndpoint := r.resolve("")
	if shouldBroker {
		t.Error("empty host must not broker; it must fall through to defaultAction")
	}
	if authEndpoint != "" || tokenEndpoint != "" {
		t.Errorf("empty host must return no endpoints, got auth=%q token=%q", authEndpoint, tokenEndpoint)
	}
}

// TestBrokerRouter_ResolveEmptyHost_DefaultBroker confirms the empty host
// takes the defaultAction path rather than being hardcoded to passthrough.
func TestBrokerRouter_ResolveEmptyHost_DefaultBroker(t *testing.T) {
	r, err := newBrokerRouter("broker", []tokenBrokerRoute{
		{Host: "api.example.com", Action: "passthrough"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if shouldBroker, _, _ := r.resolve(""); !shouldBroker {
		t.Error("empty host must follow defaultAction=broker, not a hardcoded passthrough")
	}
}
