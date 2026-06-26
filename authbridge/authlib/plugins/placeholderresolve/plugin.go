// Package placeholderresolve provides an outbound pipeline plugin that
// resolves OpenShell-style credential placeholders in the Authorization
// header to their real secret values, on the wire, so the agent never holds
// the real credential.
//
// OpenShell injects an agent's credential env (e.g. ANTHROPIC_AUTH_TOKEN)
// as a placeholder string of the form "openshell:resolve:env:<KEY>". The
// agent emits that placeholder verbatim (for the `claude` provider it rides
// in `Authorization: Bearer <placeholder>` — the only header any listener
// reconciles to the upstream wire). This plugin scans the Authorization
// header, resolves each placeholder via the configured source, and rewrites
// the header in place. Unresolvable placeholders fail closed (the request is
// denied rather than forwarded with the placeholder).
//
// The credential source is selected by the required `source` field:
//   - "gateway": the OpenShell gateway (fetches the sandbox's resolved
//     provider environment; requires running as a sidecar in the sandbox pod).
//   - "secret_dir": a mounted directory, one file per KEY.
//
// The resolution + safe header-injection primitives live in authlib/credinject
// so a future host-keyed injector can reuse them.
package placeholderresolve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/credinject"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/placeholderresolve/gateway"
)

// defaultPrefix is the OpenShell placeholder prefix — kept identical to the
// supervisor's so agents are unchanged when AuthBridge replaces OpenShell's
// internal proxy (OpenShell secrets.rs PLACEHOLDER_PREFIX).
const defaultPrefix = "openshell:resolve:env:"

// Default OpenShell sidecar paths for the gateway source (match the k8s
// driver's pod-spec conventions).
const (
	defaultMTLSCertDir = "/etc/openshell-tls/client"
	defaultSATokenPath = "/var/run/secrets/openshell/token"
)

// Credential source discriminators for the `source` config field.
const (
	sourceGateway   = "gateway"
	sourceSecretDir = "secret_dir"
)

// placeholderResolveConfig is the plugin's config schema. Field tags drive both
// runtime decoding (json) and operator-facing schema introspection.
type placeholderResolveConfig struct {
	// Prefix is the placeholder prefix to match; the KEY following it is
	// matched against the OpenShell env-key grammar.
	Prefix string `json:"prefix" description:"Placeholder prefix to match before the env key." default:"openshell:resolve:env:"`

	// Source selects where credentials are resolved from. Required; there is
	// no implicit fallback — an unconfigured source fails closed.
	Source string `json:"source" required:"true" description:"Credential source: 'gateway' (OpenShell sandbox sidecar) or 'secret_dir' (mounted files)."`

	// Gateway configures the OpenShell-gateway source (source: gateway).
	Gateway *gatewayConfig `json:"gateway" description:"OpenShell gateway source config (used when source=gateway)."`

	// SecretDir is the directory for the file source (source: secret_dir):
	// each KEY is read from <SecretDir>/<KEY>.
	SecretDir string `json:"secret_dir" description:"Directory to resolve each KEY from a file named <KEY> (used when source=secret_dir)."`
}

// gatewayConfig configures the OpenShell-gateway resolver source.
type gatewayConfig struct {
	Endpoint    string `json:"endpoint" required:"true" description:"OpenShell gateway gRPC endpoint, e.g. https://openshell.<ns>.svc:8080."`
	MTLSCertDir string `json:"mtls_cert_dir" description:"Dir holding ca.crt/tls.crt/tls.key for mTLS to the gateway." default:"/etc/openshell-tls/client"`
	SATokenPath string `json:"sa_token_path" description:"Projected SA token file (audience openshell-gateway)." default:"/var/run/secrets/openshell/token"`
	SandboxID   string `json:"sandbox_id" required:"true" description:"This sandbox's id (OPENSHELL_SANDBOX_ID); must match the gateway-minted JWT."`
	Insecure    bool   `json:"insecure" description:"Permit plaintext gRPC to a non-loopback gateway (opt-in). Sends the SA token + JWT in cleartext; refused by default for non-loopback endpoints." default:"false"`
}

