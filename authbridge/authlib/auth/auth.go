package auth

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rossoctl/rossocortex/authbridge/authlib/bypass"
	"github.com/rossoctl/rossocortex/authbridge/authlib/plugins/jwtvalidation/validation"
	"github.com/rossoctl/rossocortex/authbridge/authlib/plugins/tokenexchange/cache"
	"github.com/rossoctl/rossocortex/authbridge/authlib/plugins/tokenexchange/exchange"
	"github.com/rossoctl/rossocortex/authbridge/authlib/routing"
)

// IdentityConfig holds the agent's identity for audience validation and token exchange.
type IdentityConfig struct {
	ClientID  string   // agent's OAuth client ID (used by token-exchange)
	Audiences []string // expected inbound JWT audiences (jwt-validation); at least one required for static inbound validation
	Issuer    string   // expected JWT iss (jwt-validation); inbound debug logging only
}

// AudienceDeriver derives a target audience from a request host.
// Used by waypoint mode to auto-derive audience from the destination service name.
// Returns "" if no derivation is possible (falls back to route config).
type AudienceDeriver func(host string) string

// Config holds the resolved dependencies for the auth layer.
type Config struct {
	Verifier        validation.Verifier
	Exchanger       *exchange.Client
	Cache           *cache.Cache
	Bypass          *bypass.Matcher
	Router          *routing.Router
	Identity        IdentityConfig
	NoTokenPolicy   string          // NoTokenClientCredentials, NoTokenAllow, or NoTokenDeny
	AudienceDeriver AudienceDeriver // optional, derives audience from host (waypoint mode)
	Logger          *slog.Logger
}

// Auth composes authlib building blocks into inbound validation and outbound exchange.
type Auth struct {
	verifier        validation.Verifier
	exchanger       *exchange.Client
	cache           *cache.Cache
	bypass          *bypass.Matcher
	router          *routing.Router
	identity        atomic.Pointer[IdentityConfig]
	noTokenPolicy   string
	audienceDeriver AudienceDeriver
	log             *slog.Logger
	Stats           *Stats
}

// Stats holds statistics for validation and exchange.
// The current implementation keeps approvals/denials/exchange as counters.
type Stats struct {
	mu                    sync.Mutex // protects the following fields
	inboundApprovals      map[InboundApprovalReason]int
	inboundDenials        map[InboundDenialReason]int
	outboundApprovals     map[OutboundApprovalReason]int
	outboundDenials       map[OutboundDenialReason]int
	outboundReplaceTokens map[OutboundReplaceTokenReason]int
}

// InboundDenialReason enumerates the reasons for inbound validation failure.
type InboundDenialReason int

const (
	DENY_NO_HEADER InboundDenialReason = iota
	DENY_MALFORMED_HEADER
	DENY_VALIDATOR_MISSING
	DENY_JWT_FAILED
)

// InboundApprovalReason enumerates the rationale for inbound validation success.
type InboundApprovalReason int

const (
	APPROVE_PASSTHROUGH InboundApprovalReason = iota
	APPROVE_AUTHORIZED
)

// OutboundApprovalReason enumerates the rationale for outbound validation success.
type OutboundApprovalReason int

const (
	OUTBOUND_NO_MATCHING_ROUTE OutboundApprovalReason = iota
	OUTBOUND_NO_EXCHANGER
	OUTBOUND_PASSTHROUGH
	OUTBOUND_NO_TOKEN_POLICY
)

// OutboundDenialReason enumerates the reasons for outbound denial.
type OutboundDenialReason int

const (
	OUTBOUND_CREDS_REQUESTED_NO_EXCHANGER OutboundDenialReason = iota
	OUTBOUND_CREDENTIALS_GRANT_FAILURE
	OUTBOUND_NO_TOKEN
	OUTBOUND_TOKEN_EXCHANGE_FAILED
)

// OutboundReplaceTokenReason enumerates the reasons for outbound token exchange.
type OutboundReplaceTokenReason int

const (
	OUTBOUND_ACTION_REPLACE_TOKEN OutboundReplaceTokenReason = iota
	OUTBOUND_ACTION_CACHE_HIT
)

func (r InboundDenialReason) String() string {
	switch r {
	case DENY_NO_HEADER:
		return "no_header"
	case DENY_MALFORMED_HEADER:
		return "malformed_header"
	case DENY_VALIDATOR_MISSING:
		return "validator_missing"
	case DENY_JWT_FAILED:
		return "jwt_failed"
	default:
		return "unknown"
	}
}

