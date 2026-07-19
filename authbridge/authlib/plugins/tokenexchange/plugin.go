package tokenexchange

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rossoctl/rossocortex/authbridge/authlib/auth"
	"github.com/rossoctl/rossocortex/authbridge/authlib/config"
	"github.com/rossoctl/rossocortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/rossocortex/authbridge/authlib/placeholder"
	"github.com/rossoctl/rossocortex/authbridge/authlib/plugins"
	"github.com/rossoctl/rossocortex/authbridge/authlib/plugins/tokenexchange/cache"
	"github.com/rossoctl/rossocortex/authbridge/authlib/plugins/tokenexchange/exchange"
	"github.com/rossoctl/rossocortex/authbridge/authlib/routing"
	fwspiffe "github.com/rossoctl/rossocortex/authbridge/authlib/spiffe"
)

// tokenExchangeConfig is the plugin's local config schema. See
// authbridge/docs/plugin-reference.md for the pattern.
// Field tags drive both runtime decoding (json) and operator-facing
// schema introspection (description / required / default / enum).
// Inline doc comments retain long-form rationale; struct tags carry
// single-line summaries for templating. See pipeline/schema.go.
type tokenExchangeConfig struct {
	// TokenURL is the OAuth token endpoint. Explicit value always wins.
	// When empty, derived from Provider + ProviderURL + ProviderRealm
	// (or the deprecated keycloak_url + keycloak_realm pair).
	TokenURL string `json:"token_url" description:"OAuth token endpoint URL. Required unless provider + provider_url (or keycloak_url + keycloak_realm) are set for automatic derivation."`

	// Provider selects the IdP for automatic endpoint derivation.
	// Supported: keycloak, entra-id, okta, generic.
	// When empty and keycloak_url is set, defaults to "keycloak" for
	// backward compatibility. When "generic" or empty, token_url must
	// be supplied explicitly.
	Provider string `json:"provider" description:"IdP provider for endpoint derivation and client auth. Only registered providers are accepted." enum:"keycloak,generic"`

	// ProviderURL is the IdP base URL used for endpoint derivation.
	// Interpretation depends on Provider:
	//   keycloak: internal Keycloak base URL (e.g. https://keycloak.example.com)
	//   entra-id: ignored (endpoints are derived from ProviderRealm/tenant)
	//   okta:     Okta domain URL (e.g. https://dev-123.okta.com)
	ProviderURL string `json:"provider_url" description:"IdP base URL for endpoint derivation. Interpretation varies by provider."`

	// ProviderRealm is the IdP-specific namespace/tenant:
	//   keycloak: realm name (e.g. rossoctl)
	//   entra-id: tenant ID or domain (e.g. contoso.onmicrosoft.com)
	//   okta:     authorization server ID (optional, omit for org-level)
	ProviderRealm string `json:"provider_realm" description:"IdP-specific realm/tenant/auth-server. Keycloak: realm name. Entra ID: tenant ID. Okta: auth server ID (optional)."`

	// KeycloakURL and KeycloakRealm are deprecated aliases for
	// ProviderURL and ProviderRealm with Provider="keycloak".
	// Supported for backward compatibility; prefer provider_url +
	// provider_realm + provider instead.
	KeycloakURL   string `json:"keycloak_url" description:"DEPRECATED: use provider_url + provider=keycloak instead. Internal Keycloak base URL."`
	KeycloakRealm string `json:"keycloak_realm" description:"DEPRECATED: use provider_realm + provider=keycloak instead. Keycloak realm name."`

	// DefaultPolicy is applied when a request's host matches no route:
	// "passthrough" (default) forwards the request unchanged;
	// "exchange" attempts a client-credentials exchange with an empty
	// audience (usually fails — kept for rare use cases where the IdP
	// allows it).
	DefaultPolicy string `json:"default_policy" description:"Behavior when host matches no route: passthrough forwards unchanged, exchange attempts empty-audience client-credentials." default:"passthrough" enum:"passthrough,exchange"`

	// NoTokenPolicy controls how the plugin handles outbound requests
	// that arrive without a bearer token: "client-credentials" does an
	// unprompted client_credentials exchange; "allow" forwards
	// unchanged; "deny" rejects. Default: "deny" in all modes.
	// Operators who need a different behavior (e.g. waypoint's historic
	// "allow" default) must set it explicitly per plugin entry.
	NoTokenPolicy string `json:"no_token_policy" description:"Behavior when outbound has no bearer token: client-credentials, allow, or deny." default:"deny" enum:"client-credentials,allow,deny"`

	// Identity carries client credentials used for token exchange.
	Identity tokenExchangeIdentity `json:"identity" description:"Client credentials used for token exchange (spiffe or client-secret)."`

	// Routes drives host-to-audience matching. A host that matches no
	// route falls through to DefaultPolicy.
	Routes tokenExchangeRoutes `json:"routes" description:"Host-to-audience routing rules; non-matching hosts fall through to default_policy."`

	// AudienceFromHost — when true, requests with no matching route use
	// routing.ServiceNameFromHost(host) as the target audience. Used in
	// waypoint mode.
	AudienceFromHost bool `json:"audience_from_host" description:"When true, derive audience from host for unrouted requests (waypoint mode)." default:"false"`

	ResolvePlaceholders bool `json:"resolve_placeholders" default:"false" description:"Resolve an inbound bearer carrying the placeholder prefix from the shared store to the real token before exchange. Unresolvable placeholders are denied (fail closed)."`
}

