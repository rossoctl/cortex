// Package placeholderresolve provides an outbound pipeline plugin that
// resolves OpenShell-style credential placeholders in request headers to
// their real secret values, on the wire, so the agent never holds the
// real credential.
//
// OpenShell injects an agent's credential env (e.g. ANTHROPIC_AUTH_TOKEN)
// as a placeholder string of the form "openshell:resolve:env:<KEY>". The
// agent emits that placeholder verbatim (for the `claude` provider it
// rides in `Authorization: Bearer <placeholder>`); this plugin scans the
// configured headers, resolves each placeholder via an injected Resolver,
// and rewrites the header in place. Unresolvable placeholders fail closed
// (the request is denied rather than forwarded with the placeholder).
//
// The Resolver is the swap seam. The primary source is the OpenShell gateway
// (the `gateway` config block — fetches the sandbox's resolved provider
// environment via GetSandboxProviderEnvironment, requires running as a
// sidecar in the sandbox pod). For testing it can resolve from an inline
// map, a mounted secret directory, or the process environment.
package placeholderresolve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/config"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/placeholderresolve/gateway"
)

// defaultPrefix is the OpenShell placeholder prefix. Kept identical to the
// supervisor's so agents are unchanged when AuthBridge replaces OpenShell's
// internal proxy (OpenShell secrets.rs PLACEHOLDER_PREFIX).
const defaultPrefix = "openshell:resolve:env:"

// envKeyPattern is OpenShell's env-key grammar: a leading letter/underscore
// followed by letters, digits, or underscores. It bounds the KEY captured
// after the prefix and (because it admits no '/' or '.') also makes the
// file-source path join safe from traversal.
const envKeyPattern = `([A-Za-z_][A-Za-z0-9_]*)`

// Default OpenShell sidecar paths for the gateway source (match the k8s
// driver's pod-spec conventions).
const (
	defaultMTLSCertDir = "/etc/openshell-tls/client"
	defaultSATokenPath = "/var/run/secrets/openshell/token"
)

// placeholderResolveConfig is the plugin's local config schema. Field tags
// drive both runtime decoding (json) and operator-facing schema
// introspection (description / default).
type placeholderResolveConfig struct {
	// Prefix is the placeholder prefix to match. The KEY following it is
	// matched against the env-key grammar.
	Prefix string `json:"prefix" description:"Placeholder prefix to match before the env key." default:"openshell:resolve:env:"`

	// Headers lists the request headers to scan and rewrite. Defaults to
	// Authorization (the only header the forward proxy propagates to the
	// upstream wire today).
	Headers []string `json:"headers" description:"Request headers to scan for placeholders." default:"Authorization"`

	// Gateway, when set, resolves each KEY from the OpenShell gateway's
	// resolved provider environment (the native-provider source). Takes
	// precedence over mappings/secret_dir/env. Requires running as a sidecar
	// in the sandbox pod (shared SA + sandbox-id annotation).
	Gateway *gatewayConfig `json:"gateway" description:"Resolve credentials from the OpenShell gateway (sandbox sidecar)."`

	// Mappings is an optional inline KEY->value map used as the resolver
	// source. Intended for isolation testing.
	Mappings map[string]string `json:"mappings" description:"Inline KEY->value resolver source (isolation testing)."`

	// SecretDir, when set, resolves each KEY by reading <SecretDir>/<KEY> as
	// a credential file.
	SecretDir string `json:"secret_dir" description:"Directory to resolve each KEY from a file named <KEY>."`
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
	if len(c.Headers) == 0 {
		c.Headers = []string{"Authorization"}
	}
	if c.Gateway != nil {
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
	if len(c.Headers) == 0 {
		return errors.New("headers must list at least one header")
	}
	if c.Gateway != nil {
		if c.Gateway.Endpoint == "" {
			return errors.New("gateway.endpoint is required")
		}
		if c.Gateway.SandboxID == "" {
			return errors.New("gateway.sandbox_id is required")
		}
	}
	return nil
}

// Resolver maps a placeholder KEY to its real secret value. ok is false
// when the key is unknown — the plugin fails closed on a false.
type Resolver interface {
	Resolve(ctx context.Context, key string) (string, bool)
}

// lifecycleResolver is implemented by resolvers needing background warm-up
// (the gateway source). The map/file/env resolvers do not implement it and
// are treated as always-ready.
type lifecycleResolver interface {
	Start(ctx context.Context) error
	Ready() bool
	Stop(ctx context.Context) error
}

// mapResolver resolves from an inline map (isolation testing).
type mapResolver map[string]string

func (m mapResolver) Resolve(_ context.Context, key string) (string, bool) {
	v, ok := m[key]
	return v, ok
}

// envResolver resolves from the process environment. An unset or empty var
// is treated as unresolved (fail closed).
type envResolver struct{}

func (envResolver) Resolve(_ context.Context, key string) (string, bool) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

// fileResolver resolves each KEY by reading <dir>/<KEY>. The env-key grammar
// forbids '/' and '.' so the join cannot escape dir.
type fileResolver struct{ dir string }

func (f fileResolver) Resolve(_ context.Context, key string) (string, bool) {
	v, err := config.ReadCredentialFile(filepath.Join(f.dir, key))
	if err != nil {
		return "", false
	}
	return v, true
}

// PlaceholderResolve is the outbound plugin. cfg/re/resolver are immutable
// after Configure returns, so OnRequest reads them without synchronization.
type PlaceholderResolve struct {
	cfg      placeholderResolveConfig
	re       *regexp.Regexp
	resolver Resolver
}

// New constructs an unconfigured plugin.
func New() *PlaceholderResolve { return &PlaceholderResolve{} }

func init() {
	plugins.RegisterPlugin("placeholder-resolve", func() pipeline.Plugin { return New() })
}

func (p *PlaceholderResolve) Name() string { return "placeholder-resolve" }

func (p *PlaceholderResolve) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		Description: "Resolve OpenShell credential placeholders in headers to real values (fail-closed).",
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

	re, err := regexp.Compile(regexp.QuoteMeta(c.Prefix) + envKeyPattern)
	if err != nil {
		return fmt.Errorf("placeholder-resolve: compile prefix pattern: %w", err)
	}

	// Pick the resolver source. The gateway (native OpenShell provider) wins;
	// then inline mappings, a mounted secret dir, else the process env.
	var resolver Resolver
	switch {
	case c.Gateway != nil:
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
	case len(c.Mappings) > 0:
		resolver = mapResolver(c.Mappings)
	case c.SecretDir != "":
		resolver = fileResolver{dir: c.SecretDir}
	default:
		resolver = envResolver{}
	}

	// Commit only after all fallible construction succeeded, so a failed
	// Configure leaves the plugin in its zero state.
	p.cfg = c
	p.re = re
	p.resolver = resolver
	return nil
}

