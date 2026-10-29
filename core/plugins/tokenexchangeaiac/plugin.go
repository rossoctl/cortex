// Package tokenexchangeaiac is an outbound token-exchange plugin that needs no
// routes. It derives a target audience from the destination host and lets the
// IdP decide what that destination is: a token comes back for a target this
// caller is granted, "no such client" means the destination is not a target and
// the request passes through, and every other answer denies.
//
// It shares no exchange code with token-exchange, by design: the two are
// alternatives, and a fix to one must not silently change the other. Both read
// the bearer and name their no-token policies through core/auth, so a request
// carries the same meaning under either. See docs/token-exchange-aiac-plugin.md.
package tokenexchangeaiac

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
	"sync/atomic"
	"time"

	"github.com/rossoctl/cortex/core/auth"
	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/placeholder"
	"github.com/rossoctl/cortex/core/plugins"
)

// PluginName is the name the plugin registers under and is configured by.
const PluginName = "token-exchange-aiac"

// Invocation reasons, machine-stable.
const (
	reasonExchanged         = "token_exchanged"
	reasonCacheHit          = "cache_hit"
	reasonAudienceUnknown   = "audience_unknown"
	reasonNoToken           = "no_token"
	reasonNotGranted        = "audience_not_granted"
	reasonTargetDisabled    = "target_disabled"
	reasonSubjectInvalid    = "subject_invalid"
	reasonForeignNamespace  = "foreign_namespace"
	reasonExchangeFailed    = "exchange_failed"
	reasonCredentialsAbsent = "credentials_not_ready"
	reasonOpaqueTunnel      = "opaque_tunnel"

	reasonPlaceholderUnresolved = "placeholder_unresolved"
)

// serviceAccountNamespaceFile is where the kubelet projects the pod's own
// namespace. Read when neither namespace nor namespace_file is set and the file
// exists, so an in-cluster sidecar knows its namespace without being told.
var serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// selfTokenKey caches the caller's own client-credentials token. It cannot
// collide with a tokenKey, which always starts with 64 hex digits.
const selfTokenKey = "\x00self"

type tokenExchangeAIACConfig struct {
	// TokenURL is the OAuth token endpoint. When empty it is derived from
	// KeycloakURL and KeycloakRealm.
	TokenURL      string `json:"token_url" description:"OAuth token endpoint. Required unless keycloak_url and keycloak_realm are set."`
	KeycloakURL   string `json:"keycloak_url" description:"Keycloak base URL (internal service URL), used to derive token_url."`
	KeycloakRealm string `json:"keycloak_realm" description:"Keycloak realm, used to derive token_url."`

	ClientID         string `json:"client_id" description:"This workload's OAuth client ID. Overrides client_id_file."`
	ClientIDFile     string `json:"client_id_file" description:"File holding the client ID." default:"/shared/client-id.txt"`
	ClientSecret     string `json:"client_secret" description:"This workload's OAuth client secret. Overrides client_secret_file."`
	ClientSecretFile string `json:"client_secret_file" description:"File holding the client secret." default:"/shared/client-secret.txt"`

	// AudienceTemplate maps a destination to the audience requested for it.
	AudienceTemplate string `json:"audience_template" description:"Audience requested for a destination. Placeholders: {service}, {namespace}." default:"{service}"`
	ClusterDomain    string `json:"cluster_domain" description:"Cluster DNS domain; hosts ending in .svc.<cluster_domain> are in-cluster." default:"cluster.local"`
	Namespace        string `json:"namespace" description:"Namespace assumed for short service names; under a template without {namespace}, also the only namespace a qualified host may name. Alternatively namespace_file."`
	// NamespaceFile is read once, in Configure, when Namespace is empty. It is
	// meant for /var/run/secrets/kubernetes.io/serviceaccount/namespace, which
	// the kubelet projects from the pod's own metadata: the one source that is
	// right in every copy of a config shared across namespaces (the platform
	// renders one authbridge-runtime-config into each agent namespace), where
	// an inline namespace is wrong in all but one. No default and no poller,
	// as for lineage-telemetry's key of the same name.
	NamespaceFile string `json:"namespace_file" description:"Read when namespace is empty, once at start (e.g. /var/run/secrets/kubernetes.io/serviceaccount/namespace); absent, blank or not a DNS label refuses at start. When neither is set, the service-account file is read if it exists."`

	NoTokenPolicy      string `json:"no_token_policy" description:"Outbound request with no bearer: exchange the workload's own client-credentials token, allow it through unchanged, or deny." default:"deny" enum:"client-credentials,allow,deny"`
	UnknownAudienceTTL string `json:"unknown_audience_ttl" description:"How long an audience the IdP does not know is passed through without asking again. 0 asks every time." default:"5m"`
	Timeout            string `json:"timeout" description:"Timeout for each call to the token endpoint." default:"5s"`

	// ResolvePlaceholders matches token-exchange's option of the same name.
	ResolvePlaceholders bool `json:"resolve_placeholders" default:"false" description:"Resolve an outbound bearer carrying the placeholder prefix from the shared store to the real token before exchange. Unresolvable placeholders are denied (fail closed)."`
}