type tokenExchangeIdentity struct {
	// Type is one of "spiffe" or "client-secret".
	Type string `json:"type" required:"true" description:"Identity scheme: spiffe (JWT-SVID assertion) or client-secret." enum:"spiffe,client-secret"`

	// ClientID identifies the OAuth client at the IdP. Explicit value
	// wins; else read from ClientIDFile at Configure time (or by Init
	// if the file isn't yet available).
	ClientID     string `json:"client_id" description:"OAuth client ID. One of client_id or client_id_file is required."`
	ClientIDFile string `json:"client_id_file" description:"Read client ID from this file. Default: /shared/client-id.txt."`

	// ClientSecret / ClientSecretFile are the client-secret credentials
	// (type=client-secret).
	ClientSecret     string `json:"client_secret" description:"OAuth client secret (type=client-secret)."`
	ClientSecretFile string `json:"client_secret_file" description:"Read client secret from file. Default: /shared/client-secret.txt."`

	// JWTAudience is the audience claim minted on the JWT-SVID used as
	// the RFC 8693 client assertion. Required when Type=="spiffe";
	// ignored otherwise.
	JWTAudience string `json:"jwt_audience" description:"Audience claim minted on the JWT-SVID assertion. REQUIRED when type=spiffe; ignored otherwise."`

	// AssertionType is the client_assertion_type URN used with spiffe
	// identity. Default: urn:ietf:params:oauth:client-assertion-type:jwt-spiffe
	// (supported by Keycloak). Set to jwt-bearer for Okta compatibility.
	AssertionType string `json:"assertion_type" description:"Client assertion type URN for spiffe identity. Default: jwt-spiffe. Use jwt-bearer for Okta." enum:"jwt-spiffe,jwt-bearer"`
}

type tokenExchangeRoutes struct {
	// File is an optional path to a routes.yaml file (see
	// authlib/routing.LoadRoutes).
	File string `json:"file" description:"Path to a routes.yaml file. Default: /etc/authproxy/routes.yaml."`

	// Rules are inline route entries; combined with routes loaded from
	// File.
	Rules []tokenExchangeRoute `json:"rules" description:"Inline route entries. Combined with rules loaded from file."`
}

type tokenExchangeRoute struct {
	Host           string `json:"host"`
	TargetAudience string `json:"target_audience"`
	TokenScopes    string `json:"token_scopes"`
	TokenURL       string `json:"token_url"`
	Action         string `json:"action"`
	// Passthrough is accepted for backwards compatibility with the
	// pre-migration routes.yaml shape (Action:"passthrough" is preferred).
	Passthrough bool `json:"passthrough"`
}

