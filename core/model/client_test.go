package model

import "testing"

// IsKnownAgent answers for canonical names only: what AgentName folds a recognised Label to.
func TestIsKnownAgent(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"claude-code", true},
		{"bob", true},
		{"opencode", true},
		{"codex", true},
		{"pi", true},
		// A product token is not a canonical name: claude-cli is how Claude Code is RECOGNISED.
		{"claude-cli", false},
		// Retired spellings are not canonical names either — but AgentName folds them to
		// one before any consumer asks, so IsKnownAgent(AgentName(label)) stays true for
		// a label recorded under the old name. Pinned by TestAgentName below.
		{"bob-shell", false},
		{"ibm-bob", false},
		{"codex_exec", false},
		{"codex-mcp-client", false},
		// Versioned labels have to be folded first; the godoc says so.
		{"claude-code/2.1.285", false},
		{"curl", false},
		{UnknownClientLabel, false},
		{"(other)", false},
		{"", false},
	} {
		if got := IsKnownAgent(tc.name); got != tc.want {
			t.Errorf("IsKnownAgent(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
	// Every value ParseUserAgent can put in Name is one this accepts, so a new knownClients
	// entry cannot be recognised by the parser and rejected here.
	for product, name := range knownClients {
		if !IsKnownAgent(name) {
			t.Errorf("knownClients[%q] = %q, which IsKnownAgent rejects", product, name)
		}
	}
}
