// Package client provides an HTTP client for the Token Broker service.
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Client is an HTTP client for the Token Broker service.
type Client struct {
	httpClient *http.Client
}

// NewClient creates a new Token Broker client.
func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout: 310 * time.Second, // Longer than Token Broker's 300s timeout
		},
	}
}

// AcquireToken calls the Token Broker to get a token for the given target server.
// The broker extracts user-id and session-key from the provided JWT token.
// Blocks until a token is available or the context is cancelled.
// If authorizationEndpoint is provided, it will be sent to the broker via X-Authorization-Endpoint header.
// If tokenEndpoint is provided, it will be sent to the broker via X-Token-Endpoint header.
func (c *Client) AcquireToken(ctx context.Context, tokenBrokerURL, token, serverURL, authorizationEndpoint, tokenEndpoint string) (string, error) {
	if tokenBrokerURL == "" {
		return "", fmt.Errorf("token broker URL cannot be empty")
	}

	url := fmt.Sprintf("%s/sessions/token", tokenBrokerURL)

	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Server-Url", serverURL)
	if authorizationEndpoint != "" {
		req.Header.Set("X-Authorization-Endpoint", authorizationEndpoint)
	}
	if tokenEndpoint != "" {
		req.Header.Set("X-Token-Endpoint", tokenEndpoint)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token broker request failed: %w", err)
	}
	defer resp.Body.Close()

	slog.Debug("token-broker-client: received response from broker",
		"status_code", resp.StatusCode,
		"server_url", serverURL)

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 10MB limit
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var brokerErr struct {
			Error   string `json:"error"`
			Message string `json:"message"` // Token Broker uses "message" instead of "error_description"
		}
		_ = json.Unmarshal(body, &brokerErr)
		return "", &BrokerError{
			StatusCode:       resp.StatusCode,
			OAuthError:       brokerErr.Error,
			OAuthDescription: brokerErr.Message,
		}
	}

	var result struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parsing token response: %w", err)
	}
	if result.Token == "" {
		return "", fmt.Errorf("token response missing token")
	}

	return result.Token, nil
}