func (c *tokenExchangeConfig) applyDefaults() {
	// Backward compatibility: migrate keycloak_url/keycloak_realm to
	// provider_url/provider_realm when the new fields are empty.
	// Also infer provider=keycloak when any deprecated keycloak_* field
	// is present (handles mixed legacy/new paths).
	if c.KeycloakURL != "" || c.KeycloakRealm != "" {
		if c.Provider == "" {
			c.Provider = DefaultProvider
		}
	}
	if c.KeycloakURL != "" && c.ProviderURL == "" {
		c.ProviderURL = c.KeycloakURL
		slog.Warn("token-exchange: keycloak_url is deprecated; use provider_url + provider=keycloak instead")
	}
	if c.KeycloakRealm != "" && c.ProviderRealm == "" {
		c.ProviderRealm = c.KeycloakRealm
		slog.Warn("token-exchange: keycloak_realm is deprecated; use provider_realm + provider=keycloak instead")
	}

	// Derive token_url and default assertion_type from the registered
	// IdP provider when not set explicitly.
	if c.Provider != "" {
		if p := LookupProvider(c.Provider); p != nil {
			if c.TokenURL == "" {
				c.TokenURL = p.TokenEndpoint(c.ProviderURL, c.ProviderRealm)
			}
			if c.Identity.AssertionType == "" && c.Identity.Type == SpiffeIdentity {
				c.Identity.AssertionType = p.DefaultAssertionType()
			}
		}
	}
	if c.DefaultPolicy == "" {
		c.DefaultPolicy = PassthroughPolicy
	}
	if c.NoTokenPolicy == "" {
		c.NoTokenPolicy = auth.NoTokenPolicyDeny
	}
	// Rossoctl file-system conventions for credential sources. Each
	// default kicks in only when the matching inline value is also
	// empty, so operators who supply inline credentials are never
	// surprised by a file read.
	//
	// The route file default is safe because routing.LoadRoutes
	// returns (nil, nil) when the file doesn't exist — missing routes
	// means "no inline rules," which is the correct behavior for
	// deployments without a mounted authproxy-routes ConfigMap.
	if c.Routes.File == "" {
		c.Routes.File = "/etc/authproxy/routes.yaml"
	}
	switch c.Identity.Type {
	case SpiffeIdentity:
		if c.Identity.ClientID == "" && c.Identity.ClientIDFile == "" {
			c.Identity.ClientIDFile = "/shared/client-id.txt"
		}
		// JWT-SVID source is injected via the framework SPIFFE provider
		// (T11) rather than read from a per-plugin file path.
	case ClientSecretIdentity:
		if c.Identity.ClientID == "" && c.Identity.ClientIDFile == "" {
			c.Identity.ClientIDFile = "/shared/client-id.txt"
		}
		if c.Identity.ClientSecret == "" && c.Identity.ClientSecretFile == "" {
			c.Identity.ClientSecretFile = "/shared/client-secret.txt"
		}
	}
}

