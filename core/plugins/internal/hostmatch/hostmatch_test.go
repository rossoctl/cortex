package hostmatch

import "testing"

func TestMatchesAny(t *testing.T) {
	patterns := []string{"keycloak.*", "otel-collector", "::1"}
	for _, tc := range []struct {
		host string
		want bool
	}{
		{"keycloak.local", true},
		{"keycloak.local:8081", true},
		{"KeyCloak.Local", true},
		{"keycloak", false},
		{"otel-collector:4317", true},
		{"otel-collector.ns.svc", false},
		{"[::1]:4317", true},
		{"[::2]:4317", false},
		{"", false},
	} {
		if got := MatchesAny(patterns, tc.host); got != tc.want {
			t.Errorf("MatchesAny(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}