func (c *tokenExchangeAIACConfig) applyDefaults() {
	if c.TokenURL == "" && c.KeycloakURL != "" && c.KeycloakRealm != "" {
		c.TokenURL = strings.TrimSuffix(c.KeycloakURL, "/") + "/realms/" + c.KeycloakRealm + "/protocol/openid-connect/token"
	}
	if c.ClientID == "" && c.ClientIDFile == "" {
		c.ClientIDFile = "/shared/client-id.txt"
	}
	if c.ClientSecret == "" && c.ClientSecretFile == "" {
		c.ClientSecretFile = "/shared/client-secret.txt"
	}
	if c.AudienceTemplate == "" {
		c.AudienceTemplate = placeholderService
	}
	if c.ClusterDomain == "" {
		c.ClusterDomain = "cluster.local"
	}
	if c.NoTokenPolicy == "" {
		c.NoTokenPolicy = auth.NoTokenPolicyDeny
	}
	if c.UnknownAudienceTTL == "" {
		c.UnknownAudienceTTL = "5m"
	}
	if c.Timeout == "" {
		c.Timeout = "5s"
	}
}

// settings are the parsed forms of the config's string-typed fields.
type settings struct {
	unknownTTL time.Duration
	timeout    time.Duration
}

func (c *tokenExchangeAIACConfig) validate() (settings, error) {
	var s settings
	if c.TokenURL == "" {
		return s, errors.New("token_url is required (or keycloak_url and keycloak_realm)")
	}
	switch c.NoTokenPolicy {
	case auth.NoTokenPolicyClientCredentials, auth.NoTokenPolicyAllow, auth.NoTokenPolicyDeny:
	default:
		return s, fmt.Errorf("no_token_policy must be %s, %s or %s, got %q",
			auth.NoTokenPolicyClientCredentials, auth.NoTokenPolicyAllow, auth.NoTokenPolicyDeny, c.NoTokenPolicy)
	}
	var err error
	if s.unknownTTL, err = time.ParseDuration(c.UnknownAudienceTTL); err != nil || s.unknownTTL < 0 {
		return s, fmt.Errorf("unknown_audience_ttl %q is not a non-negative duration", c.UnknownAudienceTTL)
	}
	if s.timeout, err = time.ParseDuration(c.Timeout); err != nil || s.timeout <= 0 {
		return s, fmt.Errorf("timeout %q is not a positive duration", c.Timeout)
	}
	return s, nil
}

// TokenExchangeAIAC is the plugin. The zero value denies every request until
// Configure succeeds.
type TokenExchangeAIAC struct {
	cfg      tokenExchangeAIACConfig
	idp      *idpClient
	targets  *targetResolver
	tokens   *tokenCache
	unknown  *unknownCache
	creds    atomic.Pointer[credentials]
	bgCancel atomic.Pointer[context.CancelFunc]

	// now is the clock for both caches; tests replace it.
	now func() time.Time

	exchanged, cacheHits, unknownPassed, notEligible, denied atomic.Uint64
}