func (c *tokenExchangeConfig) validate() error {
	// Reject unknown provider names early.
	if c.Provider != "" && c.Provider != GenericProvider {
		if LookupProvider(c.Provider) == nil {
			return fmt.Errorf("provider %q is not registered (available providers are registered via init())", c.Provider)
		}
	}
	if c.TokenURL == "" {
		return errors.New("token_url is required (or set provider + provider_url + provider_realm)")
	}
	switch c.DefaultPolicy {
	case ExchangePolicy, PassthroughPolicy:
	default:
		return fmt.Errorf("default_policy must be %s or %s, got %q", ExchangePolicy, PassthroughPolicy, c.DefaultPolicy)
	}
	switch c.NoTokenPolicy {
	case auth.NoTokenPolicyAllow, auth.NoTokenPolicyDeny, auth.NoTokenPolicyClientCredentials:
	default:
		return fmt.Errorf("no_token_policy must be allow, deny, or client-credentials, got %q", c.NoTokenPolicy)
	}
	switch c.Identity.Type {
	case SpiffeIdentity:
		// applyDefaults fills the identity file paths when the
		// matching inline values are empty, so no per-field check
		// for client_id here — Configure's best-effort read logs a
		// WARN if the file isn't yet readable at boot, and Init's
		// watcher retries in the background.
		//
		// jwt_audience is required for the spiffe identity path: the
		// framework JWT-SVID source needs an audience to mint the
		// client-assertion JWT for. Catching the missing value here
		// gives operators a clear startup error rather than a runtime
		// failure on the first outbound exchange.
		if c.Identity.JWTAudience == "" {
			return errors.New("tokenexchange: identity.type=spiffe requires identity.jwt_audience to be set")
		}
		// Reject invalid assertion_type (non-empty but unknown).
		if c.Identity.AssertionType != "" {
			if _, ok := AssertionTypeURN[c.Identity.AssertionType]; !ok {
				return fmt.Errorf("tokenexchange: unknown assertion_type %q (supported: %s, %s)", c.Identity.AssertionType, JWTSpiffeAssertion, JWTBearerAssertion)
			}
		}
	case ClientSecretIdentity:
		// applyDefaults fills the identity file paths when the
		// matching inline values are empty.
	case "":
		return fmt.Errorf("identity.type is required (%s or %s)", SpiffeIdentity, ClientSecretIdentity)
	default:
		return fmt.Errorf("unknown identity.type %q", c.Identity.Type)
	}
	// Validate provider↔identity compatibility when a provider is set.
	if c.Provider != "" && c.Provider != GenericProvider {
		if p := LookupProvider(c.Provider); p != nil {
			supported := p.SupportedIdentityTypes()
			found := false
			for _, s := range supported {
				if s == c.Identity.Type {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("provider %q does not support identity.type=%q (supported: %v)", c.Provider, c.Identity.Type, supported)
			}
		}
	}
	return nil
}

// TokenExchange performs outbound token exchange. Configure builds the
// internal exchanger / router / auth handler; Init polls for credential
// files that weren't available at Configure time and swaps them in via
// auth.UpdateIdentity.
//
// cfg is immutable after Configure returns. Background goroutines
// read credential values into locals and feed them through
// auth.UpdateIdentity rather than mutating cfg, so OnRequest callers
// can safely read p.cfg without synchronization.
type TokenExchange struct {
	cfg   tokenExchangeConfig
	inner *auth.Auth

	// bgCancel stops the background credential-file poller started by
	// Init. See JWTValidation.bgCancel for why this can't be tied to
	// Init's ctx, and for why it lives in an atomic.Pointer.
	bgCancel atomic.Pointer[context.CancelFunc]

	// ready flips true when credentials are available — either because
	// the synchronous read in Configure succeeded, because the operator
	// supplied inline credentials, or because pollCredentials finished.
	// auth.Auth.Ready() checks Identity.Audiences which token-exchange
	// doesn't set (it uses ClientID), so we track readiness locally.
	ready atomic.Bool

	// provider is the framework SPIFFE provider injected via
	// SetSPIFFEProvider before Configure runs (see plugins.BuildWithSPIFFE).
	// Nil when SPIRE is disabled or no provider was wired in. Used by
	// Configure / pollCredentials when identity.type=spiffe to obtain a
	// JWTSource for client-assertion JWTs.
	provider *fwspiffe.Provider

	// testJWTSource is a test-only override that bypasses provider. Used
	// by package-internal tests to exercise the spiffe identity wiring
	// without spinning up a real SPIRE socket. nil in production.
	testJWTSource fwspiffe.JWTSource
}

// SetSPIFFEProvider implements fwspiffe.ProviderConsumer. The framework's
// plugins.BuildWithSPIFFE calls this before Configure runs. The Provider
// is consulted by Configure / pollCredentials when identity.type=spiffe;
// other identity types ignore it.
func (p *TokenExchange) SetSPIFFEProvider(prov *fwspiffe.Provider) {
	p.provider = prov
}

// jwtSource returns the framework JWTSource to use for spiffe identity,
// bound to the supplied audience: testJWTSource (test-only) overrides;
// otherwise provider.JWTSource(audience) opens (lazily) the SDK JWT
// client and binds an adapter to the audience. Returns nil when neither
// is available.
//
// Errors from the SDK are logged at WARN and surfaced as nil — the
// caller treats nil as "no JWT source" and either fails Configure
// (unconfigured spiffe identity) or stays not-ready (poll path).
func (p *TokenExchange) jwtSource(audience string) fwspiffe.JWTSource {
	if p.testJWTSource != nil {
		return p.testJWTSource
	}
	if p.provider == nil {
		return nil
	}
	src, err := p.provider.JWTSource(audience)
	if err != nil {
		slog.Warn("token-exchange: provider.JWTSource failed", "audience", audience, "error", err)
		return nil
	}
	return src
}

// NewTokenExchange constructs an unconfigured plugin.
func NewTokenExchange() *TokenExchange { return &TokenExchange{} }

func init() {
	plugins.RegisterPlugin("token-exchange", func() pipeline.Plugin { return NewTokenExchange() })
}

func (p *TokenExchange) Name() string { return "token-exchange" }

func (p *TokenExchange) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		Description: "RFC 8693 outbound token exchange per route. Supports Keycloak, Entra ID, Okta, and any RFC 8693-compliant IdP.",
	}
}

// ConfigSchema implements pipeline.SchemaProvider; surfaces field
// metadata to abctl edit templates and other config-aware tooling.
func (p *TokenExchange) ConfigSchema() []pipeline.FieldSchema {
	return pipeline.SchemaOf(tokenExchangeConfig{})
}

