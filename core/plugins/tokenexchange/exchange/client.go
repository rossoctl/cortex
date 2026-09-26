// Package exchange implements RFC 8693 OAuth 2.0 Token Exchange
// and client credentials grant.
package exchange

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ExchangeRequest contains the parameters for a token exchange.
type ExchangeRequest struct {
	SubjectToken  string
	Audience      string
	Scopes        string
	ActorToken    string // optional, RFC 8693 Section 4.1
	TokenEndpoint string // optional per-request override of the client's tokenURL
}

// ExchangeResponse contains the result of a successful token exchange.
type ExchangeResponse struct {
	AccessToken string
	TokenType   string
	ExpiresIn   int // seconds
}

// Client performs OAuth token exchange and client credentials requests.
type Client struct {
	tokenURL   string
	authMu     sync.RWMutex
	auth       ClientAuth
	httpClient *http.Client
}

// Option configures the exchange client. No options are defined today;
// the type and the variadic stay so adding one is not a signature change
// for NewClient.
type Option func(*Client)

// NewClient creates a token exchange client.
func NewClient(tokenURL string, auth ClientAuth, opts ...Option) *Client {
	c := &Client{
		tokenURL: tokenURL,
		auth:     auth,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// UpdateAuth replaces the client authentication used for token requests.
// This supports dynamic credential loading after the server has started.
func (c *Client) UpdateAuth(auth ClientAuth) {
	c.authMu.Lock()
	c.auth = auth
	c.authMu.Unlock()
}

// getAuth returns the current client authentication under a read lock.
func (c *Client) getAuth() ClientAuth {
	c.authMu.RLock()
	defer c.authMu.RUnlock()
	return c.auth
}

// Exchange performs an RFC 8693 token exchange.
func (c *Client) Exchange(ctx context.Context, req *ExchangeRequest) (*ExchangeResponse, error) {
	if req.SubjectToken == "" {
		return nil, fmt.Errorf("subject_token is required for token exchange")
	}
	form := url.Values{
		"grant_type":           {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"requested_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
		"subject_token":        {req.SubjectToken},
		"subject_token_type":   {"urn:ietf:params:oauth:token-type:access_token"},
	}
	if req.Audience != "" {
		form.Set("audience", req.Audience)
	}
	if req.Scopes != "" {
		form.Set("scope", req.Scopes)
	}
	if req.ActorToken != "" {
		form.Set("actor_token", req.ActorToken)
		form.Set("actor_token_type", "urn:ietf:params:oauth:token-type:access_token")
	}

	if err := c.getAuth().Apply(ctx, form); err != nil {
		return nil, fmt.Errorf("applying client auth: %w", err)
	}

	tokenURL := c.tokenURL
	if req.TokenEndpoint != "" {
		tokenURL = req.TokenEndpoint
	}
	return c.doTokenRequest(ctx, tokenURL, form)
}

// ClientCredentials performs a client credentials grant.
// audience is included as a form parameter so Keycloak issues a token
// scoped to the target service (matching the old go-processor behavior).
func (c *Client) ClientCredentials(ctx context.Context, audience, scopes string) (*ExchangeResponse, error) {
	form := url.Values{
		"grant_type": {"client_credentials"},
	}
	if audience != "" {
		form.Set("audience", audience)
	}
	if scopes != "" {
		form.Set("scope", scopes)
	}

	if err := c.getAuth().Apply(ctx, form); err != nil {
		return nil, fmt.Errorf("applying client auth: %w", err)
	}

	return c.doTokenRequest(ctx, c.tokenURL, form)
}

func (c *Client) doTokenRequest(ctx context.Context, tokenURL string, form url.Values) (*ExchangeResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()

	// Limit response body to 1MB to prevent OOM from malicious endpoints.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var oauthErr struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &oauthErr)
		return nil, &ExchangeError{
			StatusCode:       resp.StatusCode,
			OAuthError:       oauthErr.Error,
			OAuthDescription: oauthErr.Description,
		}
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("parsing token response: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return nil, fmt.Errorf("token response missing access_token")
	}

	return &ExchangeResponse{
		AccessToken: tokenResp.AccessToken,
		TokenType:   tokenResp.TokenType,
		ExpiresIn:   tokenResp.ExpiresIn,
	}, nil
}
