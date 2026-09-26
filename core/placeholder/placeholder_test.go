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
