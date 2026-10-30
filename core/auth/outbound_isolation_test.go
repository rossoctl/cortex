package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rossoctl/cortex/core/plugins/tokenexchange/cache"
	"github.com/rossoctl/cortex/core/plugins/tokenexchange/exchange"
	"github.com/rossoctl/cortex/core/routing"
)

// scopeEchoServer is a fake IdP whose issued token encodes the scope (and
// which server issued it), so a test can tell which grant a cached token
// came from.
func scopeEchoServer(t *testing.T, name string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": name + "-" + r.FormValue("scope"),
			"token_type":   "Bearer",
			"expires_in":   300,
		})
	}))
}

// Repro for #1563 case 1: two routes share an audience but differ in
// scopes. The second route must not be served the first route's
// broader-scoped token from the cache.
func TestHandleOutbound_CacheKeysScopes(t *testing.T) {
	srv := scopeEchoServer(t, "idp")
	defer srv.Close()

	router, _ := routing.NewRouter("passthrough", []routing.Route{
		{Host: "a-svc", Audience: "X", Scopes: "openid"},
		{Host: "b-svc", Audience: "X", Scopes: "openid X-admin"},
	})
	a := New(Config{
		Router:    router,
		Exchanger: exchange.NewClient(srv.URL, &exchange.ClientSecretAuth{ClientID: "agent", ClientSecret: "s"}),
		Cache:     cache.New(),
	})

	// b-svc first: mints the admin-scoped token.
	res := a.HandleOutbound(context.Background(), "Bearer user-token", "b-svc")
	if res.Action != ActionReplaceToken || res.Token != "idp-openid X-admin" {
		t.Fatalf("b-svc: action=%s token=%q", res.Action, res.Token)
	}

	// a-svc must NOT be served b-svc's admin-scoped token from the cache.
	res = a.HandleOutbound(context.Background(), "Bearer user-token", "a-svc")
	if res.Action != ActionReplaceToken {
		t.Fatalf("a-svc: action=%s (%s)", res.Action, res.DenyReason)
	}
	if res.Token == "idp-openid X-admin" {
		t.Fatal("a-svc was served b-svc's admin-scoped token from the cache: the cache key ignores scopes")
	}
	if res.Token != "idp-openid" {
		t.Errorf("a-svc token = %q, want a fresh exchange with scopes \"openid\"", res.Token)
	}
}

// Repro for #1563 case 1: two routes share an audience but point at
// different token endpoints (two realms/IdPs). The cache must not serve
// one IdP's token for the other's route.
func TestHandleOutbound_CacheKeysTokenEndpoint(t *testing.T) {
	realmA := scopeEchoServer(t, "realmA")
	defer realmA.Close()
	realmB := scopeEchoServer(t, "realmB")
	defer realmB.Close()

	router, _ := routing.NewRouter("passthrough", []routing.Route{
		{Host: "a-svc", Audience: "X", TokenEndpoint: realmA.URL},
		{Host: "b-svc", Audience: "X", TokenEndpoint: realmB.URL},
	})
	a := New(Config{
		Router:    router,
		Exchanger: exchange.NewClient("http://default.invalid", &exchange.ClientSecretAuth{ClientID: "agent", ClientSecret: "s"}),
		Cache:     cache.New(),
	})

	res := a.HandleOutbound(context.Background(), "Bearer user-token", "b-svc")
	if res.Action != ActionReplaceToken || res.Token != "realmB-" {
		t.Fatalf("b-svc: action=%s token=%q", res.Action, res.Token)
	}

	res = a.HandleOutbound(context.Background(), "Bearer user-token", "a-svc")
	if res.Action != ActionReplaceToken {
		t.Fatalf("a-svc: action=%s (%s)", res.Action, res.DenyReason)
	}
	if res.Token == "realmB-" {
		t.Fatal("a-svc was served realm B's token from the cache: the cache key ignores the token endpoint")
	}
	if res.Token != "realmA-" {
		t.Errorf("a-svc token = %q, want a fresh exchange against realm A", res.Token)
	}
}

// Repro for #1563 case 2: default_policy exchange with no matching route
// and no audience derivation resolves an empty audience. The exchange
// request then carries no audience parameter, which the IdP answers with
// a token valid for every audience the caller holds — and the proxy
// forwards that token to the (possibly attacker-controlled) host. The
// proxy must refuse instead.
func TestHandleOutbound_EmptyAudienceExchangeRefused(t *testing.T) {
	var gotAudienceSet bool
	var gotAudience string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_, gotAudienceSet = r.Form["audience"]
		gotAudience = r.FormValue("audience")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "broad-token",
			"token_type":   "Bearer",
			"expires_in":   300,
		})
	}))
	defer srv.Close()

	router, _ := routing.NewRouter("exchange", []routing.Route{})
	a := New(Config{
		Router:    router,
		Exchanger: exchange.NewClient(srv.URL, &exchange.ClientSecretAuth{ClientID: "agent", ClientSecret: "s"}),
		Cache:     cache.New(),
	})

	res := a.HandleOutbound(context.Background(), "Bearer user-token", "evil.example.com")
	if res.Action == ActionReplaceToken {
		t.Fatalf("unrouted host with empty resolved audience got a token (%q, audience param set=%v %q): an audience-less exchange returns the caller's broadest token and must be refused",
			res.Token, gotAudienceSet, gotAudience)
	}
	if res.Action != ActionDeny {
		t.Fatalf("expected deny, got %s", res.Action)
	}
}
