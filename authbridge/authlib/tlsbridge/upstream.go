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
// injected upstream trust). It NEVER sets InsecureSkipVerify and never uses the
// mesh-mTLS dialer — re-origination must verify the origin the way the agent would.
func NewUpstreamClient(extraRootsPEM []byte) (*http.Client, error) {
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
			TLSClientConfig:       &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}
