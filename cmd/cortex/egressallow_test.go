package main

import "testing"

// resolveEgressAllow picks the allow list's source: config wins over the
// MOCA_EGRESS_ALLOW env var, the env var is split on commas with blanks
// dropped, and nothing configured anywhere means the feature is off (nil).
// The env var is the MOCA conformance harness's only channel into the target,
// so the fallback is the contract, not a convenience.
func TestResolveEgressAllow(t *testing.T) {
	configured := []string{"a.test:443", "b.test:80"}
	if got := resolveEgressAllow(configured, func(string) string {
		t.Fatal("config entries must win without consulting the environment")
		return ""
	}); len(got) != 2 || got[0] != "a.test:443" || got[1] != "b.test:80" {
		t.Errorf("config wins: got %v, want the configured entries", got)
	}

	env := func(name, value string) func(string) string {
		return func(k string) string {
			if k == envEgressAllow {
				return value
			}
			return ""
		}
	}

	if got := resolveEgressAllow(nil, env(envEgressAllow, " a.test:443 , ,b.test:80 ")); len(got) != 2 ||
		got[0] != "a.test:443" || got[1] != "b.test:80" {
		t.Errorf("env fallback: got %v, want the two comma-separated entries, trimmed", got)
	}
	if got := resolveEgressAllow(nil, env(envEgressAllow, "  ")); got != nil {
		t.Errorf("blank env: got %v, want nil — off, not an empty-but-armed list", got)
	}
	if got := resolveEgressAllow(nil, env(envEgressAllow, "")); got != nil {
		t.Errorf("unset env: got %v, want nil", got)
	}
	if got := resolveEgressAllow(nil, func(string) string { return "" }); got != nil {
		t.Errorf("nothing configured: got %v, want nil", got)
	}
}