func (r InboundApprovalReason) String() string {
	switch r {
	case APPROVE_PASSTHROUGH:
		return "passthrough"
	case APPROVE_AUTHORIZED:
		return "authorized"
	default:
		return "unknown"
	}
}

func (r OutboundApprovalReason) String() string {
	switch r {
	case OUTBOUND_NO_MATCHING_ROUTE:
		return "no_matching_route"
	case OUTBOUND_NO_EXCHANGER:
		return "no_exchanger"
	case OUTBOUND_PASSTHROUGH:
		return "passthrough"
	case OUTBOUND_NO_TOKEN_POLICY:
		return "no_token_policy"
	default:
		return "unknown"
	}
}

func (r OutboundDenialReason) String() string {
	switch r {
	case OUTBOUND_CREDS_REQUESTED_NO_EXCHANGER:
		return "creds_requested_no_exchanger"
	case OUTBOUND_CREDENTIALS_GRANT_FAILURE:
		return "credentials_grant_failure"
	case OUTBOUND_NO_TOKEN:
		return "no_token"
	case OUTBOUND_TOKEN_EXCHANGE_FAILED:
		return "token_exchange_failed"
	default:
		return "unknown"
	}
}

func (r OutboundReplaceTokenReason) String() string {
	switch r {
	case OUTBOUND_ACTION_REPLACE_TOKEN:
		return "replace_token"
	case OUTBOUND_ACTION_CACHE_HIT:
		return "cache_hit"
	default:
		return "unknown"
	}
}

func (s *Stats) MarshalJSON() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	inApprovals := make(map[string]int, len(s.inboundApprovals))
	for k, v := range s.inboundApprovals {
		inApprovals[k.String()] = v
	}
	inDenials := make(map[string]int, len(s.inboundDenials))
	for k, v := range s.inboundDenials {
		inDenials[k.String()] = v
	}
	outApprovals := make(map[string]int, len(s.outboundApprovals))
	for k, v := range s.outboundApprovals {
		outApprovals[k.String()] = v
	}
	outDenials := make(map[string]int, len(s.outboundDenials))
	for k, v := range s.outboundDenials {
		outDenials[k.String()] = v
	}
	outReplaceTokens := make(map[string]int, len(s.outboundReplaceTokens))
	for k, v := range s.outboundReplaceTokens {
		outReplaceTokens[k.String()] = v
	}

	return json.Marshal(struct {
		InboundApprovals      map[string]int `json:"inbound_approvals"`
		InboundDenials        map[string]int `json:"inbound_denials"`
		OutboundApprovals     map[string]int `json:"outbound_approvals"`
		OutboundDenials       map[string]int `json:"outbound_denials"`
		OutboundReplaceTokens map[string]int `json:"outbound_replace_tokens"`
	}{
		InboundApprovals:      inApprovals,
		InboundDenials:        inDenials,
		OutboundApprovals:     outApprovals,
		OutboundDenials:       outDenials,
		OutboundReplaceTokens: outReplaceTokens,
	})
}

// New creates an Auth instance from resolved configuration.
func New(cfg Config) *Auth {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	a := &Auth{
		verifier:        cfg.Verifier,
		exchanger:       cfg.Exchanger,
		cache:           cfg.Cache,
		bypass:          cfg.Bypass,
		router:          cfg.Router,
		noTokenPolicy:   cfg.NoTokenPolicy,
		audienceDeriver: cfg.AudienceDeriver,
		log:             logger,
		Stats:           NewStats(),
	}
	id := cfg.Identity
	a.identity.Store(&id)
	return a
}

// UpdateIdentity updates the agent's identity and exchanger credentials
// after credential files have been resolved. This is called from a background
// goroutine after the gRPC listener has started.
func (a *Auth) UpdateIdentity(id IdentityConfig, clientAuth exchange.ClientAuth) {
	a.identity.Store(&id)
	if clientAuth != nil {
		a.exchanger.UpdateAuth(clientAuth)
	}
	a.log.Info("identity updated", "client_id", id.ClientID)
}

// Ready returns true if inbound expected audiences have been loaded.
func (a *Auth) Ready() bool {
	id := a.identity.Load()
	return id != nil && len(id.Audiences) > 0
}

// InboundAudiences returns a copy of the configured expected audiences
// for inbound JWT validation (may be empty before credentials load).
func (a *Auth) InboundAudiences() []string {
	id := a.identity.Load()
	if id == nil || len(id.Audiences) == 0 {
		return nil
	}
	out := make([]string, len(id.Audiences))
	copy(out, id.Audiences)
	return out
}

