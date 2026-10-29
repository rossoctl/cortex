package tlsbridge

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"time"
)

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
func NewUpstreamClient(extraRootsPEM []byte, insecure bool) (*http.Client, error) {
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
			TLSClientConfig:       &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12, InsecureSkipVerify: insecure}, //nolint:gosec // opt-in via upstream_insecure for internal/self-signed upstreams
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}
