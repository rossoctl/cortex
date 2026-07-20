package tokenbroker

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
	"github.com/rossoctl/rossocortex/authbridge/authlib/auth"
	"github.com/rossoctl/rossocortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/rossocortex/authbridge/authlib/plugins"
	"github.com/rossoctl/rossocortex/authbridge/authlib/plugins/tokenbroker/client"
	"gopkg.in/yaml.v3"
)

// tokenBrokerConfig is the plugin's local config schema.
//
// Field tags drive both runtime decoding (json) and operator-facing
// schema introspection (description / required / default / enum).
// See pipeline/schema.go for the consumer contract.
type tokenBrokerConfig struct {
	// BrokerURL is the base URL of the token broker service.
	BrokerURL string `json:"broker_url" required:"true" description:"Base URL of the token broker service."`

	// DefaultPolicy is applied when a request's host matches no route:
	// "passthrough" (default) forwards the request unchanged;
	// "broker" attempts to acquire a token from the broker.
	DefaultPolicy string `json:"default_policy" description:"Behavior when host matches no route." default:"passthrough" enum:"passthrough,broker"`

	// Routes drives host-to-broker matching. A host that matches no
	// route falls through to DefaultPolicy.
	Routes tokenBrokerRoutes `json:"routes" description:"Host-to-broker routing rules; non-matching hosts fall through to default_policy."`
}

type tokenBrokerRoutes struct {
	// File is an optional path to a routes.yaml file.
	File string `json:"file" description:"Path to a routes.yaml file. Inline rules below are merged with file-loaded rules."`

	// Rules are inline route entries; combined with routes loaded from File.
	Rules []tokenBrokerRoute `json:"rules" description:"Inline route entries. Combined with rules loaded from file."`
}

type tokenBrokerRoute struct {
	Host                  string `json:"host" description:"Host glob pattern."`
	Action                string `json:"action" description:"broker or passthrough." default:"broker" enum:"broker,passthrough"`
	AuthorizationEndpoint string `json:"authorization_endpoint,omitempty" description:"Per-route OAuth authorization endpoint override."`
	TokenEndpoint         string `json:"token_endpoint,omitempty" description:"Per-route OAuth token endpoint override."`
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
	client *client.Client
	router *brokerRouter
}

// NewTokenBroker constructs an unconfigured plugin.
func init() {
	plugins.RegisterPlugin("token-broker", func() pipeline.Plugin { return NewTokenBroker() })
}

func NewTokenBroker() *TokenBroker { return &TokenBroker{} }

func (p *TokenBroker) Name() string { return "token-broker" }

func (p *TokenBroker) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		Description: "Token broker: exchanges incoming tokens against the configured IdP.",
	}
}

// ConfigSchema implements pipeline.SchemaProvider; surfaces field
// metadata to abctl edit templates and other config-aware tooling.
func (p *TokenBroker) ConfigSchema() []pipeline.FieldSchema {
	return pipeline.SchemaOf(tokenBrokerConfig{})
}

func (p *TokenBroker) Configure(raw json.RawMessage) error {
	var c tokenBrokerConfig

	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return fmt.Errorf("token-broker config: %w", err)
		}
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return fmt.Errorf("token-broker config: %w", err)
	}

	// Build HTTP client for broker
	p.client = client.NewClient()

	// Build router from routes
	router, err := buildBrokerRouterFrom(c.DefaultPolicy, c.Routes, c.BrokerURL)
	if err != nil {
		return fmt.Errorf("token-broker routes: %w", err)
	}

	// Commit configuration
	p.cfg = c
	p.router = router

	return nil
}

// makeBrokerDetails creates a details map for invocation recording.
// Includes broker_url, server_url, and optionally authorization_endpoint and token_endpoint.
func makeBrokerDetails(brokerURL, serverURL, authorizationEndpoint, tokenEndpoint string) map[string]string {
	details := map[string]string{
		"broker_url": brokerURL,
		"server_url": serverURL,
	}
	if authorizationEndpoint != "" {
		details["authorization_endpoint"] = authorizationEndpoint
	}
	if tokenEndpoint != "" {
		details["token_endpoint"] = tokenEndpoint
	}
	return details
}

