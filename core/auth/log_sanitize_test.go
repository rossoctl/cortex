package auth

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/rossoctl/cortex/core/bypass"
	"github.com/rossoctl/cortex/core/plugins/tokenexchange/cache"
	"github.com/rossoctl/cortex/core/plugins/tokenexchange/exchange"
	"github.com/rossoctl/cortex/core/routing"
)

// attrCapture records every string attribute value handed to the logger, before a
// handler escapes it: the rule is about what the code logs, not how a handler prints it.
type attrCapture struct {
	mu   *sync.Mutex
	vals *[]string
}

func newAttrCapture() attrCapture { return attrCapture{mu: &sync.Mutex{}, vals: &[]string{}} }

func (attrCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c attrCapture) WithAttrs([]slog.Attr) slog.Handler     { return c }
func (c attrCapture) WithGroup(string) slog.Handler          { return c }
func (c attrCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	r.Attrs(func(a slog.Attr) bool {
		*c.vals = append(*c.vals, a.Value.String())
		return true
	})
	return nil
}

// hostile carries LF, CR and an ESC sequence: each could forge or recolour a log line.
const hostile = "evil\nfake=1\r\x1b[31m"

func assertNoControl(t *testing.T, c attrCapture) {
	t.Helper()
	if len(*c.vals) == 0 {
		t.Fatal("nothing was logged")
	}
	for _, v := range *c.vals {
		if strings.ContainsAny(v, "\n\r\x1b") {
			t.Errorf("logged value carries a control character: %q", v)
		}
	}
}

func TestHandleInbound_SanitizesLoggedPath(t *testing.T) {
	m, err := bypass.NewMatcher([]string{"/bypass/*"})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		cfg        Config
		authHeader string
		path       string
	}{
		"bypass":      {Config{Bypass: m}, "", "/bypass/" + hostile},
		"no header":   {Config{Verifier: &mockVerifier{}}, "", "/" + hostile},
		"malformed":   {Config{Verifier: &mockVerifier{}}, "Basic x", "/" + hostile},
		"invalid jwt": {Config{Verifier: &mockVerifier{err: errors.New("bad token")}}, "Bearer t", "/" + hostile},
		"valid jwt":   {Config{Verifier: &mockVerifier{claims: validClaims()}}, "Bearer t", "/" + hostile},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := newAttrCapture()
			tc.cfg.Logger = slog.New(c)
			New(tc.cfg).HandleInbound(context.Background(), tc.authHeader, tc.path, "aud")
			assertNoControl(t, c)
		})
	}
}

func TestHandleOutbound_SanitizesLoggedHost(t *testing.T) {
	srv := newTestExchangeServer(t)
	defer srv.Close()
	dead := newTestExchangeServer(t)
	dead.Close()
	route := func(r routing.Route) *routing.Router {
		router, err := routing.NewRouter("passthrough", []routing.Route{r})
		if err != nil {
			t.Fatal(err)
		}
		return router
	}
	auth := &exchange.ClientSecretAuth{ClientID: "agent", ClientSecret: "secret"}
	cached := cache.New()
	cached.Set("user-token", "aud", "", "", "cached-token", 300e9)
	cases := map[string]struct {
		cfg        Config
		authHeader string
	}{
		"no route":    {Config{Router: route(routing.Route{Host: "known-svc", Audience: "x"})}, "Bearer user-token"},
		"passthrough": {Config{Router: route(routing.Route{Host: "*", Action: "passthrough"})}, "Bearer user-token"},
		"derived":     {Config{Router: route(routing.Route{Host: "*"}), AudienceDeriver: routing.ServiceNameFromHost}, "Bearer user-token"},
		"derived no token": {Config{Router: route(routing.Route{Host: "*"}), AudienceDeriver: routing.ServiceNameFromHost,
			NoTokenPolicy: NoTokenPolicyClientCredentials}, ""},
		"no token":     {Config{Router: route(routing.Route{Host: "*", Audience: "aud"})}, ""},
		"no audience":  {Config{Router: route(routing.Route{Host: "*"})}, "Bearer user-token"},
		"cache hit":    {Config{Router: route(routing.Route{Host: "*", Audience: "aud"}), Cache: cached}, "Bearer user-token"},
		"no exchanger": {Config{Router: route(routing.Route{Host: "*", Audience: "aud"})}, "Bearer user-token"},
		"exchange failed": {Config{Router: route(routing.Route{Host: "*", Audience: "aud"}),
			Exchanger: exchange.NewClient(dead.URL, auth)}, "Bearer user-token"},
		"exchanged": {Config{Router: route(routing.Route{Host: "*", Audience: "aud"}),
			Exchanger: exchange.NewClient(srv.URL, auth), Cache: cache.New()}, "Bearer user-token"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := newAttrCapture()
			tc.cfg.Logger = slog.New(c)
			New(tc.cfg).HandleOutbound(context.Background(), tc.authHeader, hostile)
			assertNoControl(t, c)
		})
	}
}