// HandleInbound validates an inbound request's JWT token.
// audience overrides the default expected audience when non-empty. This supports
// waypoint mode where audience is derived per-request from the destination host.
// For envoy-sidecar and proxy-sidecar modes, pass "" to use the configured default.
func (a *Auth) HandleInbound(ctx context.Context, authHeader, path, audience string) *InboundResult {
	// 1. Bypass check
	if a.bypass != nil && a.bypass.Match(path) {
		a.IncInboundApprove(APPROVE_PASSTHROUGH)
		a.log.Debug("bypass path matched", "path", path)
		return &InboundResult{Action: ActionAllow}
	}

	// 2. Extract bearer token
	if authHeader == "" {
		a.IncInboundDeny(DENY_NO_HEADER)
		a.log.Debug("inbound denied: no Authorization header", "path", path)
		return &InboundResult{
			Action:         ActionDeny,
			DenyStatus:     http.StatusUnauthorized,
			DenyReason:     "missing Authorization header",
			DenyReasonCode: DENY_NO_HEADER,
		}
	}
	token := extractBearer(authHeader)
	if token == "" {
		a.IncInboundDeny(DENY_MALFORMED_HEADER)
		a.log.Debug("inbound denied: malformed Authorization header", "path", path)
		return &InboundResult{
			Action:         ActionDeny,
			DenyStatus:     http.StatusUnauthorized,
			DenyReason:     "invalid Authorization header format",
			DenyReasonCode: DENY_MALFORMED_HEADER,
		}
	}

	// 3. Validate JWT
	if a.verifier == nil {
		a.IncInboundDeny(DENY_VALIDATOR_MISSING)
		return &InboundResult{
			Action:         ActionDeny,
			DenyStatus:     http.StatusUnauthorized,
			DenyReason:     "inbound validation not configured",
			DenyReasonCode: DENY_VALIDATOR_MISSING,
		}
	}
	var audiences []string
	if audience != "" {
		audiences = []string{audience} // waypoint mode: single derived audience
	} else {
		id := a.identity.Load()
		if id != nil {
			audiences = id.Audiences
		}
	}
	if len(audiences) == 0 {
		return &InboundResult{
			Action:         ActionDeny,
			DenyStatus:     http.StatusServiceUnavailable,
			DenyReason:     "identity not yet configured (credentials pending)",
			DenyReasonCode: DENY_VALIDATOR_MISSING,
		}
	}
	a.log.Debug("validating inbound JWT", "path", path, "expectedAudiences", audiences)
	claims, err := a.verifier.Verify(ctx, token, audiences)
	if err != nil {
		// Log full error at Info; log detailed context at Debug.
		// Generic message returned to client to avoid leaking details.
		a.IncInboundDeny(DENY_JWT_FAILED)
		a.log.Info("JWT validation failed", "error", err)
		var expectedIssuer string
		if id := a.identity.Load(); id != nil {
			expectedIssuer = id.Issuer
		}
		a.log.Debug("JWT validation details",
			"path", path,
			"expectedAudiences", audiences,
			"expectedIssuer", expectedIssuer,
			"error", err)
		return &InboundResult{
			Action:         ActionDeny,
			DenyStatus:     http.StatusUnauthorized,
			DenyReason:     "token validation failed",
			DenyReasonCode: DENY_JWT_FAILED,
		}
	}

	// 4. Allow with claims
	a.IncInboundApprove(APPROVE_AUTHORIZED)
	a.log.Info("inbound authorized",
		"subject", claims.Subject, "clientID", claims.ClientID)
	a.log.Debug("inbound authorized details",
		"path", path,
		"audience", claims.Audience,
		"scopes", claims.Scopes)
	return &InboundResult{Action: ActionAllow, Claims: claims}
}

