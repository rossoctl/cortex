package tokenexchangeaiac

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	grantTokenExchange   = "urn:ietf:params:oauth:grant-type:token-exchange"
	tokenTypeAccessToken = "urn:ietf:params:oauth:token-type:access_token"

	// maxIdPResponse bounds what is read from the token endpoint. A token
	// response is a few kilobytes; anything near this is not one.
	maxIdPResponse = 1 << 20
)

// outcome is what a failed IdP call means for the request that caused it.
type outcome int

const (
	// outcomeFailed: deny. The default for every error not listed below,
	// including transport failures, so an IdP outage never becomes a
	// passthrough.
	outcomeFailed outcome = iota
	// outcomeUnknownAudience: the IdP has no client by that name, so the
	// destination is not a target it knows. Passthrough.
	outcomeUnknownAudience
	// outcomeNotGranted: the client exists but this caller holds no grant
	// for it. Deny, loudly — that is a missing grant, not a stranger.
	outcomeNotGranted
	// outcomeTargetDisabled: the destination's client exists but is
	// disabled. Deny as forbidden: a deliberate IdP answer, so retrying the
	// same call cannot succeed.
	outcomeTargetDisabled
	// outcomeSubjectInvalid: the IdP rejected the subject token itself, before
	// looking at the audience, so the answer says nothing about the
	// destination. The caller asks again with a subject the IdP accepts.
	outcomeSubjectInvalid
)

// idpError is a non-200 answer from the token endpoint, carrying the RFC 6749
// error fields.
type idpError struct {
	status      int
	code        string
	description string
}

func (e *idpError) Error() string {
	if e.description != "" {
		return fmt.Sprintf("token endpoint answered HTTP %d: %s: %s", e.status, e.code, e.description)
	}
	return fmt.Sprintf("token endpoint answered HTTP %d: %s", e.status, e.code)
}

// classify maps an IdP error to an outcome. The four recognised answers are
// Keycloak's, from its standard token exchange provider, and were confirmed
// against Keycloak 26.5:
//
//	audience names no client             400 invalid_client  "Audience not found"
//	client exists, caller has no grant   400 invalid_request "Requested audience not available: <aud>"
//	audience names a disabled client     400 invalid_client  "Client disabled"
//	subject is not a valid token         400 invalid_request "Invalid token"
//
// The last is checked before the audience is: an expired, malformed or
// wrongly signed subject, or a bearer the IdP never issued (an API key), gets
// it for every audience, the unknown ones included.
//
// Both the code AND the description are matched. invalid_client alone also
// covers bad client credentials, and invalid_request alone also covers a
// subject token not issued to this caller; none of those may become a
// passthrough. A disabled caller is not "Client disabled": Keycloak answers it
// 401 invalid_client "Invalid client or Invalid client credentials", so it stays
// a failure. If a Keycloak upgrade rewords a message, the match fails towards
// a 503 deny, never towards passthrough.
func classify(err error) outcome {
	var ie *idpError
	if !errors.As(err, &ie) || ie.status != http.StatusBadRequest {
		return outcomeFailed
	}
	switch {
	case ie.code == "invalid_client" && strings.HasPrefix(ie.description, "Audience not found"):
		return outcomeUnknownAudience
	case ie.code == "invalid_request" && strings.HasPrefix(ie.description, "Requested audience not available"):
		return outcomeNotGranted
	case ie.code == "invalid_client" && ie.description == "Client disabled":
		return outcomeTargetDisabled
	case ie.code == "invalid_request" && ie.description == "Invalid token":
		return outcomeSubjectInvalid
	}
	return outcomeFailed
}

type credentials struct {
	clientID     string
	clientSecret string
}

type issuedToken struct {
	accessToken string
	expiresIn   time.Duration
}

// idpClient talks to one OAuth token endpoint, authenticating with a client
// secret in the form body (client_secret_post), which is what Keycloak's
// operator-registered clients accept.
type idpClient struct {
	tokenURL string
	http     *http.Client
}

