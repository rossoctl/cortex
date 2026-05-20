package jwtvalidation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/bypass"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/config"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/jwtvalidation/validation"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/routing"
)

// jwtValidationConfig is the plugin's local config schema. See
// authbridge/docs/plugin-reference.md for the decode → applyDefaults →
// validate pattern.
type jwtValidationConfig struct {
	// Issuer is the JWT `iss` claim expected on inbound tokens.
	// In split-horizon deployments this is the PUBLIC Keycloak URL
	// (whatever Keycloak stamps into the `iss` claim) — it only needs
	// to match bit-for-bit, not be reachable from inside the pod.
	Issuer string `json:"issuer"`

	// JWKSURL points at the JWKS endpoint used to verify signatures.
	// The sidecar actually GETs this URL from inside the cluster, so
	// in split-horizon deployments it must be the INTERNAL Keycloak
	// URL — not the public hostname from Issuer, which typically
	// won't resolve from inside the mesh.
	//
	// When empty, the URL is derived with this priority:
	//   1. KeycloakURL + KeycloakRealm (Keycloak convention; the
	//      internal URL, when the operator supplies it)
	//   2. Issuer (fallback for single-horizon deployments where the
	//      issuer hostname is reachable from inside the cluster)
	JWKSURL string `json:"jwks_url"`

	// KeycloakURL and KeycloakRealm are a convenience for deriving
	// JWKSURL from the internal Keycloak service URL, symmetric with
	// token-exchange's fields of the same name. Prefer supplying these
	// over JWKSURL when you also want the usual split-horizon behavior
	// (public Issuer + internal JWKS fetch).
	//
	// Pre-PR-#378 the binary derived jwks_url from outbound.token_url
	// via a cross-plugin pass; per-plugin configs don't share state,
	// so each plugin now carries its own copy of the "where is
	// Keycloak internally" hint.
	KeycloakURL   string `json:"keycloak_url"`
	KeycloakRealm string `json:"keycloak_realm"`

	// Audience is the literal audience value expected on inbound
	// tokens. One of {Audience, AudienceFile, AudienceMode:"per-host"}
	// is required.
	Audience string `json:"audience"`

	// AudienceFile reads the expected audience from a file. Used
	// together with client-registration's /shared/client-id.txt. The
	// file may not exist at Configure time; a background poll started
	// by Init waits for it and updates the plugin when it appears.
	//
	// Note: an empty-string value is treated as "unset" — applyDefaults
	// will fill in /shared/client-id.txt. To opt out of any file poll,
	// supply an explicit Audience instead; the file default only kicks
	// in when both Audience and AudienceFile are empty.
	AudienceFile string `json:"audience_file"`

	// AudienceMode chooses how the expected audience is resolved:
	// "static" (default) uses Audience/AudienceFile; "per-host" derives
	// it from pctx.Host via routing.ServiceNameFromHost (waypoint mode).
	AudienceMode string `json:"audience_mode"`

	// AllowedAudiences lists extra audience strings the sidecar accepts
	// for inbound JWT validation (OR semantics: the token's aud claim
	// must contain at least one of the configured values).
	//
	// Union order (first occurrence wins when deduplicating): each
	// `allowed_audiences` entry in config order, then literal `audience`,
	// then the value read from `audience_file` when used. Callers should
	// not rely on order for security decisions — only membership matters.
	//
	// Transitional bridge for deployments where tokens legitimately carry
	// multiple audiences (RFC 7519 string or array) and the agent must
	// accept more than the workload client ID alone — prefer aligning
	// IdP audience policy long-term. See plugin-reference.md.
	AllowedAudiences []string `json:"allowed_audiences"`

	// BypassPaths are URL path globs (see authlib/bypass) that skip
	// validation entirely.
	BypassPaths []string `json:"bypass_paths"`
}

func (c *jwtValidationConfig) applyDefaults() {
	// JWKSURL derivation priority:
	//   1. Explicit JWKSURL wins.
	//   2. KeycloakURL + KeycloakRealm → internal Keycloak URL (the
	//      reachable host for JWKS fetching in split-horizon setups).
	//   3. Issuer → same host as the token's iss claim (fine for
	//      single-horizon deployments; breaks split-horizon).
	if c.JWKSURL == "" {
		if c.KeycloakURL != "" && c.KeycloakRealm != "" {
			base := strings.TrimRight(c.KeycloakURL, "/") + "/realms/" + c.KeycloakRealm
			c.JWKSURL = base + "/protocol/openid-connect/certs"
		} else if c.Issuer != "" {
			c.JWKSURL = strings.TrimRight(c.Issuer, "/") + "/protocol/openid-connect/certs"
		}
	}
	if c.AudienceMode == "" {
		c.AudienceMode = "static"
	}
	// When neither Audience nor AudienceFile is set, fall back to the
	// Kagenti convention: client-registration writes the agent's client
	// ID (which doubles as the inbound audience) to this path.
	// Deployments that don't run client-registration should set
	// Audience explicitly — the Configure-time read is best-effort and
	// Init's poll will give up silently if ctx is cancelled.
	if c.AudienceMode == "static" && c.Audience == "" && c.AudienceFile == "" && len(c.AllowedAudiences) == 0 {
		c.AudienceFile = "/shared/client-id.txt"
	}
	if len(c.BypassPaths) == 0 {
		c.BypassPaths = bypass.DefaultPatterns
	}
}