// New constructs an unconfigured plugin.
func New() *TokenExchangeAIAC { return &TokenExchangeAIAC{now: time.Now} }

func init() {
	plugins.RegisterPlugin(PluginName, func() pipeline.Plugin { return New() })
}

func (p *TokenExchangeAIAC) Name() string { return PluginName }

func (p *TokenExchangeAIAC) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		Description: "Exchanges outbound tokens for an audience derived from the destination host; the IdP decides which hosts are targets. No routes.",
	}
}

func (p *TokenExchangeAIAC) ConfigSchema() []pipeline.FieldSchema {
	return pipeline.SchemaOf(tokenExchangeAIACConfig{})
}

func (p *TokenExchangeAIAC) Configure(raw json.RawMessage) error {
	var c tokenExchangeAIACConfig
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return fmt.Errorf("%s config: %w", PluginName, err)
		}
	}
	c.applyDefaults()
	s, err := c.validate()
	if err != nil {
		return fmt.Errorf("%s config: %w", PluginName, err)
	}
	if c.Namespace != "" && !validLabel(c.Namespace) {
		return fmt.Errorf("%s config: namespace %q is not a DNS label", PluginName, c.Namespace)
	}
	if c.Namespace == "" && c.NamespaceFile == "" {
		// Not configured: use the pod's own namespace where there is one. A
		// laptop has no such file and carries on without a namespace.
		if _, err := os.Stat(serviceAccountNamespaceFile); err == nil {
			c.NamespaceFile = serviceAccountNamespaceFile
		}
	}
	if c.Namespace == "" && c.NamespaceFile != "" {
		ns, err := config.ReadCredentialFile(c.NamespaceFile)
		if err != nil {
			return fmt.Errorf("%s config: namespace_file %q: %w", PluginName, c.NamespaceFile, err)
		}
		if !validLabel(ns) {
			return fmt.Errorf("%s config: namespace_file %q: %q is not a DNS label", PluginName, c.NamespaceFile, ns)
		}
		c.Namespace = ns
	}
	targets, err := newTargetResolver(c.AudienceTemplate, c.ClusterDomain, c.Namespace)
	if err != nil {
		return fmt.Errorf("%s config: %w", PluginName, err)
	}
	switch {
	case c.Namespace != "":
	case targets.usesNamespace():
		slog.Warn(PluginName+": audience_template uses {namespace} and namespace is unset; "+
			"short service names will pass through without an exchange",
			"audience_template", c.AudienceTemplate)
	default:
		slog.Warn(PluginName+": namespace is unset; hosts naming a namespace "+
			"(service.namespace.svc) are never exchanged, and are denied when they name a target",
			"audience_template", c.AudienceTemplate)
	}

	// Best-effort synchronous credential load; Init polls for whatever is
	// missing. Logged so a wrong path shows up in the first pod's logs.
	if c.ClientID == "" {
		if v, err := config.ReadCredentialFile(c.ClientIDFile); err == nil {
			c.ClientID = v
		} else {
			slog.Warn(PluginName+": client_id_file not yet readable; Init will poll",
				"path", c.ClientIDFile, "error", err)
		}
	}
	if c.ClientSecret == "" {
		if v, err := config.ReadCredentialFile(c.ClientSecretFile); err == nil {
			c.ClientSecret = v
		} else {
			slog.Warn(PluginName+": client_secret_file not yet readable; Init will poll",
				"path", c.ClientSecretFile, "error", err)
		}
	}

	// Commit only after everything fallible has succeeded.
	if p.now == nil {
		p.now = time.Now
	}
	p.cfg = c
	p.idp = &idpClient{tokenURL: c.TokenURL, http: &http.Client{Timeout: s.timeout}}
	p.targets = targets
	p.tokens = newTokenCache(p.now)
	p.unknown = newUnknownCache(p.now, s.unknownTTL)
	if c.ClientID != "" && c.ClientSecret != "" {
		p.creds.Store(&credentials{clientID: c.ClientID, clientSecret: c.ClientSecret})
	}
	return nil
}

