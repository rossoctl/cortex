package script

import (
	"net/http"
	"testing"
)

// A refused edit must leave nothing applied: the whole map is validated
// before any header is set, whatever order the map iterates in.
func TestApplyHeaders_ARefusedEditLeavesNothingApplied(t *testing.T) {
	for i := 0; i < 100; i++ {
		h := http.Header{}
		e := &edits{SetHeaders: map[string]string{
			"X-One": "1", "X-Two": "2", "X-Three": "3",
			"Authorization": "Bearer stolen",
		}}
		if err := applyHeaders(h, e); err == nil {
			t.Fatal("expected the secret header to be refused")
		}
		for k := range e.SetHeaders {
			if k != "Authorization" && h.Get(k) != "" {
				t.Fatalf("iteration %d: refused edit left %s applied", i, k)
			}
		}
	}
}

// A CR or LF in a name or value would make net/http refuse the outbound
// request — a 502 despite fail-open — so the edit is refused before it lands.
func TestApplyHeaders_RejectsInvalidNamesAndValues(t *testing.T) {
	cases := []struct {
		name string
		e    *edits
	}{
		{"name with CR LF", &edits{SetHeaders: map[string]string{"X-Ok": "1", "Bad\r\nName": "v"}}},
		{"value with CR LF", &edits{SetHeaders: map[string]string{"X-Ok": "1", "X-Bad": "v\r\n"}}},
	}
	for _, c := range cases {
		h := http.Header{}
		if err := applyHeaders(h, c.e); err == nil {
			t.Errorf("%s: expected a refusal", c.name)
		}
		if h.Get("X-Ok") != "" {
			t.Errorf("%s: refused edit left X-Ok applied", c.name)
		}
	}
}
