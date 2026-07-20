package tokenexchange

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/placeholder"
	"github.com/rossoctl/cortex/authbridge/authlib/shared"
)

func resolveTestPlugin(t *testing.T, exchangeURL string) *TokenExchange {
	t.Helper()
	p := NewTokenExchange()
	raw := []byte(`{
	  "token_url":"` + exchangeURL + `",
	  "default_policy":"exchange",
	  "resolve_placeholders":true,
	  "identity":{"type":"client-secret","client_id":"agent","client_secret":"secret"}
	}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	return p
}

func exchangeStub(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "exchanged-token", "token_type": "Bearer", "expires_in": 300,
		})
	}))
}

func TestResolve_MatchedRouteExchanges(t *testing.T) {
	srv := exchangeStub(t)
	defer srv.Close()
	p := resolveTestPlugin(t, srv.URL)

	st := shared.New()
	handle, _ := placeholder.New()
	st.Put(placeholder.Key(handle), "real-user-token", time.Hour)

	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      "target-svc",
		Headers:   http.Header{"Authorization": []string{"Bearer " + handle}},
		Shared:    st,
	}
	action := invokeOnRequest(p, pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("action = %v, want Continue", action.Type)
	}
	if pctx.Headers.Get("Authorization") != "Bearer exchanged-token" {
		t.Fatalf("auth = %q, want Bearer exchanged-token", pctx.Headers.Get("Authorization"))
	}
}

func TestResolve_MissDenies(t *testing.T) {
	srv := exchangeStub(t)
	defer srv.Close()
	p := resolveTestPlugin(t, srv.URL)

	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      "target-svc",
		Headers:   http.Header{"Authorization": []string{"Bearer abph_unknownhandle"}},
		Shared:    shared.New(),
	}
	action := invokeOnRequest(p, pctx)
	if action.Type != pipeline.Reject {
		t.Fatalf("action = %v, want Reject", action.Type)
	}
}

func TestResolve_NonPlaceholderPassThrough(t *testing.T) {
	srv := exchangeStub(t)
	defer srv.Close()
	p := resolveTestPlugin(t, srv.URL)

	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      "target-svc",
		Headers:   http.Header{"Authorization": []string{"Bearer real-jwt"}},
		Shared:    shared.New(),
	}
	action := invokeOnRequest(p, pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("action = %v, want Continue", action.Type)
	}
	if pctx.Headers.Get("Authorization") != "Bearer exchanged-token" {
		t.Fatalf("auth = %q, want Bearer exchanged-token (normal exchange)", pctx.Headers.Get("Authorization"))
	}
}

func TestResolve_UnmatchedRoutePlaceholderNotLeaked(t *testing.T) {
	p := NewTokenExchange()
	// default_policy defaults to passthrough; with no routes, every host is
	// passthrough — token-exchange must NOT write the resolved real token to
	// the header. The agent forwards the opaque handle off-route; the real
	// token stays in-process.
	raw := []byte(`{
	  "token_url":"http://unused.local",
	  "resolve_placeholders":true,
	  "identity":{"type":"client-secret","client_id":"agent","client_secret":"secret"}
	}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	st := shared.New()
	handle, _ := placeholder.New()
	st.Put(placeholder.Key(handle), "real-user-token", time.Hour)

	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      "some-passthrough-host",
		Headers:   http.Header{"Authorization": []string{"Bearer " + handle}},
		Shared:    st,
	}
	action := invokeOnRequest(p, pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("action = %v, want Continue", action.Type)
	}
	got := pctx.Headers.Get("Authorization")
	if got != "Bearer "+handle {
		t.Fatalf("header = %q, want unchanged \"Bearer %s\" — real token must not leak off-route", got, handle)
	}
}