// Init starts a background wait for credential files Configure could not read.
// The wait runs on its own context, cancelled by Shutdown, because Init's ctx
// only bounds pipeline start-up.
func (p *TokenExchangeAIAC) Init(_ context.Context) error {
	if p.creds.Load() != nil || p.bgCancel.Load() != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.bgCancel.Store(&cancel)
	go p.pollCredentials(ctx)
	return nil
}

func (p *TokenExchangeAIAC) pollCredentials(ctx context.Context) {
	id, secret := p.cfg.ClientID, p.cfg.ClientSecret
	var err error
	if id == "" {
		if id, err = config.WaitForCredentialFile(ctx, p.cfg.ClientIDFile); err != nil {
			return
		}
	}
	if secret == "" {
		if secret, err = config.WaitForCredentialFile(ctx, p.cfg.ClientSecretFile); err != nil {
			return
		}
	}
	p.creds.Store(&credentials{clientID: id, clientSecret: secret})
	slog.Info(PluginName + ": credentials loaded")
}

func (p *TokenExchangeAIAC) Shutdown(_ context.Context) error {
	if cancel := p.bgCancel.Swap(nil); cancel != nil {
		(*cancel)()
	}
	return nil
}

// Ready holds the pod out of service until client credentials are loaded.
func (p *TokenExchangeAIAC) Ready() bool { return p.creds.Load() != nil }

