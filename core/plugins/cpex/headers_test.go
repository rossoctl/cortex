package cpex

import (
	"net/http"
	"testing"
)

func TestIsSensitive(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// Authorization deliberately passes through (jwt resolvers need it).
		{"Authorization", false},
		{"authorization", false},
		{"Content-Type", false},
		{"X-Request-Id", false},
		// Stripped (case-insensitive, exact + prefix).
		{"Cookie", true},
		{"cookie", true},
		{"Set-Cookie", true},
		{"X-Api-Key", true},
		{"x-api-key", true},
		{"X-CSRF-Token", true},
		{"x-csrf-token", true},
		{"Proxy-Authorization", true},
		{"X-Amz-Security-Token-Extra", true}, // prefix match
	}
	for _, tc := range cases {
		if got := isSensitive(tc.name); got != tc.want {
			t.Errorf("isSensitive(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestFlattenHeaders(t *testing.T) {
	h := http.Header{
		"Authorization": {"Bearer abc"},
		"Cookie":        {"session=xyz"},
		"X-Api-Key":     {"secret"},
		"X-Csrf-Token":  {"t"},
		"Content-Type":  {"application/json"},
		"X-Multi":       {"a", "b"},
	}
	out := flattenHeaders(h)

	// Authorization passes through.
	if out["Authorization"] != "Bearer abc" {
		t.Errorf("Authorization should pass through, got %q", out["Authorization"])
	}
	// Sensitive headers stripped (case-insensitively).
	for _, k := range []string{"Cookie", "X-Api-Key", "X-Csrf-Token"} {
		if _, ok := out[k]; ok {
			t.Errorf("%s should have been stripped: %v", k, out)
		}
	}
	// Plain header preserved.
	if out["Content-Type"] != "application/json" {
		t.Errorf("Content-Type lost: %v", out)
	}
	// Multi-value comma-joined per RFC 7230 §3.2.2.
	if out["X-Multi"] != "a, b" {
		t.Errorf("X-Multi = %q, want %q", out["X-Multi"], "a, b")
	}
}

func TestFlattenHeaders_Empty(t *testing.T) {
	if flattenHeaders(nil) != nil {
		t.Error("nil header → nil map")
	}
	// All-sensitive input yields nil (not an empty map).
	only := http.Header{"Cookie": {"x"}, "X-Api-Key": {"y"}}
	if flattenHeaders(only) != nil {
		t.Errorf("all-sensitive header set → nil, got %v", flattenHeaders(only))
	}
}
