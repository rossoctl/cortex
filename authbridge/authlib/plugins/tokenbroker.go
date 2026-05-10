package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/gobwas/glob"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/tokenbroker"
	"gopkg.in/yaml.v3"
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
	Host                  string `json:"host"`
	Action                string `json:"action"` // "broker" or "passthrough"; defaults to "broker"
	AuthorizationEndpoint string `json:"authorization_endpoint,omitempty"`
	TokenEndpoint         string `json:"token_endpoint,omitempty"`
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

// brokerRouter resolves destination hosts to broker actions.
// Uses first-match-wins semantics with gobwas/glob patterns.
type brokerRouter struct {
	routes        []compiledBrokerRoute
	defaultAction string // "broker" or "passthrough"
}

type compiledBrokerRoute struct {
	pattern               string
	glob                  glob.Glob
	action                string // "broker" or "passthrough"
	authorizationEndpoint string
	tokenEndpoint         string
}

// newBrokerRouter creates a router from the given routes.
// defaultAction is "broker" or "passthrough" (applied when no route matches).
// Returns an error if any host pattern is invalid.
func newBrokerRouter(defaultAction string, rules []tokenBrokerRoute) (*brokerRouter, error) {
	if defaultAction == "" {
		defaultAction = "passthrough"
	}
	compiled := make([]compiledBrokerRoute, 0, len(rules))
	for _, r := range rules {
		// Use '.' as separator so *.example.com doesn't match foo.bar.example.com
		g, err := glob.Compile(r.Host, '.')
		if err != nil {
			return nil, fmt.Errorf("invalid route pattern %q: %w", r.Host, err)
		}
		action := r.Action
		if action == "" {
			action = "broker"
		}
		compiled = append(compiled, compiledBrokerRoute{
			pattern:               r.Host,
			glob:                  g,
			action:                action,
			authorizationEndpoint: r.AuthorizationEndpoint,
			tokenEndpoint:         r.TokenEndpoint,
		})
	}
	return &brokerRouter{routes: compiled, defaultAction: defaultAction}, nil
}

// resolve returns whether the given host should use the broker and the authorization/token endpoints if specified.
// Port is stripped from the host before matching.
// Returns (shouldBroker, authorizationEndpoint, tokenEndpoint) where shouldBroker is true if a route matches with action "broker"
// or if no route matches and default is "broker".
func (r *brokerRouter) resolve(host string) (bool, string, string) {
	// Strip port if present
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	// Check for matching route
	for _, entry := range r.routes {
		if entry.glob.Match(host) {
			return entry.action == "broker", entry.authorizationEndpoint, entry.tokenEndpoint
		}
	}

	// No route matched, use default action
	return r.defaultAction == "broker", "", ""
}

// TokenBroker performs token brokering for outbound requests.
// It acquires tokens from a token broker service based on routing rules.
type TokenBroker struct {
	cfg    tokenBrokerConfig
	client *tokenbroker.Client
	router *brokerRouter
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

	// Check if this should be a broker request and get authorization/token endpoints
	shouldBroker, authorizationEndpoint, tokenEndpoint := p.router.resolve(host)

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

	// Call broker to acquire token, passing authorization and token endpoints if available
	token, err := p.client.AcquireToken(ctx, brokerURL, subjectToken, serverURL, authorizationEndpoint, tokenEndpoint)
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

// loadBrokerRoutesFromFile loads broker routes from a YAML file.
// Returns an empty slice (not error) if the file doesn't exist.
func loadBrokerRoutesFromFile(path string) ([]tokenBrokerRoute, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading routes config: %w", err)
	}
	var routes []tokenBrokerRoute
	if err := yaml.Unmarshal(data, &routes); err != nil {
		return nil, fmt.Errorf("parsing routes config: %w", err)
	}
	return routes, nil
}

// buildBrokerRouterFrom constructs a router from the broker routes configuration.
func buildBrokerRouterFrom(defaultPolicy string, routes tokenBrokerRoutes, defaultBrokerURL string, explicitRoutesFile string) (*brokerRouter, error) {
	var allRoutes []tokenBrokerRoute

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

		fileRoutes, err := loadBrokerRoutesFromFile(routes.File)
		if err != nil {
			return nil, fmt.Errorf("loading routes from %s: %w", routes.File, err)
		}
		if fileRoutes != nil {
			allRoutes = append(allRoutes, fileRoutes...)
		}
	}

	// Add inline rules
	allRoutes = append(allRoutes, routes.Rules...)

	return newBrokerRouter(defaultPolicy, allRoutes)
}