// HandleOutbound processes an outbound request, performing token exchange if needed.
func (a *Auth) HandleOutbound(ctx context.Context, authHeader, host string) *OutboundResult {
	// 1. Resolve route
	var resolved *routing.ResolvedRoute
	if a.router != nil {
		resolved = a.router.Resolve(host)
	}

	// 2. Passthrough
	if resolved == nil {
		a.IncOutboundApprove(OUTBOUND_NO_MATCHING_ROUTE)
		a.log.Info("outbound passthrough", "host", host, "reason", "no matching route")
		return &OutboundResult{Action: ActionAllow}
	}
	if resolved.Passthrough {
		a.IncOutboundApprove(OUTBOUND_PASSTHROUGH)
		a.log.Info("outbound passthrough", "host", host, "reason", "route action")
		return &OutboundResult{Action: ActionAllow}
	}

	// 3. Determine audience/scopes
	audience := resolved.Audience
	scopes := resolved.Scopes

	// If no audience from route and deriver is set, derive from host (waypoint pattern)
	if audience == "" && a.audienceDeriver != nil {
		audience = a.audienceDeriver(host)
		a.log.Debug("audience derived from host", "host", host, "audience", audience)
	}

	a.log.Debug("outbound exchange requested",
		"host", host, "audience", audience, "scopes", scopes,
		"hasSubjectToken", authHeader != "")

	// 4. Extract bearer token
	subjectToken := extractBearer(authHeader)

	if subjectToken == "" {
		// No token — apply no-token policy
		a.log.Debug("no subject token, applying no-token policy",
			"policy", a.noTokenPolicy, "host", host, "audience", audience)
		return a.handleNoToken(ctx, audience, scopes)
	}

	// 5. Cache check
	if a.cache != nil {
		if cached, ok := a.cache.Get(subjectToken, audience); ok {
			a.IncOutboundReplaceToken(OUTBOUND_ACTION_CACHE_HIT)
			a.log.Debug("outbound cache hit", "host", host, "audience", audience)
			return &OutboundResult{
				Action:          ActionReplaceToken,
				Token:           cached,
				CacheHit:        true,
				RouteMatched:    true,
				TargetAudience:  audience,
				RequestedScopes: scopes,
			}
		}
	}

	// 6. Token exchange
	if a.exchanger == nil {
		a.IncOutboundApprove(OUTBOUND_NO_EXCHANGER)
		a.log.Warn("exchanger not configured, passing through",
			"host", host, "audience", audience)
		return &OutboundResult{Action: ActionAllow}
	}

	// RFC 8693 Section 4.1 actor-token "act" claim chaining is not yet
	// wired by any plugin; the wire-format support stays in
	// exchange.ExchangeRequest.ActorToken for when a plugin needs it.
	resp, err := a.exchanger.Exchange(ctx, &exchange.ExchangeRequest{
		SubjectToken:  subjectToken,
		Audience:      audience,
		Scopes:        scopes,
		TokenEndpoint: resolved.TokenEndpoint, // per-route override
	})
	if err != nil {
		a.IncOutboundDeny(OUTBOUND_TOKEN_EXCHANGE_FAILED)
		a.log.Info("token exchange failed", "host", host, "error", err)
		a.log.Debug("token exchange failure details",
			"host", host,
			"audience", audience,
			"scopes", scopes,
			"tokenEndpoint", resolved.TokenEndpoint,
			"error", err)
		return &OutboundResult{
			Action:          ActionDeny,
			DenyStatus:      http.StatusServiceUnavailable,
			DenyReason:      "token exchange failed",
			DenyReasonCode:  OUTBOUND_TOKEN_EXCHANGE_FAILED,
			RouteMatched:    true,
			TargetAudience:  audience,
			RequestedScopes: scopes,
		}
	}

	// 7. Cache result
	if a.cache != nil && resp.ExpiresIn > 0 {
		a.cache.Set(subjectToken, audience, resp.AccessToken,
			time.Duration(resp.ExpiresIn)*time.Second)
	}

	a.IncOutboundReplaceToken(OUTBOUND_ACTION_REPLACE_TOKEN)
	a.log.Info("outbound token exchanged", "host", host, "audience", audience)
	a.log.Debug("outbound exchange details",
		"host", host, "audience", audience, "expiresIn", resp.ExpiresIn)
	return &OutboundResult{
		Action:          ActionReplaceToken,
		Token:           resp.AccessToken,
		RouteMatched:    true,
		TargetAudience:  audience,
		RequestedScopes: scopes,
	}
}

