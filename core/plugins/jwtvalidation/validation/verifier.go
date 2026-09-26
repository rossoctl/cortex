// Package validation provides JWT verification with JWKS-based key resolution.
package validation

import (
	"context"
	"time"
)

// Verifier validates JWT tokens.
type Verifier interface {
	// Verify parses and validates a JWT token string.
	// audiences must contain at least one expected audience (prevents confused deputy attacks).
	// The token is accepted if its aud claim contains any of the expected audiences.
	// Returns claims on success or an error describing the validation failure.
	Verify(ctx context.Context, tokenStr string, audiences []string) (*Claims, error)
}

// Claims contains the validated claims extracted from a JWT.
type Claims struct {
	Subject   string
	Issuer    string
	Audience  []string
	ClientID  string // "azp" claim
	Scopes    []string
	ExpiresAt time.Time
	Extra     map[string]any
}

// HasAudience checks if the claims contain the given audience.
func (c *Claims) HasAudience(aud string) bool {
	for _, a := range c.Audience {
		if a == aud {
			return true
		}
	}
	return false
}

// HasScope checks if the claims contain the given scope.
func (c *Claims) HasScope(scope string) bool {
	for _, s := range c.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}
