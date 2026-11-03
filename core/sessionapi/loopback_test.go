package sessionapi

import (
	"net/http"
	"testing"
)

// The reads hold raw prompts and completions, so with WithLoopbackOnly a page
// doing DNS rebinding (a foreign Host) or any browser (an Origin) gets 403 on
// every route, not only the erase. A local client still reads.
func TestLoopbackOnlyGuardsEveryRoute(t *testing.T) {
	ts, _ := newTestServer(t, WithLoopbackOnly(true))
	for _, path := range []string{"/", "/v1/sessions", "/v1/sessions/x", "/v1/sessions/x/timeline", "/v1/events", "/v1/pipeline", "/v1/usage", "/healthz"} {
		for _, tc := range []struct {
			name, host, origin string
			want               int
		}{
			{"rebound Host", "evil.example:47601", "", http.StatusForbidden},
			{"browser Origin", "127.0.0.1:47601", "https://evil.example", http.StatusForbidden},
		} {
			req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
			req.Host = tc.host
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("GET %s with %s = %d, want %d", path, tc.name, resp.StatusCode, tc.want)
			}
		}
	}
	// httptest's URL is 127.0.0.1, a loopback Host, so a plain client passes.
	resp, err := http.Get(ts.URL + "/v1/sessions")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /v1/sessions from a local client = %d, want 200", resp.StatusCode)
	}
}

// Without the option (a cluster deployment) a pod's own name is a fine Host.
func TestLoopbackOnlyOffServesAnyHost(t *testing.T) {
	ts, _ := newTestServer(t)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/sessions", nil)
	req.Host = "weather-agent.team1.svc:9094"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /v1/sessions with a pod Host and no WithLoopbackOnly = %d, want 200", resp.StatusCode)
	}
}