func (a *Auth) handleNoToken(ctx context.Context, audience, scopes string) *OutboundResult {
	switch a.noTokenPolicy {
	case NoTokenPolicyAllow:
		a.IncOutboundApprove(OUTBOUND_NO_TOKEN_POLICY)
		a.log.Debug("no token, policy=allow")
		return &OutboundResult{Action: ActionAllow}

	case NoTokenPolicyClientCredentials:
		if a.exchanger == nil {
			a.IncOutboundDeny(OUTBOUND_CREDS_REQUESTED_NO_EXCHANGER)
			a.log.Debug("no token, client_credentials requested but exchanger not configured",
				"audience", audience)
			return &OutboundResult{
				Action:         ActionDeny,
				DenyStatus:     http.StatusServiceUnavailable,
				DenyReason:     "exchanger not configured for client credentials",
				DenyReasonCode: OUTBOUND_CREDS_REQUESTED_NO_EXCHANGER,
			}
		}
		a.log.Debug("no token, falling back to client_credentials",
			"audience", audience, "scopes", scopes)
		resp, err := a.exchanger.ClientCredentials(ctx, audience, scopes)
		if err != nil {
			a.IncOutboundDeny(OUTBOUND_CREDENTIALS_GRANT_FAILURE)
			a.log.Info("client credentials grant failed", "error", err)
			a.log.Debug("client credentials failure details",
				"audience", audience, "scopes", scopes, "error", err)
			return &OutboundResult{
				Action:         ActionDeny,
				DenyStatus:     http.StatusServiceUnavailable,
				DenyReason:     "client credentials token acquisition failed",
				DenyReasonCode: OUTBOUND_CREDENTIALS_GRANT_FAILURE,
			}
		}
		a.IncOutboundReplaceToken(OUTBOUND_ACTION_REPLACE_TOKEN)
		return &OutboundResult{Action: ActionReplaceToken, Token: resp.AccessToken}

	default: // NoTokenDeny or unknown
		a.IncOutboundDeny(OUTBOUND_NO_TOKEN)
		a.log.Debug("no token, policy denies request",
			"policy", a.noTokenPolicy, "audience", audience)
		return &OutboundResult{
			Action:         ActionDeny,
			DenyStatus:     http.StatusUnauthorized,
			DenyReason:     "missing Authorization header",
			DenyReasonCode: OUTBOUND_NO_TOKEN,
		}
	}
}

func extractBearer(authHeader string) string {
	// RFC 7235: auth scheme is case-insensitive
	if len(authHeader) > 7 && strings.EqualFold(authHeader[:7], "bearer ") {
		return authHeader[7:]
	}
	return ""
}

func NewStats() *Stats {
	return &Stats{
		inboundApprovals:      make(map[InboundApprovalReason]int),
		inboundDenials:        make(map[InboundDenialReason]int),
		outboundApprovals:     make(map[OutboundApprovalReason]int),
		outboundDenials:       make(map[OutboundDenialReason]int),
		outboundReplaceTokens: make(map[OutboundReplaceTokenReason]int),
	}
}

// MergeStats returns a new Stats whose counters are the sum of all
// srcs. Each src's mutex is taken independently; the returned Stats
// has its own storage and no relationship to the sources. Safe to
// call concurrently — sources are only read under their own locks.
//
// Used by the /stats aggregator to fold per-plugin counters into a
// single response per HTTP request without leaking plugin-local
// Stats instances into the presentation layer.
func MergeStats(srcs ...*Stats) *Stats {
	out := NewStats()
	for _, s := range srcs {
		if s == nil {
			continue
		}
		s.mu.Lock()
		for k, v := range s.inboundApprovals {
			out.inboundApprovals[k] += v
		}
		for k, v := range s.inboundDenials {
			out.inboundDenials[k] += v
		}
		for k, v := range s.outboundApprovals {
			out.outboundApprovals[k] += v
		}
		for k, v := range s.outboundDenials {
			out.outboundDenials[k] += v
		}
		for k, v := range s.outboundReplaceTokens {
			out.outboundReplaceTokens[k] += v
		}
		s.mu.Unlock()
	}
	return out
}

// IncInboundApprove records a new approval (for statistics)
func (a *Auth) IncInboundApprove(reason InboundApprovalReason) {
	a.Stats.mu.Lock()
	a.Stats.inboundApprovals[reason]++
	a.Stats.mu.Unlock()
}

// IncInboundDeny records a new denial (for statistics)
func (a *Auth) IncInboundDeny(reason InboundDenialReason) {
	a.Stats.mu.Lock()
	a.Stats.inboundDenials[reason]++
	a.Stats.mu.Unlock()
}

// IncOutboundApprove records a new approval (for statistics)
func (a *Auth) IncOutboundApprove(reason OutboundApprovalReason) {
	a.Stats.mu.Lock()
	a.Stats.outboundApprovals[reason]++
	a.Stats.mu.Unlock()
}

// IncOutboundDeny records a new denial (for statistics)
func (a *Auth) IncOutboundDeny(reason OutboundDenialReason) {
	a.Stats.mu.Lock()
	a.Stats.outboundDenials[reason]++
	a.Stats.mu.Unlock()
}

func (a *Auth) IncOutboundReplaceToken(reason OutboundReplaceTokenReason) {
	a.Stats.mu.Lock()
	a.Stats.outboundReplaceTokens[reason]++
	a.Stats.mu.Unlock()
}