func (c *placeholderResolveConfig) applyDefaults() {
	if c.Prefix == "" {
		c.Prefix = defaultPrefix
	}
	if c.Source == sourceGateway && c.Gateway != nil {
		if c.Gateway.MTLSCertDir == "" {
			c.Gateway.MTLSCertDir = defaultMTLSCertDir
		}
		if c.Gateway.SATokenPath == "" {
			c.Gateway.SATokenPath = defaultSATokenPath
		}
	}
}

func (c *placeholderResolveConfig) validate() error {
	if c.Prefix == "" {
		return errors.New("prefix must not be empty")
	}
	switch c.Source {
	case sourceGateway:
		if c.Gateway == nil {
			return errors.New("source 'gateway' requires a gateway block")
		}
		if c.Gateway.Endpoint == "" {
			return errors.New("gateway.endpoint is required")
		}
		if c.Gateway.SandboxID == "" {
			return errors.New("gateway.sandbox_id is required")
		}
	case sourceSecretDir:
		if c.SecretDir == "" {
			return errors.New("source 'secret_dir' requires secret_dir")
		}
	case "":
		return errors.New("source is required (one of: gateway, secret_dir)")
	default:
		return fmt.Errorf("unknown source %q (want one of: gateway, secret_dir)", c.Source)
	}
	return nil
}

// PlaceholderResolve is the outbound plugin. cfg/resolver are immutable after
// Configure returns, so OnRequest reads them without synchronization.
type PlaceholderResolve struct {
	cfg      placeholderResolveConfig
	resolver credinject.Resolver
}

// New constructs an unconfigured plugin.
func New() *PlaceholderResolve { return &PlaceholderResolve{} }

func init() {
	plugins.RegisterPlugin("placeholder-resolve", func() pipeline.Plugin { return New() })
}

func (p *PlaceholderResolve) Name() string { return "placeholder-resolve" }

func (p *PlaceholderResolve) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		Description: "Resolve OpenShell credential placeholders in the Authorization header to real values (fail-closed).",
	}
}

// ConfigSchema surfaces field metadata to config-aware tooling. A non-nil
// Gateway is passed so the nested block's fields are reflected.
func (p *PlaceholderResolve) ConfigSchema() []pipeline.FieldSchema {
	return pipeline.SchemaOf(placeholderResolveConfig{Gateway: &gatewayConfig{}})
}

func (p *PlaceholderResolve) Configure(raw json.RawMessage) error {
	var c placeholderResolveConfig
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return fmt.Errorf("placeholder-resolve config: %w", err)
		}
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return fmt.Errorf("placeholder-resolve config: %w", err)
	}

	var resolver credinject.Resolver
	switch c.Source {
	case sourceGateway:
		gw, gerr := gateway.New(gateway.Config{
			Endpoint:    c.Gateway.Endpoint,
			MTLSCertDir: c.Gateway.MTLSCertDir,
			SATokenPath: c.Gateway.SATokenPath,
			SandboxID:   c.Gateway.SandboxID,
			Insecure:    c.Gateway.Insecure,
		})
		if gerr != nil {
			return fmt.Errorf("placeholder-resolve: gateway resolver: %w", gerr)
		}
		resolver = gw
	case sourceSecretDir:
		resolver = credinject.FileResolver{Dir: c.SecretDir}
	}

	// Commit only after all fallible construction succeeded, so a failed
	// Configure leaves the plugin in its zero (deny) state.
	p.cfg = c
	p.resolver = resolver
	return nil
}

// Init starts a lifecycle-capable resolver's background warm-up (the gateway
// source). Static resolvers are always ready.
func (p *PlaceholderResolve) Init(ctx context.Context) error {
	if lr, ok := p.resolver.(credinject.LifecycleResolver); ok {
		return lr.Start(ctx)
	}
	return nil
}

// Ready reports resolver readiness — always true for the static (file)
// resolver; gated on the primed cache for the gateway source.
func (p *PlaceholderResolve) Ready() bool {
	if lr, ok := p.resolver.(credinject.LifecycleResolver); ok {
		return lr.Ready()
	}
	return true
}

