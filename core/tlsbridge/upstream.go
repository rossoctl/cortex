package tlsbridge

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"time"
)

// UpstreamOption configures the client NewUpstreamClient builds. Variadic so
// the existing two-argument signature — and every caller outside this change —
// is unchanged.
type UpstreamOption func(*upstreamOptions)

type upstreamOptions struct {
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// WithDialContext replaces the re-origination client's dialer. The forward
// proxy passes the egress guard's DialContext here (#1484) so a bridged
// request is subject to the same destination check as a plaintext one — the
// bridge must not become the path around the SSRF guard.
func WithDialContext(dial func(ctx context.Context, network, addr string) (net.Conn, error)) UpstreamOption {
	return func(o *upstreamOptions) { o.dial = dial }
}

// NewUpstreamClient builds the HTTP client the TLS bridge uses to re-originate
// to the real origin. RootCAs = system roots + extraRootsPEM (the agent's
// injected upstream trust). When insecure is true it sets InsecureSkipVerify so
// re-origination does NOT verify the origin cert — for internal/self-signed
// upstreams (e.g. a private LiteLLM endpoint). Prefer extraRootsPEM
// (upstream_ca_bundle) over insecure whenever the origin CA is available.
//
// WebSocket upgrades need no client of their own, tempting as one pinned to
// HTTP/1.1 looks: net/http already pins a request carrying "Connection:
// upgrade" and "Upgrade: websocket" to HTTP/1.1 (Request.requiresHTTP1) — the
// exact pair forwardproxy's stripForUpstream normalizes a forwarded upgrade to
// — so this client carries one as HTTP/1.1 even against an origin it would
// otherwise negotiate h2 with.
func NewUpstreamClient(extraRootsPEM []byte, insecure bool, opts ...UpstreamOption) (*http.Client, error) {
	var o upstreamOptions
	for _, opt := range opts {
		opt(&o)
	}

	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if len(extraRootsPEM) > 0 {
		if !pool.AppendCertsFromPEM(extraRootsPEM) {
			return nil, fmt.Errorf("tlsbridge: upstream_ca_bundle is not valid PEM")
		}
	}
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12, InsecureSkipVerify: insecure}, //nolint:gosec // opt-in via upstream_insecure for internal/self-signed upstreams
			ForceAttemptHTTP2: true,
			MaxIdleConns:      100,
			// Go's default is 2, which churns connections when more than two
			// callers share an h1-only upstream (LiteLLM, Keycloak).
			MaxIdleConnsPerHost:   100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
			DialContext:           o.dial,
		},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}