// Init starts a lifecycle-capable resolver's background warm-up (the gateway
// source). It returns promptly; readiness is reported via Ready so the
// pipeline gates traffic until the credential cache is primed.
func (p *PlaceholderResolve) Init(ctx context.Context) error {
	if lr, ok := p.resolver.(lifecycleResolver); ok {
		return lr.Start(ctx)
	}
	return nil
}

// Ready reports resolver readiness — always true for the static
// (map/file/env) resolvers; gated on the primed cache for the gateway source.
func (p *PlaceholderResolve) Ready() bool {
	if lr, ok := p.resolver.(lifecycleResolver); ok {
		return lr.Ready()
	}
	return true
}

// Shutdown stops a lifecycle-capable resolver's background work.
func (p *PlaceholderResolve) Shutdown(ctx context.Context) error {
	if lr, ok := p.resolver.(lifecycleResolver); ok {
		return lr.Stop(ctx)
	}
	return nil
}

func (p *PlaceholderResolve) OnRequest(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
	if p.resolver == nil || p.re == nil {
		return pipeline.DenyStatus(503, "upstream.unreachable", "placeholder-resolve not configured")
	}

	anyResolved := false
	for _, h := range p.cfg.Headers {
		val := pctx.Headers.Get(h)
		if val == "" {
			continue
		}
		rewritten, found, ok := p.rewrite(ctx, val)
		if !found {
			continue
		}
		if !ok {
			// A placeholder was present but could not be resolved (unknown
			// key or invalid value). Fail closed — never forward the
			// placeholder to the upstream.
			return pctx.DenyAndRecord("placeholder_unresolved", "auth.unauthorized", "unresolvable credential placeholder")
		}
		pctx.Headers.Set(h, rewritten)
		anyResolved = true
	}

	if anyResolved {
		pctx.Modify("placeholder_resolved")
	} else {
		pctx.Skip("no_placeholder")
	}
	return pipeline.Action{Type: pipeline.Continue}
}

func (p *PlaceholderResolve) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// rewrite replaces every placeholder in val with its resolved value.
// Returns (rewritten, found, ok): found is true when at least one
// placeholder matched; ok is false when a matched placeholder could not be
// resolved or resolved to an unsafe value (caller must fail closed).
func (p *PlaceholderResolve) rewrite(ctx context.Context, val string) (string, bool, bool) {
	found := false
	failed := false
	out := p.re.ReplaceAllStringFunc(val, func(match string) string {
		found = true
		key := strings.TrimPrefix(match, p.cfg.Prefix)
		v, ok := p.resolver.Resolve(ctx, key)
		if !ok || !validResolved(v) {
			failed = true
			return match // unchanged; the request is denied regardless
		}
		return v
	})
	if failed {
		return "", true, false
	}
	return out, found, true
}

// validResolved rejects resolved values carrying CR, LF, or NUL — guarding
// against header-injection (CWE-113) via a poisoned credential store.
func validResolved(s string) bool {
	return !strings.ContainsAny(s, "\r\n\x00")
}

// Compile-time interface checks.
var (
	_ pipeline.Plugin       = (*PlaceholderResolve)(nil)
	_ pipeline.Configurable = (*PlaceholderResolve)(nil)
	_ pipeline.Initializer  = (*PlaceholderResolve)(nil)
	_ pipeline.Readier      = (*PlaceholderResolve)(nil)
	_ pipeline.Shutdowner   = (*PlaceholderResolve)(nil)
)