func (p *TokenExchangeAIAC) OnRequest(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
	if p.idp == nil {
		return pipeline.DenyStatus(http.StatusServiceUnavailable, "upstream.unreachable", PluginName+" not configured")
	}

	// Resolved before the host is looked at, as in token-exchange: an
	// unresolvable placeholder is denied whatever the destination. Only the
	// exchange sees the real token; a request that passes through keeps the
	// placeholder.
	subject := auth.ExtractBearer(pctx.Headers.Get("Authorization"))
	if p.cfg.ResolvePlaceholders && placeholder.IsPlaceholder(subject) {
		real, ok := resolvePlaceholder(pctx, subject)
		if !ok {
			p.denied.Add(1)
			return pctx.DenyAndRecord(reasonPlaceholderUnresolved, "auth.unauthorized", "unresolvable credential placeholder")
		}
		subject = real
	}

	// A CONNECT tunnel, or a transparently redirected connection, carries
	// opaque bytes: no header set here reaches the destination, and the
	// destination's own Authorization travels inside the encrypted stream. Gate
	// nothing on it, as the doc promises.
	if pctx.Method == http.MethodConnect {
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionSkip, Reason: reasonOpaqueTunnel,
			Details: map[string]string{"host": pctx.Host}})
		return pipeline.Action{Type: pipeline.Continue}
	}

	t, why := p.targets.resolve(pctx.Host)
	if why != "" {
		p.notEligible.Add(1)
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionSkip, Reason: why,
			Details: map[string]string{"host": pctx.Host}})
		return pipeline.Action{Type: pipeline.Continue}
	}
	details := map[string]string{"host": pctx.Host, "target_audience": t.audience}

	if p.unknown.has(t.audience) {
		return p.passUnknown(pctx, details, true)
	}

	// allow forwards a bearer-less request unchanged whatever the
	// destination is, so it needs neither credentials nor the IdP.
	if subject == "" && p.cfg.NoTokenPolicy == auth.NoTokenPolicyAllow {
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionSkip, Reason: reasonNoToken, Details: details})
		return pipeline.Action{Type: pipeline.Continue}
	}
	if t.foreign {
		return p.judgeForeign(ctx, pctx, details, t.audience)
	}

	creds := p.creds.Load()
	if creds == nil {
		return p.deny(pctx, details, reasonCredentialsAbsent, http.StatusServiceUnavailable,
			"upstream.token-exchange-failed", "client credentials are not loaded yet")
	}

	// With no bearer, both remaining policies exchange the workload's own
	// token: only an exchange tells a target from a destination the IdP has no
	// client for. client-credentials forwards the narrowed token; deny uses
	// the exchange only to find out, so that a non-target still passes through
	// and only a target answers 401.
	var (
		tok      string
		cacheHit bool
		err      error
	)
	if subject == "" {
		tok, cacheHit, err = p.exchangeSelf(ctx, *creds, t.audience)
		if err == nil {
			if p.cfg.NoTokenPolicy == auth.NoTokenPolicyDeny {
				return p.denyNoToken(pctx, details)
			}
			return p.replace(pctx, details, tok, cacheHit)
		}
	} else {
		tok, cacheHit, err = p.exchangeCached(ctx, *creds, subject, t.audience)
		if err == nil {
			return p.replace(pctx, details, tok, cacheHit)
		}
		// Keycloak judges the subject before the audience, so "Invalid token"
		// says nothing about the destination: an expired user token, or a
		// bearer it never issued (an API key for an in-cluster model gateway),
		// gets it for unknown audiences too. Ask again with the workload's own
		// token, which the IdP does accept, and let that answer decide.
		if classify(err) == outcomeSubjectInvalid {
			bearerErr := err
			if _, _, err = p.exchangeSelf(ctx, *creds, t.audience); err == nil {
				return p.deny(pctx, details, reasonSubjectInvalid, http.StatusUnauthorized,
					"auth.unauthorized", "the bearer token was rejected by the IdP")
			}
			if classify(err) == outcomeFailed {
				// Keep why the bearer was refused in the log, not only why
				// the second question could not be answered.
				err = fmt.Errorf("%w (the bearer was refused first: %v)", err, bearerErr)
			}
		}
	}
	switch classify(err) {
	case outcomeUnknownAudience:
		p.unknown.add(t.audience)
		return p.passUnknown(pctx, details, false)
	case outcomeNotGranted:
		slog.Info(PluginName+": audience exists but is not granted to this workload",
			"host", pctx.Host, "audience", t.audience)
		return p.deny(pctx, details, reasonNotGranted, http.StatusForbidden,
			"policy.forbidden", "this workload is not granted the destination's audience")
	case outcomeTargetDisabled:
		slog.Info(PluginName+": the destination's client is disabled", "host", pctx.Host, "audience", t.audience)
		return p.deny(pctx, details, reasonTargetDisabled, http.StatusForbidden,
			"policy.forbidden", "the destination's client is disabled")
	default:
		// Logged, not returned: the error can name other audiences the
		// caller holds, and the response body goes back to the workload.
		slog.Warn(PluginName+": token exchange failed", "host", pctx.Host, "audience", t.audience, "error", err)
		return p.deny(pctx, details, reasonExchangeFailed, http.StatusServiceUnavailable,
			"upstream.token-exchange-failed", "token exchange failed")
	}
}

// denyNoToken answers a bearer-less request to a target under
// no_token_policy: deny.
func (p *TokenExchangeAIAC) denyNoToken(pctx *pipeline.Context, details map[string]string) pipeline.Action {
	return p.deny(pctx, details, reasonNoToken, http.StatusUnauthorized,
		"auth.unauthorized", "missing Authorization header")
}