func (c *jwtValidationConfig) validate() error {
	if c.Issuer == "" {
		return errors.New("issuer is required")
	}
	if c.JWKSURL == "" {
		return errors.New("jwks_url could not be derived; set it explicitly")
	}
	switch c.AudienceMode {
	case "static":
		// applyDefaults sets audience_file when audience, audience_file,
		// and allowed_audiences are all empty. Otherwise at least one
		// source supplies expected audiences (literal, file path, or
		// allowed_audiences list).
	case "per-host":
		// Audience derived at request time from pctx.Host — nothing to check.
	default:
		return fmt.Errorf("audience_mode must be static or per-host, got %q", c.AudienceMode)
	}
	return nil
}

// expectedAudiences merges allowed_audiences, literal audience, and an
// optional value read from audience_file (caller passes trimmed file
// contents, or "" when unreadable / unused). See AllowedAudiences field
// for union / dedup order.
func expectedAudiences(c jwtValidationConfig, audienceFromFile string) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for _, a := range c.AllowedAudiences {
		add(a)
	}
	add(c.Audience)
	add(audienceFromFile)
	return out
}

// JWTValidation validates inbound JWTs. Internal state is built during
// Configure and later updated by Init's background audience-file poller
// via auth.UpdateIdentity, which is atomic with respect to in-flight
// requests.
type JWTValidation struct {
	cfg             jwtValidationConfig
	inner           *auth.Auth
	audienceDeriver func(string) string

	// bgCancel stops the background audience-file poller started by
	// Init. It's created with context.Background() (not Init's ctx) so
	// the poller's lifetime is the plugin's lifetime, not Start's
	// 60-second budget — otherwise a slow client-registration during
	// pod boot would orphan the plugin after the initCtx deadline.
	//
	// Held in an atomic.Pointer so a future caller can invoke Shutdown
	// from a goroutine other than the one that ran Init without racing
	// the Init assignment. Today the pipeline serializes Start / Stop,
	// so the lock-free guarantee is future-proofing rather than a
	// correctness fix for current callers.
	bgCancel atomic.Pointer[context.CancelFunc]
}

// NewJWTValidation constructs an unconfigured plugin. Configure must be
// called before the pipeline accepts traffic.
func NewJWTValidation() *JWTValidation { return &JWTValidation{} }

func init() {
	plugins.RegisterPlugin("jwt-validation", func() pipeline.Plugin { return NewJWTValidation() })
}

func (p *JWTValidation) Name() string { return "jwt-validation" }

func (p *JWTValidation) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{Writes: []string{"security"}}
}

// Configure decodes the plugin's config subtree, applies defaults,
// validates, and constructs the internal auth handler. If AudienceFile
// is set but the file isn't yet readable (client-registration still
// provisioning during pod boot), the handler is created with an empty
// audience and Init's goroutine fills it in when the file appears.
func (p *JWTValidation) Configure(raw json.RawMessage) error {
	var c jwtValidationConfig
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return fmt.Errorf("jwt-validation config: %w", err)
		}
	}
	// Capture whether audience_file arrived explicitly so the boot-time
	// WARN can distinguish "operator pointed at the wrong path" from
	// "defaulted to the Kagenti convention and you might not have
	// noticed." applyDefaults fills AudienceFile in when both audience
	// and audience_file are empty, erasing the signal.
	audienceFileExplicit := c.AudienceFile != ""
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return fmt.Errorf("jwt-validation config: %w", err)
	}
	p.cfg = c

	if c.AudienceMode == "per-host" {
		p.audienceDeriver = routing.ServiceNameFromHost
	}

	var audienceFromFile string
	if c.Audience == "" && c.AudienceFile != "" {
		if v, err := config.ReadCredentialFile(c.AudienceFile); err == nil {
			audienceFromFile = v
		} else {
			// Boot-time visibility: operators see this in `kubectl logs`
			// of the initial pod instead of chasing 503s from traffic
			// that arrived before Init's poll filled the audience in.
			// When the path was defaulted (not written in the YAML),
			// spell that out so non-Kagenti deployers don't wonder why
			// the plugin is asking for /shared/client-id.txt.
			if audienceFileExplicit {
				slog.Warn("jwt-validation: audience_file not yet readable; Init will poll in background",
					"path", c.AudienceFile, "error", err)
			} else {
				slog.Warn("jwt-validation: audience_file defaulted to Kagenti convention and not yet readable; "+
					"Init will poll in background. Set audience (literal value) or audience_file (explicit path) "+
					"if you are not running under Kagenti.",
					"path", c.AudienceFile, "error", err)
			}
		}
	}

	audiences := expectedAudiences(c, audienceFromFile)

	matcher, err := bypass.NewMatcher(c.BypassPaths)
	if err != nil {
		return fmt.Errorf("jwt-validation bypass patterns: %w", err)
	}
	verifier := validation.NewLazyJWKSVerifier(c.JWKSURL, c.Issuer)
	p.inner = auth.New(auth.Config{
		Verifier: verifier,
		Bypass:   matcher,
		Identity: auth.IdentityConfig{Audiences: audiences, Issuer: c.Issuer},
	})
	return nil
}

