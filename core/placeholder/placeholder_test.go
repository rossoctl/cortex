package placeholder

import (
	"strings"
	"testing"
)

func TestNew_HasPrefixAndIsUnique(t *testing.T) {
	a, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !strings.HasPrefix(a, Prefix) {
		t.Fatalf("handle %q missing prefix %q", a, Prefix)
	}
	if len(a) <= len(Prefix)+10 {
		t.Fatalf("handle %q too short", a)
	}
	b, _ := New()
	if a == b {
		t.Fatal("two handles collided")
	}
}

func TestIsPlaceholder(t *testing.T) {
	h, _ := New()
	if !IsPlaceholder(h) {
		t.Fatalf("IsPlaceholder(%q) = false", h)
	}
	if IsPlaceholder("eyJhbGci-real-jwt") {
		t.Fatal("real token misclassified as placeholder")
	}
}

func TestKey_NamespacesHandle(t *testing.T) {
	if got := Key("abph_xyz"); got != "placeholder/abph_xyz" {
		t.Fatalf("Key = %q", got)
	}
}

type mapStore map[string]any

func (m mapStore) Get(k string) (any, bool) { v, ok := m[k]; return v, ok }

func TestResolve_FailsClosed(t *testing.T) {
	store := mapStore{Key("abph_ok"): "real", Key("abph_int"): 42}
	if got, ok := Resolve(store, "abph_ok"); !ok || got != "real" {
		t.Fatalf("Resolve(known) = %q, %v", got, ok)
	}
	for _, h := range []string{"abph_missing", "abph_int"} {
		if got, ok := Resolve(store, h); ok || got != "" {
			t.Errorf("Resolve(%q) = %q, %v; want fail closed", h, got, ok)
		}
	}
	// A nil interface from pipeline.Context.Shared must fail closed, not panic.
	var none interface{ Get(string) (any, bool) }
	if _, ok := Resolve(none, "abph_ok"); ok {
		t.Error("Resolve with no store succeeded")
	}
}
