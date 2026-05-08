package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/routing"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/tokenbroker"
)

// tokenBrokerConfig is the plugin's local config schema.
type tokenBrokerConfig struct {
	// BrokerURL is the base URL of the token broker service.
	BrokerURL string `json:"broker_url"`

	// DefaultPolicy is applied when a request's host matches no route:
	// "passthrough" (default) forwards the request unchanged;
	// "broker" attempts to acquire a token from the broker.
	DefaultPolicy string `json:"default_policy"`

	// Routes drives host-to-broker matching. A host that matches no
	// route falls through to DefaultPolicy.
	Routes tokenBrokerRoutes `json:"routes"`
}

type tokenBrokerRoutes struct {
	// File is an optional path to a routes.yaml file.
	File string `json:"file"`

	// Rules are inline route entries; combined with routes loaded from File.
	Rules []tokenBrokerRoute `json:"rules"`
}

type tokenBrokerRoute struct {
	Host   string `json:"host"`
	Action string `json:"action"` // "broker" or "passthrough"; defaults to "broker"
}

func (c *tokenBrokerConfig) applyDefaults() {
	if c.DefaultPolicy == "" {
		c.DefaultPolicy = "passthrough"
	}
	// Normalize broker URL by removing trailing slash
	if c.BrokerURL != "" {
		c.BrokerURL = strings.TrimSuffix(c.BrokerURL, "/")
	}
}

func (c *tokenBrokerConfig) validate() error {
	if c.BrokerURL == "" {
		return errors.New("broker_url is required")
	}
	switch c.DefaultPolicy {
	case "broker", "passthrough":
	default:
		return fmt.Errorf("default_policy must be broker or passthrough, got %q", c.DefaultPolicy)
	}
	return nil
}

// TokenBroker performs token brokering for outbound requests.
// It acquires tokens from a token broker service based on routing rules.
type TokenBroker struct {
	cfg    tokenBrokerConfig
	client *tokenbroker.Client
	router *routing.Router
}

// NewTokenBroker constructs an unconfigured plugin.
func init() {
	RegisterPlugin("token-broker", func() pipeline.Plugin { return NewTokenBroker() })
}

func NewTokenBroker() *TokenBroker { return &TokenBroker{} }

func (p *TokenBroker) Name() string { return "token-broker" }

func (p *TokenBroker) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}

func (p *TokenBroker) Configure(raw json.RawMessage) error {
	var c tokenBrokerConfig
	var explicitRoutesFile string

	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return fmt.Errorf("token-broker config: %w", err)
		}
		// Remember if routes file was explicitly specified
		explicitRoutesFile = c.Routes.File
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return fmt.Errorf("token-broker config: %w", err)
	}

	// Build HTTP client for broker
	p.client = tokenbroker.NewClient()

	// Build router from routes
	router, err := buildBrokerRouterFrom(c.DefaultPolicy, c.Routes, c.BrokerURL, explicitRoutesFile)
	if err != nil {
		return fmt.Errorf("token-broker routes: %w", err)
	}

	// Commit configuration
	p.cfg = c
	p.router = router

	return nil
}

func (p *TokenBroker) OnRequest(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
	authHeader := pctx.Headers.Get("Authorization")
	host := pctx.Host

	// Resolve route
	resolved := p.router.Resolve(host)

	// Check if this should be a broker request
	shouldBroker := false
	if resolved != nil && !resolved.Passthrough {
		// Matched a route with action != "passthrough"
		shouldBroker = true
	} else if resolved == nil && p.cfg.DefaultPolicy == "broker" {
		// No route matched, but default policy is broker
		shouldBroker = true
	}

	if !shouldBroker {
		// Not a broker route, continue
		return pipeline.Action{Type: pipeline.Continue}
	}

	// Extract bearer token
	subjectToken := extractBearer(authHeader)
	if subjectToken == "" {
		return pipeline.DenyStatus(
			http.StatusUnauthorized,
			"auth.missing-token",
			"broker route requires authorization token",
		)
	}

	// Derive server URL from host
	serverURL := "http://" + host

	// Use the plugin's configured broker URL
	brokerURL := p.cfg.BrokerURL

	// Call broker to acquire token
	token, err := p.client.AcquireToken(ctx, brokerURL, subjectToken, serverURL)
	if err != nil {
		// Handle broker errors
		if brokerErr, ok := err.(*tokenbroker.BrokerError); ok {
			slog.Warn("token-broker: broker returned error",
				"status", brokerErr.StatusCode,
				"error", brokerErr.OAuthError,
				"description", brokerErr.OAuthDescription)
			return pipeline.DenyStatus(
				brokerErr.StatusCode,
				"upstream.broker-error",
				brokerErr.OAuthDescription,
			)
		}
		slog.Error("token-broker: broker request failed", "error", err)
		return pipeline.DenyStatus(
			http.StatusBadGateway,
			"upstream.broker-unavailable",
			err.Error(),
		)
	}

	// Replace token in authorization header
	pctx.Headers.Set("Authorization", "Bearer "+token)
	return pipeline.Action{Type: pipeline.Continue}
}

func (p *TokenBroker) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// extractBearer extracts the bearer token from an Authorization header.
func extractBearer(authHeader string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(authHeader, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(authHeader, prefix))
}

// buildBrokerRouterFrom constructs a router from the broker routes configuration.
func buildBrokerRouterFrom(defaultPolicy string, routes tokenBrokerRoutes, defaultBrokerURL string, explicitRoutesFile string) (*routing.Router, error) {
	var allRoutes []routing.Route

	// Load routes from file if specified
	if routes.File != "" {
		// If routes file was explicitly specified, check it exists
		if explicitRoutesFile != "" {
			if _, err := os.Stat(routes.File); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil, fmt.Errorf("routes file does not exist: %s", routes.File)
				}
				return nil, fmt.Errorf("checking routes file %s: %w", routes.File, err)
			}
		}

		fileRoutes, err := routing.LoadRoutes(routes.File)
		if err != nil {
			return nil, fmt.Errorf("loading routes from %s: %w", routes.File, err)
		}
		if fileRoutes != nil {
			allRoutes = append(allRoutes, fileRoutes...)
		}
	}

	// Add inline rules
	for _, r := range routes.Rules {
		action := r.Action
		if action == "" {
			action = "broker"
		}
		allRoutes = append(allRoutes, routing.Route{
			Host:   r.Host,
			Action: action,
		})
	}

	return routing.NewRouter(defaultPolicy, allRoutes)
}