// Init starts a background poll for AudienceFile when the file wasn't
// readable during Configure.
//
// The ctx passed to Init bounds synchronous initialization — not
// long-running watchers. The poller is spawned with a process-lifetime
// context (context.Background() + a cancel func stored in bgCancel)
// so Pipeline.Start's 60s init budget doesn't kill it. Shutdown
// cancels the watcher when the process is shutting down.
func (p *JWTValidation) Init(_ context.Context) error {
	// Skip background poll when not using audience_file, when a literal
	// audience was set in config, or when Configure already populated
	// identity (synchronous read from file or allowed_audiences-only).
	if p.cfg.AudienceFile == "" || p.cfg.Audience != "" || p.inner.Ready() {
		return nil
	}
	// Defensive guard: pipeline.Start contract says Init runs exactly
	// once per process, but a double-call would otherwise leak the
	// first goroutine (the first cancel func would be dropped on the
	// floor when we replaced bgCancel).
	if p.bgCancel.Load() != nil {
		return nil
	}
	bgCtx, cancel := context.WithCancel(context.Background())
	p.bgCancel.Store(&cancel)
	go func() {
		v, err := config.WaitForCredentialFile(bgCtx, p.cfg.AudienceFile)
		if err != nil {
			// Only reached when Shutdown cancels bgCtx — the file-wait
			// doesn't have a deadline of its own. Log at Debug so clean
			// shutdowns don't spam the log.
			slog.Debug("jwt-validation: audience_file wait stopped",
				"path", p.cfg.AudienceFile, "error", err)
			return
		}
		audiences := expectedAudiences(p.cfg, v)
		p.inner.UpdateIdentity(auth.IdentityConfig{Audiences: audiences, Issuer: p.cfg.Issuer}, nil)
		slog.Info("jwt-validation: audience loaded from file",
			"path", p.cfg.AudienceFile, "audiences", audiences)
	}()
	return nil
}

// Shutdown cancels the background audience-file poller if one was
// started by Init. Called by Pipeline.Stop during process shutdown.
// Safe to call more than once — the atomic swap makes the second call
// a no-op.
func (p *JWTValidation) Shutdown(_ context.Context) error {
	if cancel := p.bgCancel.Swap(nil); cancel != nil {
		(*cancel)()
	}
	return nil
}

