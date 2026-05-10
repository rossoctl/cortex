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

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/config"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/tokenexchange/cache"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/tokenexchange/exchange"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/tokenexchange/spiffe"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/routing"
)

// tokenExchangeConfig is the plugin's local config schema. See
// authbridge/docs/plugin-reference.md for the pattern.
type tokenExchangeConfig struct {
	// TokenURL is the OAuth token endpoint. Explicit value wins; else
	// derived from KeycloakURL + KeycloakRealm using Keycloak's
	// convention.
	TokenURL string `json:"token_url"`

	// KeycloakURL and KeycloakRealm are a convenience for deriving
	// TokenURL when the operator prefers to supply Keycloak base + realm
	// rather than the full token endpoint.
	KeycloakURL   string `json:"keycloak_url"`
	KeycloakRealm string `json:"keycloak_realm"`

	// DefaultPolicy is applied when a request's host matches no route:
	// "passthrough" (default) forwards the request unchanged;
	// "exchange" attempts a client-credentials exchange with an empty
	// audience (usually fails — kept for rare use cases where the IdP
	// allows it).
	DefaultPolicy string `json:"default_policy"`

	// NoTokenPolicy controls how the plugin handles outbound requests
	// that arrive without a bearer token: "client-credentials" does an
	// unprompted client_credentials exchange; "allow" forwards
	// unchanged; "deny" rejects. Default: "deny" in all modes.
	// Operators who need a different behavior (e.g. waypoint's historic
	// "allow" default) must set it explicitly per plugin entry.
	NoTokenPolicy string `json:"no_token_policy"`

	// Identity carries client credentials used for token exchange.
	Identity tokenExchangeIdentity `json:"identity"`

	// Routes drives host-to-audience matching. A host that matches no
	// route falls through to DefaultPolicy.
	Routes tokenExchangeRoutes `json:"routes"`

	// AudienceFromHost — when true, requests with no matching route use
	// routing.ServiceNameFromHost(host) as the target audience. Used in
	// waypoint mode.
	AudienceFromHost bool `json:"audience_from_host"`
}

type tokenExchangeIdentity struct {
	// Type is one of "spiffe" or "client-secret".
	Type string `json:"type"`

	// ClientID identifies the client in Keycloak. Explicit value wins;
	// else read from ClientIDFile at Configure time (or by Init if the
	// file isn't yet available).
	ClientID     string `json:"client_id"`
	ClientIDFile string `json:"client_id_file"`

	// ClientSecret / ClientSecretFile are the client-secret credentials
	// (type=client-secret).
	ClientSecret     string `json:"client_secret"`
	ClientSecretFile string `json:"client_secret_file"`

	// JWTSVIDPath is the file path where spiffe-helper writes the
	// JWT-SVID (type=spiffe).
	JWTSVIDPath string `json:"jwt_svid_path"`
}

type tokenExchangeRoutes struct {
	// File is an optional path to a routes.yaml file (see
	// authlib/routing.LoadRoutes).
	File string `json:"file"`

	// Rules are inline route entries; combined with routes loaded from
	// File.
	Rules []tokenExchangeRoute `json:"rules"`
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
	if c.TokenURL == "" && c.KeycloakURL != "" && c.KeycloakRealm != "" {
		base := strings.TrimRight(c.KeycloakURL, "/") + "/realms/" + c.KeycloakRealm
		c.TokenURL = base + "/protocol/openid-connect/token"
	}
	if c.DefaultPolicy == "" {
		c.DefaultPolicy = "passthrough"
	}
	if c.NoTokenPolicy == "" {
		c.NoTokenPolicy = auth.NoTokenPolicyDeny
	}
	// Kagenti file-system conventions for credential sources. Each
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
	case "spiffe":
		if c.Identity.ClientID == "" && c.Identity.ClientIDFile == "" {
			c.Identity.ClientIDFile = "/shared/client-id.txt"
		}
		if c.Identity.JWTSVIDPath == "" {
			c.Identity.JWTSVIDPath = "/opt/jwt_svid.token"
		}
	case "client-secret":
		if c.Identity.ClientID == "" && c.Identity.ClientIDFile == "" {
			c.Identity.ClientIDFile = "/shared/client-id.txt"
		}
		if c.Identity.ClientSecret == "" && c.Identity.ClientSecretFile == "" {
			c.Identity.ClientSecretFile = "/shared/client-secret.txt"
		}
	}
}

