package jwtvalidation

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/auth"
	"github.com/rossoctl/cortex/core/bypass"
	"github.com/rossoctl/cortex/core/listener/reverseproxy"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// A tokenless request whose path only cleans to a bypass path is validated. The
// reverse proxy forwards the path as sent, so the app could route it elsewhere.
func TestJWTValidation_BypassNeedsACanonicalPath(t *testing.T) {
	var hits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer backend.Close()

	matcher, err := bypass.NewMatcher(bypass.DefaultPatterns)
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}
	p := newTestJWTValidation(t, "http://issuer", auth.New(auth.Config{
		Bypass:   matcher,
		Verifier: &mockJWTVerifier{},
		Identity: auth.IdentityConfig{Audiences: []string{"agent-aud"}},
	}))
	pl, err := pipeline.New([]pipeline.Plugin{p})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	store := session.New(5*time.Minute, 100, 100)
	defer store.Close()
	srv, err := reverseproxy.NewServer(pipeline.NewHolder(pl), store, backend.URL, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	for target, want := range map[string]int{
		"/healthz":          http.StatusOK,
		"/mcp/../healthz":   http.StatusUnauthorized,
		"/mcp/..%2Fhealthz": http.StatusUnauthorized,
		"/healthz/":         http.StatusUnauthorized,
		"/healthz%3F":       http.StatusUnauthorized,
	} {
		before := hits.Load()
		if got := postRaw(t, proxy.URL, target); got != want {
			t.Errorf("POST %s: status %d, want %d", target, got, want)
		}
		if reached := hits.Load() != before; reached != (want == http.StatusOK) {
			t.Errorf("POST %s: backend reached = %v", target, reached)
		}
	}
}

// postRaw sends a tokenless POST with target written as is, which net/http's client
// would clean, and returns the status.
func postRaw(t *testing.T, proxyURL, target string) int {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(proxyURL, "http://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	fmt.Fprintf(c, "POST %s HTTP/1.1\r\nHost: agent\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", target)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("POST %s: %v", target, err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}