func (p *JWTValidation) OnRequest(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
	if p.inner == nil {
		return pipeline.DenyStatus(503, "upstream.unreachable", "jwt-validation not configured")
	}
	authHeader := pctx.Headers.Get("Authorization")
	path := pctx.Path
	var audience string
	if p.audienceDeriver != nil {
		audience = p.audienceDeriver(pctx.Host)
	}

	result := p.inner.HandleInbound(ctx, authHeader, path, audience)
	if result.Action == auth.ActionDeny {
		// Surface the decision on pctx BEFORE returning so the listener's
		// reject path can record a SessionDenied event with diagnostic
		// context (why the token failed, what was expected). Never put
		// the raw token here — session store has no auth. The two-step
		// form (Record + Deny) is used here because we attach the
		// ExpectedIssuer / ExpectedAudience diagnostic fields that the
		// one-liner DenyAndRecord doesn't accept.
		pctx.Record(pipeline.Invocation{
			Action: pipeline.ActionDeny,
			Reason: result.DenyReasonCode.String(),
			Details: map[string]string{
				"expected_issuer":        p.cfg.Issuer,
				"expected_audiences":     strings.Join(p.inner.InboundAudiences(), ","),
				"expected_audience_host": audience,
			},
		})
		// result.DenyReason carries the specific failure (missing header,
		// audience mismatch, expired, etc.). Pick a code whose default
		// HTTP status matches what auth returned, so the fallback body is
		// meaningful even before auth.HandleInbound grows a structured
		// code of its own.
		code := "auth.unauthorized"
		if result.DenyStatus == 503 {
			code = "upstream.unreachable"
		}
		return pipeline.DenyStatus(result.DenyStatus, code, result.DenyReason)
	}

	// ActionAllow with nil Claims = bypass path (e.g., /healthz). Record
	// as a bypass event so operators can still see the request in the
	// session stream — useful for debugging "why is this URL skipping
	// JWT?" without hunting through slog lines.
	if result.Claims == nil {
		pctx.Skip("path_bypass")
		return pipeline.Action{Type: pipeline.Continue}
	}

	// ActionAllow with Claims = authorized. Surface what the plugin
	// VERIFIED in the token via the pipeline.Identity interface.
	// Plugins that don't run jwt-validation (SAML, mTLS, custom)
	// publish their own adapter; listeners read through the interface.
	pctx.Identity = claimsIdentity{c: result.Claims}
	pctx.Record(pipeline.Invocation{
		Action: pipeline.ActionAllow,
		Reason: auth.APPROVE_AUTHORIZED.String(),
		Details: map[string]string{
			"token_subject":  result.Claims.Subject,
			"token_audience": strings.Join(result.Claims.Audience, ","),
			"token_scopes":   strings.Join(result.Claims.Scopes, " "),
		},
	})

	// Capture the validated token into the process-level single-user
	// store so the outbound token-exchange plugin can use it as
	// subject_token when the agent didn't propagate the Authorization
	// header. The store itself is gated on the top-level
	// Config.SingleUserMode flag (auth.SetSingleUserModeEnabled, called
	// once at startup): when disabled, Store is a no-op and this branch
	// is harmless. See authbridge/docs/plugin-reference.md for the
	// single-player constraint.
	p.captureSingleUserToken(authHeader, result.Claims)
	return pipeline.Action{Type: pipeline.Continue}
}

// captureSingleUserToken caches the freshly-validated bearer for the
// outbound side to consume. Re-uses the jwt-validation-trusted exp from
// claims rather than re-parsing the JWT, and applies the package-level
// cap/floor policy in auth.CapSingleUserExpiry.
//
// Best-effort: any branch that returns early (empty token, near-expiry
// floor) is silent because the operational outcome is identical to
// "feature disabled" — the outbound side will fall through to its
// no-token policy on cache miss. The tripwire WARN fires only when a
// successful capture overwrites a different subject, which is the
// situation the operator needs to know about.
func (p *JWTValidation) captureSingleUserToken(authHeader string, claims *validation.Claims) {
	token := auth.ExtractBearer(authHeader)
	if token == "" {
		return
	}
	cappedExp, ok := auth.CapSingleUserExpiry(time.Now(), claims.ExpiresAt)
	if !ok {
		return
	}
	prevSub := auth.StoreSingleUserToken(token, claims.Subject, cappedExp)
	if prevSub != "" && prevSub != claims.Subject {
		slog.Warn("jwt-validation: single-user cached subject changed — single-player assumption violated; last-write-wins",
			"previous", prevSub,
			"current", claims.Subject)
	}
}

func (p *JWTValidation) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// Ready reports whether the plugin can validate traffic. auth.Auth's
// Ready() returns true once Identity.Audiences is non-empty, which the
// plugin's synchronous-load path in Configure or the async poll in
// Init (via auth.UpdateIdentity) arranges. Per-host mode skips the
// audience check because the audience is derived from pctx.Host per
// request rather than loaded up front.
func (p *JWTValidation) Ready() bool {
	if p.inner == nil {
		return false
	}
	if p.cfg.AudienceMode == "per-host" {
		return true
	}
	return p.inner.Ready()
}

// Stats returns the plugin's counter store for the /stats aggregator
// (see plugins.CollectStats). Returns nil when Configure hasn't run
// yet — aggregation code tolerates nils.
func (p *JWTValidation) Stats() *auth.Stats {
	if p.inner == nil {
		return nil
	}
	return p.inner.Stats
}

// Compile-time interface checks.
var (
	_ pipeline.Configurable = (*JWTValidation)(nil)
	_ pipeline.Initializer  = (*JWTValidation)(nil)
	_ pipeline.Shutdowner   = (*JWTValidation)(nil)
	_ pipeline.Readier      = (*JWTValidation)(nil)
	_ plugins.StatsSource   = (*JWTValidation)(nil)
)