// Shutdown stops a lifecycle-capable resolver's background work.
func (p *PlaceholderResolve) Shutdown(ctx context.Context) error {
	if lr, ok := p.resolver.(credinject.LifecycleResolver); ok {
		return lr.Stop(ctx)
	}
	return nil
}

// authHeader is the only header any listener reconciles to the upstream wire,
// so it is the only header this plugin scans and rewrites.
const authHeader = "Authorization"

func (p *PlaceholderResolve) OnRequest(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
	if p.resolver == nil {
		return pipeline.DenyStatus(503, "upstream.unreachable", "placeholder-resolve not configured")
	}

	val := pctx.Headers.Get(authHeader)
	if val == "" {
		pctx.Skip("no_authorization")
		return pipeline.Action{Type: pipeline.Continue}
	}

	rewritten, found, ok := p.rewrite(ctx, val)
	if !found {
		pctx.Skip("no_placeholder")
		return pipeline.Action{Type: pipeline.Continue}
	}
	if !ok {
		// A placeholder was present but could not be resolved (unknown key or
		// unsafe value). Fail closed — never forward the placeholder upstream.
		return pctx.DenyAndRecord("placeholder_unresolved", "auth.unauthorized", "unresolvable credential placeholder")
	}
	if !credinject.SafeSetHeader(pctx.Headers, authHeader, rewritten) {
		return pctx.DenyAndRecord("placeholder_unsafe", "auth.unauthorized", "unsafe resolved credential value")
	}
	pctx.Modify("placeholder_resolved")
	return pipeline.Action{Type: pipeline.Continue}
}

func (p *PlaceholderResolve) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// rewrite replaces every placeholder (prefix + an OpenShell env key matching
// [A-Za-z_][A-Za-z0-9_]*) in val with its resolved value. Returns
// (rewritten, found, ok): found is true when at least one placeholder matched;
// ok is false when a matched placeholder could not be resolved or resolved to
// an unsafe value (the caller must fail closed). A prefix not followed by a
// valid key is left literal.
func (p *PlaceholderResolve) rewrite(ctx context.Context, val string) (string, bool, bool) {
	if !strings.Contains(val, p.cfg.Prefix) {
		return val, false, true
	}
	found := false
	var b strings.Builder
	i := 0
	for {
		rel := strings.Index(val[i:], p.cfg.Prefix)
		if rel < 0 {
			b.WriteString(val[i:])
			break
		}
		start := i + rel
		b.WriteString(val[i:start]) // text before the prefix
		keyStart := start + len(p.cfg.Prefix)
		keyEnd := keyStart
		for keyEnd < len(val) && isEnvKeyChar(val[keyEnd], keyEnd == keyStart) {
			keyEnd++
		}
		if keyEnd == keyStart {
			// Prefix not followed by a valid env key — leave it literal.
			b.WriteString(p.cfg.Prefix)
			i = keyStart
			continue
		}
		found = true
		v, okv := p.resolver.Resolve(ctx, val[keyStart:keyEnd])
		if !okv || !credinject.SafeHeaderValue(v) {
			return "", true, false // fail closed; the request is denied
		}
		b.WriteString(v)
		i = keyEnd
	}
	return b.String(), found, true
}

// isEnvKeyChar reports whether c is allowed in an OpenShell env key. The first
// character must be a letter or underscore; later characters may also be digits.
func isEnvKeyChar(c byte, first bool) bool {
	switch {
	case c == '_', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		return true
	case !first && c >= '0' && c <= '9':
		return true
	default:
		return false
	}
}

// Compile-time interface checks.
var (
	_ pipeline.Plugin       = (*PlaceholderResolve)(nil)
	_ pipeline.Configurable = (*PlaceholderResolve)(nil)
	_ pipeline.Initializer  = (*PlaceholderResolve)(nil)
	_ pipeline.Readier      = (*PlaceholderResolve)(nil)
	_ pipeline.Shutdowner   = (*PlaceholderResolve)(nil)
)
