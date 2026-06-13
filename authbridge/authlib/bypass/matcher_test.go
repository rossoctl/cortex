package bypass

import (
	"testing"
)

func TestNewMatcher_ValidPatterns(t *testing.T) {
	_, err := NewMatcher(DefaultPatterns)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewMatcher_InvalidPattern(t *testing.T) {
	_, err := NewMatcher([]string{"[invalid"})
	if err == nil {
		t.Fatal("expected error for invalid pattern")
	}
}

func TestMatch(t *testing.T) {
	m, err := NewMatcher(DefaultPatterns)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tests := []struct {
		path  string
		match bool
	}{
		// Default bypass paths
		{"/.well-known/agent.json", true},
		{"/.well-known/openid-configuration", true},
		{"/healthz", true},
		{"/readyz", true},
		{"/livez", true},

		// Non-bypass paths
		{"/test", false},
		{"/api/v1/agents", false},
		{"/", false},

		// Query string stripping
		{"/healthz?verbose=true", true},
		{"/.well-known/agent.json?format=json", true},

		// Path normalization (security)
		{"//healthz", true},
		{"/./healthz", true},
		{"/foo/../healthz", true},

		// Well-known does NOT match nested paths (path.Match * doesn't cross /)
		{"/.well-known/foo/bar", false},
	}

	for _, tt := range tests {
		got := m.Match(tt.path)
		if got != tt.match {
			t.Errorf("Match(%q) = %v, want %v", tt.path, got, tt.match)
		}
	}
}

func TestMatch_EmptyPatterns(t *testing.T) {
	m, _ := NewMatcher(nil)
	if m.Match("/healthz") {
		t.Error("empty matcher should not match anything")
	}
}

func TestMatch_CustomPatterns(t *testing.T) {
	m, err := NewMatcher([]string{"/public/*", "/status"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !m.Match("/public/index.html") {
		t.Error("expected /public/index.html to match")
	}
	if !m.Match("/status") {
		t.Error("expected /status to match")
	}
	if m.Match("/private/data") {
		t.Error("expected /private/data to not match")
	}
}

func TestNewMatcher_RejectsFootgunPatterns(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
	}{
		{"star", "*"},
		{"slash-star", "/*"},
		{"empty", ""},
		{"whitespace-only", "  "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewMatcher([]string{tt.pattern})
			if err == nil {
				t.Errorf("NewMatcher(%q) should return error for match-all pattern", tt.pattern)
			}
		})
	}
}

func TestNewMatcher_AllowsSpecificStar(t *testing.T) {
	_, err := NewMatcher([]string{"/.well-known/*"})
	if err != nil {
		t.Fatalf("specific star pattern should be allowed: %v", err)
	}
}

func TestNewMatcher_TrimsWhitespace(t *testing.T) {
	m, err := NewMatcher([]string{" /healthz "})
	if err != nil {
		t.Fatalf("whitespace-padded pattern should be accepted: %v", err)
	}
	if !m.Match("/healthz") {
		t.Error("trimmed pattern should match /healthz")
	}
}
