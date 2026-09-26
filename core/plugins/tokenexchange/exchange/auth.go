package exchange

import (
	"context"
	"net/url"
)

// ClientAuth applies client authentication to a token exchange request.
// Implementations modify the form values to include credentials.
type ClientAuth interface {
	Apply(ctx context.Context, form url.Values) error
}

// ClientSecretAuth authenticates using client_id and client_secret in the form body.
type ClientSecretAuth struct {
	ClientID     string
	ClientSecret string
}

func (a *ClientSecretAuth) Apply(_ context.Context, form url.Values) error {
	form.Set("client_id", a.ClientID)
	form.Set("client_secret", a.ClientSecret)
	return nil
}

// JWTAssertionAuth authenticates using a JWT client assertion (e.g., SPIFFE JWT-SVID).
// The tokenSource is called on every request to support key rotation.
type JWTAssertionAuth struct {
	ClientID      string
	AssertionType string // e.g., "urn:ietf:params:oauth:client-assertion-type:jwt-spiffe"
	TokenSource   func(ctx context.Context) (string, error)
}

func (a *JWTAssertionAuth) Apply(ctx context.Context, form url.Values) error {
	token, err := a.TokenSource(ctx)
	if err != nil {
		return err
	}
	form.Set("client_id", a.ClientID)
	form.Set("client_assertion", token)
	form.Set("client_assertion_type", a.AssertionType)
	return nil
}