// judgeForeign answers a request to a host in another namespace whose audience
// does not carry the namespace. It is never exchanged, whatever its bearer:
// the token would be a target's, sent to a Service that only shares its name.
// The IdP is asked with the workload's own token whether the audience names a
// client. When it does not, the request passes through like any other
// destination the IdP does not know. When it does, the host is claiming a
// target's name, and the request is denied — never forwarded with the
// caller's bearer either, which a lookalike Service would otherwise receive.
func (p *TokenExchangeAIAC) judgeForeign(ctx context.Context, pctx *pipeline.Context, details map[string]string, audience string) pipeline.Action {
	creds := p.creds.Load()
	if creds == nil {
		return p.deny(pctx, details, reasonCredentialsAbsent, http.StatusServiceUnavailable,
			"upstream.token-exchange-failed", "client credentials are not loaded yet")
	}
	_, _, err := p.exchangeSelf(ctx, *creds, audience)
	switch o := classify(err); {
	case err != nil && o == outcomeUnknownAudience:
		p.unknown.add(audience)
		return p.passUnknown(pctx, details, false)
	case err == nil, o == outcomeNotGranted, o == outcomeTargetDisabled:
		slog.Warn(PluginName+": host in another namespace names a target; denied",
			"host", pctx.Host, "audience", audience)
		return p.deny(pctx, details, reasonForeignNamespace, http.StatusForbidden,
			"policy.forbidden", "the destination is in another namespace and shares a target's name")
	default:
		slog.Warn(PluginName+": token exchange failed", "host", pctx.Host, "audience", audience, "error", err)
		return p.deny(pctx, details, reasonExchangeFailed, http.StatusServiceUnavailable,
			"upstream.token-exchange-failed", "token exchange failed")
	}
}

// exchangeCached returns a token for audience in exchange for subject, from
// the cache when it holds one.
func (p *TokenExchangeAIAC) exchangeCached(ctx context.Context, creds credentials, subject, audience string) (string, bool, error) {
	key := tokenKey(subject, audience)
	if tok, ok := p.tokens.get(key); ok {
		p.cacheHits.Add(1)
		return tok, true, nil
	}
	issued, err := p.idp.exchange(ctx, creds, subject, audience)
	if err != nil {
		return "", false, err
	}
	p.tokens.put(key, issued)
	p.exchanged.Add(1)
	return issued.accessToken, false, nil
}

// exchangeSelf is exchangeCached with the workload's own token as the subject.
// A cached own token the IdP no longer accepts — after a Keycloak restart or a
// realm key rotation — is dropped and fetched again, once, so that it does not
// fail every request until it would have expired.
//
// The retry is bounded by a flag, not by the cache: a concurrent request can
// put another own token between the drop and the retry's read, and that one
// may be rejected too.
func (p *TokenExchangeAIAC) exchangeSelf(ctx context.Context, creds credentials, audience string) (string, bool, error) {
	return p.exchangeSelfOnce(ctx, creds, audience, true)
}

func (p *TokenExchangeAIAC) exchangeSelfOnce(ctx context.Context, creds credentials, audience string, mayRetry bool) (string, bool, error) {
	self, fromCache, err := p.selfToken(ctx, creds)
	if err != nil {
		slog.Warn(PluginName+": client-credentials grant failed", "error", err)
		return "", false, fmt.Errorf("could not obtain this workload's own token: %w", err)
	}
	tok, hit, err := p.exchangeCached(ctx, creds, self, audience)
	if mayRetry && fromCache && classify(err) == outcomeSubjectInvalid {
		p.tokens.drop(selfTokenKey)
		return p.exchangeSelfOnce(ctx, creds, audience, false)
	}
	return tok, hit, err
}

// resolvePlaceholder looks a handle up in the shared store. Returns ok=false
// (fail closed) when no store is wired or the handle is unknown or expired.
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

// selfToken returns this workload's own client-credentials token, cached, and
// whether it came from the cache.
func (p *TokenExchangeAIAC) selfToken(ctx context.Context, creds credentials) (string, bool, error) {
	if tok, ok := p.tokens.get(selfTokenKey); ok {
		return tok, true, nil
	}
	issued, err := p.idp.clientCredentials(ctx, creds)
	if err != nil {
		return "", false, err
	}
	p.tokens.put(selfTokenKey, issued)
	return issued.accessToken, false, nil
}