func (p *TokenExchange) Configure(raw json.RawMessage) error {
	var c tokenExchangeConfig
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return fmt.Errorf("token-exchange config: %w", err)
		}
	}
	// Track whether NoTokenPolicy arrived explicitly so we can warn
	// waypoint-ish operators whose pre-migration deployments relied on
	// the old mode-dependent default (waypoint=allow). applyDefaults
	// fills in "deny" for everyone; without the explicit-set signal we
	// can't tell the two cases apart at WARN time.
	noTokenPolicyExplicit := c.NoTokenPolicy != ""
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return fmt.Errorf("token-exchange config: %w", err)
	}
	// The old NoTokenPolicyForMode defaulted to "client-credentials" for
	// envoy-sidecar, "allow" for waypoint, and "deny" for proxy-sidecar.
	// The new uniform default is "deny". Warn whenever no_token_policy
	// was defaulted so operators relying on either of the old
	// mode-specific behaviors find out at boot rather than via a
	// traffic regression.
	if !noTokenPolicyExplicit {
		slog.Warn("token-exchange: no_token_policy defaulted to \"deny\"; " +
			"prior defaults were mode-specific (envoy-sidecar: client-credentials, waypoint: allow, proxy-sidecar: deny). " +
			"Set no_token_policy explicitly (allow | deny | client-credentials) to silence this warning and pin the behavior.")
	}

	// Everything below runs against the local `c`, never `p.cfg`, so a
	// partially-constructed failure leaves the plugin in its zero
	// state. The final p.cfg / p.inner assignments happen only after
	// all fallible construction has succeeded.
	//
	// Best-effort synchronous credential load. Missing files are
	// tolerated; Init will retry. Log a boot-time WARN for each file
	// that isn't yet readable so operators notice misconfiguration
	// (wrong path, missing ConfigMap mount) in `kubectl logs` of the
	// initial pod rather than discovering it later via 503s.
	if c.Identity.ClientID == "" && c.Identity.ClientIDFile != "" {
		if v, err := config.ReadCredentialFile(c.Identity.ClientIDFile); err == nil {
			c.Identity.ClientID = v
		} else {
			slog.Warn("token-exchange: client_id_file not yet readable; Init will poll in background",
				"path", c.Identity.ClientIDFile, "error", err)
		}
	}
	if c.Identity.ClientSecret == "" && c.Identity.ClientSecretFile != "" {
		if v, err := config.ReadCredentialFile(c.Identity.ClientSecretFile); err == nil {
			c.Identity.ClientSecret = v
		} else {
			slog.Warn("token-exchange: client_secret_file not yet readable; Init will poll in background",
				"path", c.Identity.ClientSecretFile, "error", err)
		}
	}

	jwtSrc := p.jwtSource(c.Identity.JWTAudience)
	clientAuth, err := buildClientAuth(c.Provider, c.Identity, jwtSrc)
	if err != nil {
		return fmt.Errorf("token-exchange: %w", err)
	}

	exchanger := exchange.NewClient(c.TokenURL, clientAuth)

	router, err := buildRouterFrom(c.DefaultPolicy, c.Routes)
	if err != nil {
		return fmt.Errorf("token-exchange routes: %w", err)
	}

	authCfg := auth.Config{
		Verifier:      nil, // token-exchange doesn't validate inbound
		Exchanger:     exchanger,
		Cache:         cache.New(),
		Router:        router,
		Identity:      auth.IdentityConfig{ClientID: c.Identity.ClientID},
		NoTokenPolicy: c.NoTokenPolicy,
	}
	if c.AudienceFromHost {
		authCfg.AudienceDeriver = routing.ServiceNameFromHost
	}
	// Commit. p.cfg + p.inner become visible atomically (from the
	// caller's point of view — Configure itself is serialized by
	// pipeline.Build).
	p.cfg = c
	p.inner = auth.New(authCfg)

	// Readiness: the synchronous credential load in Configure may have
	// populated ClientID already. For SPIFFE identity we also need a
	// JWTSource from the injected Provider; the source itself caches
	// and refreshes the SVID transparently. If the poll path is going
	// to run, ready stays false until pollCredentials flips it.
	if credentialsAreReady(c.Identity, jwtSrc) {
		p.ready.Store(true)
	}
	return nil
}

// credentialsAreReady returns true iff the identity has everything it
// needs to do an exchange right now. Keeping this as a pure function
// lets pollCredentials and Configure share the predicate.
func credentialsAreReady(id tokenExchangeIdentity, jwtSrc fwspiffe.JWTSource) bool {
	if id.ClientID == "" {
		return false
	}
	switch id.Type {
	case ClientSecretIdentity:
		return id.ClientSecret != ""
	case SpiffeIdentity:
		// SPIFFE identity is ready iff a JWTSource was injected (via the
		// framework Provider) and the operator's Secret mount has
		// supplied the client_id.
		return jwtSrc != nil
	}
	return false
}