// exchange trades subject for a token whose audience is exactly audience.
//
// No scope is sent. This plugin assumes every grant is modelled as a DEFAULT
// client scope on the caller, which Keycloak applies without being asked;
// optional scopes would have to be requested by name, and this plugin has no
// source for those names. An audience is always sent: an exchange without one
// succeeds and returns a token for every audience the caller holds.
func (c *idpClient) exchange(ctx context.Context, creds credentials, subject, audience string) (issuedToken, error) {
	if audience == "" {
		return issuedToken{}, errors.New("refusing an exchange with no audience: it would return the caller's broadest token")
	}
	form := url.Values{
		"grant_type":         {grantTokenExchange},
		"subject_token":      {subject},
		"subject_token_type": {tokenTypeAccessToken},
		"audience":           {audience},
	}
	tok, err := c.post(ctx, creds, form)
	if err != nil {
		return issuedToken{}, err
	}
	// Defence in depth. Keycloak narrows aud to the requested audience, but
	// this plugin's whole guarantee is "the token sent to X is a token for
	// X", so it is checked rather than assumed.
	if err := audienceContains(tok.accessToken, audience); err != nil {
		return issuedToken{}, err
	}
	return tok, nil
}

// clientCredentials obtains a token for the caller itself. Keycloak ignores an
// audience parameter on this grant and returns a token carrying every audience
// the caller holds, so the result must never be forwarded as is: it is only
// ever used as the subject of an exchange, which narrows it.
func (c *idpClient) clientCredentials(ctx context.Context, creds credentials) (issuedToken, error) {
	return c.post(ctx, creds, url.Values{"grant_type": {"client_credentials"}})
}

func (c *idpClient) post(ctx context.Context, creds credentials, form url.Values) (issuedToken, error) {
	form.Set("client_id", creds.clientID)
	form.Set("client_secret", creds.clientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return issuedToken{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return issuedToken{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIdPResponse))
	if err != nil {
		return issuedToken{}, fmt.Errorf("reading token endpoint response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &e) // a non-JSON error body still yields an idpError
		return issuedToken{}, &idpError{status: resp.StatusCode, code: e.Error, description: e.Description}
	}
	var ok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &ok); err != nil {
		return issuedToken{}, fmt.Errorf("decoding token endpoint response: %w", err)
	}
	if ok.AccessToken == "" {
		return issuedToken{}, errors.New("token endpoint answered 200 with no access_token")
	}
	return issuedToken{accessToken: ok.AccessToken, expiresIn: time.Duration(ok.ExpiresIn) * time.Second}, nil
}

// audienceContains reports whether a JWT's aud claim includes audience. The
// signature is not checked: the token came straight from the IdP over the
// connection this plugin opened, and the question is what the IdP issued, not
// whether to trust it. An opaque (non-JWT) token cannot be checked and is
// refused.
func audienceContains(token, audience string) error {
	var claims struct {
		Aud json.RawMessage `json:"aud"`
	}
	if err := decodeClaims(token, &claims); err != nil {
		return err
	}
	var auds []string
	var one string
	switch {
	case json.Unmarshal(claims.Aud, &one) == nil:
		auds = []string{one}
	case json.Unmarshal(claims.Aud, &auds) == nil:
	default:
		return errors.New("issued token has no usable aud claim")
	}
	for _, a := range auds {
		if a == audience {
			return nil
		}
	}
	return fmt.Errorf("issued token's audience %v does not include %q", auds, audience)
}

// tokenSubjectAndScopes returns a JWT's sub claim and its space-separated scope
// claim, unverified, for the same reason as audienceContains. A token it cannot
// decode yields zero values: the claims only enrich policy input.
func tokenSubjectAndScopes(token string) (string, []string) {
	var claims struct {
		Sub   string `json:"sub"`
		Scope string `json:"scope"`
	}
	if decodeClaims(token, &claims) != nil {
		return "", nil
	}
	scopes := strings.Fields(claims.Scope)
	if len(scopes) == 0 {
		scopes = nil
	}
	return claims.Sub, scopes
}

// decodeClaims unmarshals a JWT's payload into claims, without checking the
// signature.
func decodeClaims(token string, claims any) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errors.New("issued token is not a JWT, so its claims cannot be read")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return fmt.Errorf("issued token payload is not base64url: %w", err)
	}
	if err := json.Unmarshal(payload, claims); err != nil {
		return fmt.Errorf("issued token payload is not JSON: %w", err)
	}
	return nil
}