func (c *tokenExchangeConfig) validate() error {
	if c.TokenURL == "" {
		return errors.New("token_url is required (or set keycloak_url + keycloak_realm)")
	}
	switch c.DefaultPolicy {
	case "exchange", "passthrough":
	default:
		return fmt.Errorf("default_policy must be exchange or passthrough, got %q", c.DefaultPolicy)
	}
	switch c.NoTokenPolicy {
	case auth.NoTokenPolicyAllow, auth.NoTokenPolicyDeny, auth.NoTokenPolicyClientCredentials:
	default:
		return fmt.Errorf("no_token_policy must be allow, deny, or client-credentials, got %q", c.NoTokenPolicy)
	}
	switch c.Identity.Type {
	case "spiffe", "client-secret":
		// applyDefaults fills the identity file paths when the
		// matching inline values are empty, so no per-field check
		// here — the Configure path always ends up with a reachable
		// credential source. Configure's best-effort read logs a
		// WARN if the file isn't yet readable at boot, and Init's
		// watcher retries in the background.
	case "":
		return errors.New("identity.type is required (spiffe or client-secret)")
	default:
		return fmt.Errorf("unknown identity.type %q", c.Identity.Type)
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
	// auth.Auth.Ready() checks Identity.Audience which token-exchange
	// doesn't set (it uses ClientID), so we track readiness locally.
	ready atomic.Bool
}

// NewTokenExchange constructs an unconfigured plugin.
func NewTokenExchange() *TokenExchange { return &TokenExchange{} }

func init() {
	plugins.RegisterBuiltin("token-exchange", func() pipeline.Plugin { return NewTokenExchange() })
}

func (p *TokenExchange) Name() string { return "token-exchange" }

func (p *TokenExchange) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
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

	clientAuth, err := buildClientAuthFrom(c.Identity.Type,
		c.Identity.ClientID, c.Identity.ClientSecret, c.Identity.JWTSVIDPath)
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
	// populated ClientID already. For SPIFFE identity we also need the
	// SVID path to exist on disk (spiffe-helper has to have written it);
	// the jwt-svid source re-reads on each exchange, so existence at
	// this moment is a reasonable proxy. If the poll path is going to
	// run, ready stays false until pollCredentials flips it.
	if credentialsAreReady(c.Identity) {
		p.ready.Store(true)
	}
	return nil
}

// credentialsAreReady returns true iff the identity has everything it
// needs to do an exchange right now. Keeping this as a pure function
// lets pollCredentials and Configure share the predicate.
func credentialsAreReady(id tokenExchangeIdentity) bool {
	if id.ClientID == "" {
		return false
	}
	switch id.Type {
	case "client-secret":
		return id.ClientSecret != ""
	case "spiffe":
		// JWT-SVID source reads the file lazily; existence of ClientID
		// is the signal that client-registration has completed.
		return true
	}
	return false
}

// buildClientAuthFrom constructs an exchange.ClientAuth from explicit
// args. Used both by Configure (against the local `c`, before p.cfg is
// assigned) and by pollCredentials (which reads its credential values
// from goroutine locals, not from the immutable p.cfg). Pure function
// — no reads from the receiver.
func buildClientAuthFrom(identityType, clientID, clientSecret, jwtSVIDPath string) (exchange.ClientAuth, error) {
	switch identityType {
	case "spiffe":
		source := spiffe.NewFileJWTSource(jwtSVIDPath)
		return &exchange.JWTAssertionAuth{
			ClientID:      clientID,
			AssertionType: "urn:ietf:params:oauth:client-assertion-type:jwt-spiffe",
			TokenSource:   source.FetchToken,
		}, nil
	case "client-secret":
		return &exchange.ClientSecretAuth{
			ClientID:     clientID,
			ClientSecret: clientSecret,
		}, nil
	default:
		return nil, fmt.Errorf("unknown identity.type %q", identityType)
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
			action = "passthrough"
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
// Init's ctx bounds synchronous init only; the poller runs on a
// process-lifetime context (see bgCancel) so Pipeline.Start's 60s
// budget doesn't kill it. Shutdown cancels the poller.
func (p *TokenExchange) Init(_ context.Context) error {
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
	clientAuth, err := buildClientAuthFrom(p.cfg.Identity.Type, clientID, clientSecret, p.cfg.Identity.JWTSVIDPath)
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
	_ pipeline.Configurable = (*TokenExchange)(nil)
	_ pipeline.Initializer  = (*TokenExchange)(nil)
	_ pipeline.Shutdowner   = (*TokenExchange)(nil)
	_ pipeline.Readier      = (*TokenExchange)(nil)
	_ plugins.StatsSource   = (*TokenExchange)(nil)
)
