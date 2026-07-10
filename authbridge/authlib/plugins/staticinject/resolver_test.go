package staticinject

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// =============================================================================
// FileResolver tests
// =============================================================================

func TestFileResolver_ReadsAndTrims(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "api.example.com"), []byte("secret-value\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	r := FileResolver{Dir: dir}
	value, ok := r.Resolve(context.Background(), "api.example.com")
	if !ok {
		t.Fatalf("Resolve() ok = false, want true")
	}
	if value != "secret-value" {
		t.Errorf("Resolve() value = %q, want %q", value, "secret-value")
	}
}

func TestFileResolver_RejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	// A file that a naive join could escape to, one directory above Dir.
	if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "x"), []byte("nope"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	defer os.Remove(filepath.Join(filepath.Dir(dir), "x"))

	r := FileResolver{Dir: dir}
	value, ok := r.Resolve(context.Background(), "../x")
	if ok {
		t.Fatalf("Resolve() ok = true, want false (path traversal must be rejected)")
	}
	if value != "" {
		t.Errorf("Resolve() value = %q, want empty on rejection", value)
	}
}

func TestFileResolver_MissingKey(t *testing.T) {
	dir := t.TempDir()

	r := FileResolver{Dir: dir}
	value, ok := r.Resolve(context.Background(), "does-not-exist")
	if ok {
		t.Fatalf("Resolve() ok = true, want false for missing key")
	}
	if value != "" {
		t.Errorf("Resolve() value = %q, want empty on missing key", value)
	}
}

// =============================================================================
// MapResolver tests
// =============================================================================

func TestMapResolver(t *testing.T) {
	r := MapResolver{"api.example.com": "REAL"}

	value, ok := r.Resolve(context.Background(), "api.example.com")
	if !ok {
		t.Fatalf("Resolve() ok = false, want true for present key")
	}
	if value != "REAL" {
		t.Errorf("Resolve() value = %q, want %q", value, "REAL")
	}

	_, ok = r.Resolve(context.Background(), "missing.example.com")
	if ok {
		t.Fatalf("Resolve() ok = true, want false for absent key")
	}
}

// =============================================================================
// SafeHeaderValue / SafeSetHeader tests
// =============================================================================

func TestSafeHeaderValue_RejectsCRLFNUL(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{"normal token", "Bearer abc123", true},
		{"contains CR", "abc\rdef", false},
		{"contains LF", "abc\ndef", false},
		{"contains NUL", "abc\x00def", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SafeHeaderValue(tt.value); got != tt.want {
				t.Errorf("SafeHeaderValue(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestSafeSetHeader(t *testing.T) {
	h := http.Header{}
	if ok := SafeSetHeader(h, "Authorization", "Bearer safe-value"); !ok {
		t.Fatalf("SafeSetHeader() ok = false, want true for a safe value")
	}
	if got := h.Get("Authorization"); got != "Bearer safe-value" {
		t.Errorf("header value = %q, want %q", got, "Bearer safe-value")
	}

	h2 := http.Header{"Authorization": []string{"Bearer original"}}
	if ok := SafeSetHeader(h2, "Authorization", "evil\r\nX-Injected: 1"); ok {
		t.Fatalf("SafeSetHeader() ok = true, want false for an unsafe value")
	}
	if got := h2.Get("Authorization"); got != "Bearer original" {
		t.Errorf("header value = %q, want unmodified %q", got, "Bearer original")
	}
}