// buildClientAuth delegates client auth construction to the registered
//
// The "spiffe" identity path requires a non-nil JWTSource — supplied by
// the framework spiffe.Provider via SetSPIFFEProvider (see
// plugins.BuildWithSPIFFE). When the provider hasn't been wired in,
// this returns an explicit configuration error rather than panicking.
// IdP provider. When no provider is set (generic/explicit token_url),
// falls back to a default implementation supporting client-secret and
// spiffe with jwt-spiffe assertion type.
func buildClientAuth(providerName string, identity tokenExchangeIdentity, jwtSrc fwspiffe.JWTSource) (exchange.ClientAuth, error) {
	id := IdentityConfig{
		Type:          identity.Type,
		ClientID:      identity.ClientID,
		ClientSecret:  identity.ClientSecret,
		AssertionType: identity.AssertionType,
		JWTAudience:   identity.JWTAudience,
	}
	if p := LookupProvider(providerName); p != nil {
		return p.BuildClientAuth(id, jwtSrc)
	}
	// Fallback for generic/empty provider — basic client-secret and
	// spiffe with default jwt-spiffe assertion.
	switch identity.Type {
	case SpiffeIdentity:
		if jwtSrc == nil {
			return nil, errors.New("spiffe identity requires a SPIFFE provider to be injected")
		}
		assertionType := identity.AssertionType
		if assertionType == "" {
			assertionType = DefaultAssertion
		}
		urn, ok := AssertionTypeURN[assertionType]
		if !ok {
			return nil, fmt.Errorf("unknown assertion_type %q", assertionType)
		}
		return &exchange.JWTAssertionAuth{
			ClientID:      identity.ClientID,
			AssertionType: urn,
			TokenSource:   jwtSrc.FetchToken,
		}, nil
	case ClientSecretIdentity:
		return &exchange.ClientSecretAuth{
			ClientID:     identity.ClientID,
			ClientSecret: identity.ClientSecret,
		}, nil
	default:
		return nil, fmt.Errorf("unknown identity.type %q", identity.Type)
	}
}

// buildRouterFrom is pure — no reads from the receiver. Used from
// Configure before p.cfg is assigned, so a build failure leaves the
// plugin in its zero state.
func buildRouterFrom(defaultPolicy string, routes tokenExchangeRoutes) (*routing.Router, error) {
	var rules []routing.Route
	if routes.File != "" {
		fileRoutes, err := routing.LoadRoutes(routes.File)
		if err != nil {
			return nil, err
		}
		rules = append(rules, fileRoutes...)
	}
	for _, rc := range routes.Rules {
		action := rc.Action
		if action == "" && rc.Passthrough {
			action = PassthroughPolicy
		}
		rules = append(rules, routing.Route{
			Host:          rc.Host,
			Audience:      rc.TargetAudience,
			Scopes:        rc.TokenScopes,
			TokenEndpoint: rc.TokenURL,
			Action:        action,
		})
	}
	return routing.NewRouter(defaultPolicy, rules)
}

// Init polls for credential files that weren't available during
// Configure. When both client_id and client_secret (or jwt_svid) become
// available, it builds a fresh client-auth and calls UpdateIdentity so
// in-flight exchanges pick up the new credentials.
//
// For spiffe identity, Init also asks the framework Provider to mirror
// the JWT-SVID for the configured audience to disk (no-op when the
// provider has MirrorFiles=false). The mirror file is used by external
// readers (debug shells, e2e probes, future Envoy filesystem SDS); the
// in-memory FetchToken path is the source of truth for the hot path.
//
// Init's ctx bounds synchronous init only; the poller runs on a
// process-lifetime context (see bgCancel) so Pipeline.Start's 60s
// budget doesn't kill it. Shutdown cancels the poller.
func (p *TokenExchange) Init(ctx context.Context) error {
	if p.cfg.Identity.Type == SpiffeIdentity && p.provider != nil && p.cfg.Identity.JWTAudience != "" {
		if err := p.provider.MirrorJWT(ctx, p.cfg.Identity.JWTAudience); err != nil {
			// Mirror failures are non-fatal: the in-memory
			// JWTSource keeps working even if the file mirror
			// can't be set up. Log loud enough that operators
			// notice.
			slog.Warn("token-exchange: failed to start JWT-SVID mirror",
				"audience", p.cfg.Identity.JWTAudience, "error", err)
		}
	}

	needID := p.cfg.Identity.ClientID == "" && p.cfg.Identity.ClientIDFile != ""
	needSecret := p.cfg.Identity.ClientSecret == "" && p.cfg.Identity.ClientSecretFile != ""
	if !needID && !needSecret {
		return nil
	}
	// Defensive guard; see JWTValidation.Init for rationale.
	if p.bgCancel.Load() != nil {
		return nil
	}
	bgCtx, cancel := context.WithCancel(context.Background())
	p.bgCancel.Store(&cancel)
	go p.pollCredentials(bgCtx, needID, needSecret)
	return nil
}

