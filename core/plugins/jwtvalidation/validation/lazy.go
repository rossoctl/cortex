package validation

import (
	"context"
	"fmt"
	"sync"
)

// LazyJWKSVerifier wraps JWKSVerifier with lazy initialization.
// The JWKS keys are fetched on the first Verify() call, not at construction time.
// This avoids blocking startup on an HTTP call to the JWKS endpoint.
//
// Unlike sync.Once, initialization is retried on failure so that a temporarily
// unreachable JWKS endpoint doesn't permanently break token validation.
type LazyJWKSVerifier struct {
	jwksURL string
	issuer  string
	opts    []JWKSOption

	mu    sync.Mutex
	inner *JWKSVerifier
}

// NewLazyJWKSVerifier creates a Verifier that defers the JWKS HTTP fetch
// until the first token needs to be verified. This allows the authbridge
// gRPC listener to start immediately without waiting for the JWKS endpoint.
func NewLazyJWKSVerifier(jwksURL, issuer string, opts ...JWKSOption) *LazyJWKSVerifier {
	return &LazyJWKSVerifier{
		jwksURL: jwksURL,
		issuer:  issuer,
		opts:    opts,
	}
}

// Verify initializes the underlying JWKSVerifier on first call, then delegates.
// If initialization fails, it retries on the next call rather than caching the error.
func (v *LazyJWKSVerifier) Verify(ctx context.Context, tokenStr string, audiences []string) (*Claims, error) {
	v.mu.Lock()
	if v.inner == nil {
		inner, err := NewJWKSVerifier(ctx, v.jwksURL, v.issuer, v.opts...)
		if err != nil {
			v.mu.Unlock()
			return nil, fmt.Errorf("JWKS verifier init: %w", err)
		}
		v.inner = inner
	}
	inner := v.inner
	v.mu.Unlock()

	return inner.Verify(ctx, tokenStr, audiences)
}
