package model

import "testing"

func routedInv(plugin string, action InvocationAction, reason string, phase InvocationPhase, server string) Invocation {
	return Invocation{Plugin: plugin, Action: action, Reason: reason, Phase: phase, Details: map[string]string{"server": server}}
}

// Only the router's request-phase modify/routed names the server. Every other record that
// carries a server detail does not.
func TestRoutedServer(t *testing.T) {
	const r = "inference-router"
	for name, tc := range map[string]struct {
		in   *Invocations
		want string
	}{
		"nil":                   {nil, ""},
		"routed":                {&Invocations{Outbound: []Invocation{routedInv(r, ActionModify, "routed", InvocationPhaseRequest, "team-b")}}, "team-b"},
		"would_route observe":   {&Invocations{Outbound: []Invocation{routedInv(r, ActionObserve, "would_route", InvocationPhaseRequest, "team-a")}}, ""},
		"pinned_server_removed": {&Invocations{Outbound: []Invocation{routedInv(r, ActionDeny, "pinned_server_removed", InvocationPhaseRequest, "team-a")}}, ""},
		"another plugin":        {&Invocations{Outbound: []Invocation{routedInv("other", ActionModify, "routed", InvocationPhaseRequest, "team-a")}}, ""},
		"response phase":        {&Invocations{Outbound: []Invocation{routedInv(r, ActionModify, "routed", InvocationPhaseResponse, "team-a")}}, ""},
		"inbound":               {&Invocations{Inbound: []Invocation{routedInv(r, ActionModify, "routed", InvocationPhaseRequest, "team-a")}}, ""},
		"last wins": {&Invocations{Outbound: []Invocation{
			routedInv(r, ActionModify, "routed", InvocationPhaseRequest, "a"),
			routedInv(r, ActionModify, "routed", InvocationPhaseRequest, "b")}}, "b"},
	} {
		if got := RoutedServer(tc.in); got != tc.want {
			t.Errorf("%s: RoutedServer = %q, want %q", name, got, tc.want)
		}
	}
}

func TestLastNamedServer(t *testing.T) {
	both := func(s string) bool { return s == "team-a" || s == "team-b" }
	onlyA := func(s string) bool { return s == "team-a" }
	shared := func(string) (string, bool) { return "", true }
	table := InferenceHosts{
		{Agent: "a", Host: "gw", Server: "team-b", Last: 5},
		{Agent: "a", Host: "gw", Last: 7}, // unnamed on a shared host: no evidence
		{Agent: "b", Host: "gw", Server: "team-a", Last: 9},
	}
	if s, ok := table.LastNamedServer("a", both, shared); s != "team-b" || !ok {
		t.Errorf("shared host: (%q, %v), want (team-b, true)", s, ok)
	}
	if s, ok := table.LastNamedServer("a", onlyA, shared); s != "" || ok {
		t.Errorf("removed server: (%q, %v), want (\"\", false)", s, ok)
	}
	one := InferenceHosts{{Agent: "a", Host: "one", Last: 4}}
	if s, ok := one.LastNamedServer("a", both, func(string) (string, bool) { return "solo", true }); s != "solo" || !ok {
		t.Errorf("one server on the host: (%q, %v), want (solo, true)", s, ok)
	}
	other := InferenceHosts{{Agent: "a", Host: "other", Last: 3, LastUnrouted: 3}}
	if s, ok := other.LastNamedServer("a", both, func(string) (string, bool) { return "", false }); s != "" || !ok {
		t.Errorf("no server on the host: (%q, %v), want (\"\", true)", s, ok)
	}
	if s, ok := table.LastNamedServer("c", both, shared); s != "" || ok {
		t.Errorf("another agent: (%q, %v), want (\"\", false)", s, ok)
	}
}