func (p *TokenExchangeAIAC) replace(pctx *pipeline.Context, details map[string]string, token string, cacheHit bool) pipeline.Action {
	pctx.Headers.Set("Authorization", "Bearer "+token)
	reason := reasonExchanged
	if cacheHit {
		reason = reasonCacheHit
	}
	details["cache_hit"] = boolString(cacheHit)
	recordDelegationHop(pctx, token, details["target_audience"], cacheHit)
	pctx.Record(pipeline.Invocation{Action: pipeline.ActionModify, Reason: reason, Details: details})
	return pipeline.Action{Type: pipeline.Continue}
}

// recordDelegationHop appends the exchange to the delegation chain, which the
// opa plugin exposes as input.delegation and, on the outbound leg, as
// input.identity. Subject and scopes come from the issued token: it came from
// the IdP over this plugin's own connection, so unlike the incoming bearer it
// is not a value the workload chose. A validated inbound identity, when one is
// present, names the subject instead.
func recordDelegationHop(pctx *pipeline.Context, token, audience string, cacheHit bool) {
	sub, scopes := tokenSubjectAndScopes(token)
	if pctx.Identity != nil {
		sub = pctx.Identity.Subject()
	}
	if pctx.Extensions.Delegation == nil {
		pctx.Extensions.Delegation = &pipeline.DelegationExtension{}
	}
	pctx.Extensions.Delegation.AppendHop(pipeline.DelegationHop{
		SubjectID: sub,
		Scopes:    scopes,
		Timestamp: time.Now(),
		Audience:  audience,
		Strategy:  PluginName,
		FromCache: cacheHit,
	})
}

// passUnknown forwards a request to a destination the IdP has no client for,
// unchanged — including whatever Authorization header the workload sent.
func (p *TokenExchangeAIAC) passUnknown(pctx *pipeline.Context, details map[string]string, cached bool) pipeline.Action {
	p.unknownPassed.Add(1)
	details["cached"] = boolString(cached)
	pctx.Record(pipeline.Invocation{Action: pipeline.ActionSkip, Reason: reasonAudienceUnknown, Details: details})
	return pipeline.Action{Type: pipeline.Continue}
}

func (p *TokenExchangeAIAC) deny(pctx *pipeline.Context, details map[string]string, reason string, status int, code, msg string) pipeline.Action {
	p.denied.Add(1)
	pctx.Record(pipeline.Invocation{Action: pipeline.ActionDeny, Reason: reason, Details: details})
	return pipeline.DenyStatus(status, code, msg)
}

func (p *TokenExchangeAIAC) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// Metrics reports counters since the pipeline was built.
func (p *TokenExchangeAIAC) Metrics() []pipeline.Metric {
	return []pipeline.Metric{
		{Name: PluginName + ".exchanged", Value: float64(p.exchanged.Load()), Unit: "count", Note: "tokens issued by the IdP"},
		{Name: PluginName + ".cache_hits", Value: float64(p.cacheHits.Load()), Unit: "count"},
		{Name: PluginName + ".unknown_passthrough", Value: float64(p.unknownPassed.Load()), Unit: "count", Note: "requests to destinations the IdP has no client for"},
		{Name: PluginName + ".not_eligible", Value: float64(p.notEligible.Load()), Unit: "count", Note: "requests to hosts not offered to the IdP"},
		{Name: PluginName + ".denied", Value: float64(p.denied.Load()), Unit: "count"},
	}
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

var (
	_ pipeline.Configurable    = (*TokenExchangeAIAC)(nil)
	_ pipeline.Initializer     = (*TokenExchangeAIAC)(nil)
	_ pipeline.Shutdowner      = (*TokenExchangeAIAC)(nil)
	_ pipeline.Readier         = (*TokenExchangeAIAC)(nil)
	_ pipeline.SchemaProvider  = (*TokenExchangeAIAC)(nil)
	_ pipeline.MetricsProvider = (*TokenExchangeAIAC)(nil)
)
