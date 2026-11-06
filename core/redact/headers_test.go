package redact

import (
	"net/http"
	"testing"
)

func TestHeaders_DropsCredentialsAndCopies(t *testing.T) {
	in := http.Header{"Authorization": {"Bearer x"}, "X-Api-Key": {"k"}, "cookie": {"c"}, "Anthropic-Beta": {"a", "b"}}
	out := Headers(in)
	if len(out) != 1 || len(out["Anthropic-Beta"]) != 2 {
		t.Fatalf("Headers = %v, want only Anthropic-Beta", out)
	}
	out["Anthropic-Beta"][0] = "changed"
	if in["Anthropic-Beta"][0] != "a" {
		t.Fatal("Headers shares its values with its input")
	}
}