// pollCredentials reads credential files into local variables and
// applies them via auth.UpdateIdentity. It does not mutate p.cfg —
// keeping cfg immutable after Configure lets OnRequest and future
// readers access p.cfg without synchronization.
func (p *TokenExchange) pollCredentials(ctx context.Context, needID, needSecret bool) {
	clientID := p.cfg.Identity.ClientID
	clientSecret := p.cfg.Identity.ClientSecret
	if needID {
		v, err := config.WaitForCredentialFile(ctx, p.cfg.Identity.ClientIDFile)
		if err != nil {
			slog.Debug("token-exchange: client_id_file wait stopped",
				"path", p.cfg.Identity.ClientIDFile, "error", err)
			return
		}
		clientID = v
	}
	if needSecret {
		v, err := config.WaitForCredentialFile(ctx, p.cfg.Identity.ClientSecretFile)
		if err != nil {
			slog.Debug("token-exchange: client_secret_file wait stopped",
				"path", p.cfg.Identity.ClientSecretFile, "error", err)
			return
		}
		clientSecret = v
	}
	pollIdentity := p.cfg.Identity
	pollIdentity.ClientID = clientID
	pollIdentity.ClientSecret = clientSecret
	clientAuth, err := buildClientAuth(p.cfg.Provider, pollIdentity, p.jwtSource(p.cfg.Identity.JWTAudience))
	if err != nil {
		slog.Warn("token-exchange: failed to rebuild client auth after credential load", "error", err)
		return
	}
	p.inner.UpdateIdentity(
		auth.IdentityConfig{ClientID: clientID},
		clientAuth,
	)
	p.ready.Store(true)
	// Deliberately log no client_id: some operators treat OAuth client
	// IDs as sensitive (they appear in access logs, JWT sub claims,
	// etc.). The signal here — credentials have loaded — doesn't need
	// the identifier.
	slog.Info("token-exchange: credentials loaded")
}

// Shutdown cancels the background credential-file poller if one was
// started by Init. Called by Pipeline.Stop during process shutdown.
// Safe to call more than once.
func (p *TokenExchange) Shutdown(_ context.Context) error {
	if cancel := p.bgCancel.Swap(nil); cancel != nil {
		(*cancel)()
	}
	return nil
}

func (p *TokenExchange) OnRequest(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
	if p.inner == nil {
		return pipeline.DenyStatus(503, "upstream.unreachable", "token-exchange not configured")
	}
	authHeader := pctx.Headers.Get("Authorization")
	host := pctx.Host

	if p.cfg.ResolvePlaceholders && placeholder.IsPlaceholder(auth.ExtractBearer(authHeader)) {
		real, ok := resolvePlaceholder(pctx, auth.ExtractBearer(authHeader))
		if !ok {
			return pctx.DenyAndRecord("placeholder_unresolved", "auth.unauthorized", "unresolvable credential placeholder")
		}
		authHeader = "Bearer " + real
	}

	result := p.inner.HandleOutbound(ctx, authHeader, host)
	// Record an Auth.Outbound entry on every branch so operators have
	// full outbound audit in the session stream — matches the inbound
	// side's recording of allow/deny/bypass and mirrors the claim in the
	// PLUGIN column that every event is attributable to a plugin.
	// Passthrough is the "no route matched, default policy allowed"
	// branch and is the noisiest; operators who find it too loud can
	// either tighten routes or filter on action=passthrough in abctl.
	switch result.Action {
	case auth.ActionDeny:
		pctx.Record(pipeline.Invocation{
			Action: pipeline.ActionDeny,
			Reason: result.DenyReasonCode.String(),
			Details: map[string]string{
				"route_matched":    boolStr(result.RouteMatched),
				"route_host":       host,
				"target_audience":  result.TargetAudience,
				"requested_scopes": result.RequestedScopes,
			},
		})
		// Outbound denials almost always come from failed token exchange
		// at the IdP (upstream unreachable, bad credentials, audience
		// refused). The auth layer returns the HTTP status it wants to
		// expose; pick the closest well-known code for the body.
		code := "upstream.token-exchange-failed"
		if result.DenyStatus == http.StatusForbidden {
			code = "policy.forbidden"
		}
		return pipeline.DenyStatus(result.DenyStatus, code, result.DenyReason)
	case auth.ActionReplaceToken:
		pctx.Headers.Set("Authorization", "Bearer "+result.Token)
		recordDelegationHop(pctx, result)
		reason := "token_replaced"
		if result.CacheHit {
			reason = "cache_hit"
		}
		pctx.Record(pipeline.Invocation{
			Action: pipeline.ActionModify,
			Reason: reason,
			Details: map[string]string{
				"route_matched":    "true",
				"route_host":       host,
				"target_audience":  result.TargetAudience,
				"requested_scopes": result.RequestedScopes,
				"cache_hit":        boolStr(result.CacheHit),
			},
		})
	default:
		// ActionAllow / unroutable host / default-policy=passthrough all
		// land here. Reason discriminates explicit-route-passthrough from
		// no-route-match-default-policy; both render as "skip" in the
		// 5-value vocab.
		reason := "no_matching_route"
		if result.RouteMatched {
			reason = "route_passthrough"
		}
		pctx.Record(pipeline.Invocation{
			Action: pipeline.ActionSkip,
			Reason: reason,
			Details: map[string]string{
				"route_matched": boolStr(result.RouteMatched),
				"route_host":    host,
			},
		})
	}
	return pipeline.Action{Type: pipeline.Continue}
}

