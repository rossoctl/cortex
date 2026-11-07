package attribution

import (
	"net/http"
	"testing"
)

// Strip removes every attribution header and nothing else.
func TestWire_StripRemovesOnlyAttribution(t *testing.T) {
	h := http.Header{}
	for _, k := range wireHeaders {
		h.Set(k, "forged")
	}
	h.Set("User-Agent", "curl/8")
	Strip(h)
	if len(h) != 1 || h.Get("User-Agent") != "curl/8" {
		t.Errorf("after Strip: %v, want only User-Agent", h)
	}
}