func (p *TokenBroker) OnRequest(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
	authHeader := pctx.Headers.Get("Authorization")
	host := pctx.Host

	// Check if this should be a broker request and get authorization/token endpoints
	shouldBroker, authorizationEndpoint, tokenEndpoint := p.router.resolve(host)

	if !shouldBroker {
		// Not a broker route, continue with Skip invocation
		pctx.Skip("no_broker_route")
		return pipeline.Action{Type: pipeline.Continue}
	}

	// Derive server URL for logging
	scheme := pctx.Scheme
	if scheme == "" {
		scheme = "http"
	}
	serverURL := scheme + "://" + host

	// Use the plugin's configured broker URL
	brokerURL := p.cfg.BrokerURL

	slog.Info("token-broker: processing outbound request",
		"server_url", serverURL,
		"broker_url", brokerURL)

	subjectToken := strings.TrimSpace(auth.ExtractBearer(authHeader))
	if subjectToken == "" {
		return pctx.DenyAndRecord("missing_subject_token", "auth.missing-token",
			"broker route requires authorization token")
	}

	slog.Debug("token-broker: requesting token from broker",
		"server_url", serverURL,
		"broker_url", brokerURL,
		"authorization_endpoint", authorizationEndpoint,
		"token_endpoint", tokenEndpoint)

	// Call broker to acquire token, passing authorization and token endpoints if available
	token, err := p.client.AcquireToken(ctx, brokerURL, subjectToken, serverURL, authorizationEndpoint, tokenEndpoint)
	if err != nil {
		// Handle broker errors
		var brokerErr *client.BrokerError
		if errors.As(err, &brokerErr) {
			slog.Warn("token-broker: broker returned error",
				"status", brokerErr.StatusCode,
				"error", brokerErr.OAuthError,
				"description", brokerErr.OAuthDescription)

			details := makeBrokerDetails(brokerURL, serverURL, authorizationEndpoint, tokenEndpoint)
			details["oauth_error"] = brokerErr.OAuthError
			details["oauth_description"] = brokerErr.OAuthDescription

			pctx.Record(pipeline.Invocation{
				Action:  pipeline.ActionDeny,
				Reason:  "broker_error",
				Details: details,
			})
			return pipeline.DenyStatus(
				brokerErr.StatusCode,
				"upstream.broker-error",
				brokerErr.OAuthDescription,
			)
		}
		slog.Error("token-broker: broker request failed", "error", err)

		details := makeBrokerDetails(brokerURL, serverURL, authorizationEndpoint, tokenEndpoint)
		details["error"] = err.Error()

		pctx.Record(pipeline.Invocation{
			Action:  pipeline.ActionDeny,
			Reason:  "broker_unavailable",
			Details: details,
		})
		return pipeline.DenyStatus(
			http.StatusBadGateway,
			"upstream.broker-unavailable",
			err.Error(),
		)
	}

	// Replace token in authorization header
	pctx.Headers.Set("Authorization", "Bearer "+token)

	slog.Info("token-broker: token acquired and added to request",
		"server_url", serverURL,
		"broker_url", brokerURL)

	// Record successful token replacement
	pctx.Record(pipeline.Invocation{
		Action:  pipeline.ActionModify,
		Reason:  "token_replaced",
		Details: makeBrokerDetails(brokerURL, serverURL, authorizationEndpoint, tokenEndpoint),
	})
	return pipeline.Action{Type: pipeline.Continue}
}

func (p *TokenBroker) OnResponse(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
	// Derive server URL for logging
	scheme := pctx.Scheme
	if scheme == "" {
		scheme = "http"
	}
	serverURL := scheme + "://" + pctx.Host

	slog.Info("token-broker: received outbound response",
		"server_url", serverURL,
		"status_code", pctx.StatusCode,
		"has_response_body", len(pctx.ResponseBody) > 0)
	return pipeline.Action{Type: pipeline.Continue}
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
func buildBrokerRouterFrom(defaultPolicy string, routes tokenBrokerRoutes, defaultBrokerURL string) (*brokerRouter, error) {
	var allRoutes []tokenBrokerRoute

	// Load routes from file if specified
	if routes.File != "" {
		if _, err := os.Stat(routes.File); err != nil {
			return nil, fmt.Errorf("routes file %q: %w", routes.File, err)
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
