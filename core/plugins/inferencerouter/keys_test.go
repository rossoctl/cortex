package inferencerouter

import (
	"encoding/json"
	"maps"
	"net/http"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/plugins/inferencerouter/routerconfig"
	"github.com/rossoctl/cortex/core/redact"
)

// keyConfig is sharedConfig with the key_routes block given, which may be empty.
func keyConfig(agents, keyRoutes string) string {
	return strings.TrimSuffix(strings.TrimSpace(sharedConfig(agents)), "}") + `, "key_routes": {` + keyRoutes + `}}`
}

func TestKeyOwner(t *testing.T) {
	r := New()
	cfg := strings.Replace(sharedConfig(""), `"servers": {`, `"servers": {
		"hdr": {"url": "https://hdr.example.com", "key": "hdr-key", "main": "opus", "helper": "haiku", "auth_header": "x-team-key"},`, 1)
	if err := r.Configure(json.RawMessage(cfg)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	for _, tc := range []struct {
		name string
		h    http.Header
		want string
	}{
		{"bearer", http.Header{"Authorization": {"Bearer team-b-key"}}, "team-b"},
		{"x-api-key", http.Header{"X-Api-Key": {"team-b-key"}}, "team-b"},
		{"auth_header", http.Header{"X-Team-Key": {"hdr-key"}}, "hdr"},
		{"two servers", http.Header{"Authorization": {"Bearer team-a-key"}, "X-Api-Key": {"team-b-key"}}, ""},
		{"one server twice", http.Header{"Authorization": {"Bearer team-b-key"}, "X-Api-Key": {"team-b-key"}}, "team-b"},
		{"client key", http.Header{"Authorization": {"Bearer client-key"}}, ""},
		{"subscription token", http.Header{"Authorization": {"Bearer sk-ant-oat01-xyz"}}, ""},
		{"any read header, any key", http.Header{"X-Team-Key": {"team-b-key"}}, "team-b"},
		{"no headers", http.Header{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.keyOwner(tc.h); got != tc.want {
				t.Errorf("keyOwner = %q, want %q", got, tc.want)
			}
		})
	}
}

// /config and /v1/pipeline serve the config through redact.JSON; key_routes holds
// server names only, so it must survive and still decode.
func TestKeyRoutes_SurvivesRedaction(t *testing.T) {
	out := redact.JSON(json.RawMessage(keyConfig("", `"team-b": "team-a"`)))
	var c routerconfig.Config
	if err := json.Unmarshal(out, &c); err != nil {
		t.Fatalf("Unmarshal of the redacted config: %v\n%s", err, out)
	}
	if want := map[string]string{"team-b": "team-a"}; !maps.Equal(c.KeyRoutes, want) {
		t.Errorf("KeyRoutes = %v, want %v", c.KeyRoutes, want)
	}
	if s := string(out); strings.Contains(s, "team-a-key") || strings.Contains(s, "team-b-key") {
		t.Errorf("the redacted config holds a key:\n%s", s)
	}
}