// resolvePlaceholder looks a handle up in the shared store. Returns ok=false
// (fail closed) when no store is wired or the handle is unknown/expired.
func resolvePlaceholder(pctx *pipeline.Context, handle string) (string, bool) {
	if pctx.Shared == nil {
		return "", false
	}
	v, ok := pctx.Shared.Get(placeholder.Key(handle))
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// boolStr renders a boolean as "true" / "false" for Invocation.Details.
// Kept as a small helper rather than inlining so both the deny and
// modify branches use the same string form and abctl's filter
// matching is predictable.
func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// recordDelegationHop appends a delegation hop to pctx.Extensions.Delegation
// whenever token-exchange mints (or cache-serves) a downstream token. This
// is AuthBridge's own auth plugin establishing delegation provenance — the
// chain is then forwarded into policy context (CPEX's DelegationExtension)
// and surfaced on the session API, giving downstream policy a record of
// what audience the agent's token was exchanged for, with which scopes, and
// whether it came from cache.
//
// SubjectID is best-effort: the forward-proxy outbound pctx generally has
// no validated Identity (JWT validation runs on the separate inbound pass),
// so it's populated only when an Identity happens to be present. The
// audience / scopes / strategy / from-cache fields are always known from
// the exchange result and carry the bulk of the signal regardless.
func recordDelegationHop(pctx *pipeline.Context, result *auth.OutboundResult) {
	if pctx.Extensions.Delegation == nil {
		pctx.Extensions.Delegation = &pipeline.DelegationExtension{}
	}
	subject := ""
	if pctx.Identity != nil {
		subject = pctx.Identity.Subject()
	}
	pctx.Extensions.Delegation.AppendHop(pipeline.DelegationHop{
		SubjectID: subject,
		Scopes:    splitScopes(result.RequestedScopes),
		Timestamp: time.Now(),
		Audience:  result.TargetAudience,
		Strategy:  "token-exchange",
		FromCache: result.CacheHit,
	})
}

// splitScopes splits a raw space-separated OAuth scope string into a
// slice, dropping empty fields. Returns nil (not an empty slice) for an
// unscoped exchange so the hop's ScopesGranted stays absent rather than
// an empty list.
func splitScopes(raw string) []string {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return nil
	}
	return fields
}

func (p *TokenExchange) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// Ready reports whether client credentials are available for
// exchange. Flips true either at Configure time (if the synchronous
// credential read succeeded or the operator supplied inline values)
// or at pollCredentials success. Used by the pipeline-level /readyz
// aggregator so the kubelet holds traffic off the pod until
// credentials land.
func (p *TokenExchange) Ready() bool {
	return p.ready.Load()
}

// Stats returns the plugin's counter store for the /stats aggregator
// (see plugins.CollectStats). Returns nil when Configure hasn't run
// yet — aggregation code tolerates nils.
func (p *TokenExchange) Stats() *auth.Stats {
	if p.inner == nil {
		return nil
	}
	return p.inner.Stats
}

// Compile-time interface checks.
var (
	_ pipeline.Configurable     = (*TokenExchange)(nil)
	_ pipeline.Initializer      = (*TokenExchange)(nil)
	_ pipeline.Shutdowner       = (*TokenExchange)(nil)
	_ pipeline.Readier          = (*TokenExchange)(nil)
	_ plugins.StatsSource       = (*TokenExchange)(nil)
	_ fwspiffe.ProviderConsumer = (*TokenExchange)(nil)
)
