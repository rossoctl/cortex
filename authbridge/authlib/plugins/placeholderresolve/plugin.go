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
// The Resolver is the swap seam: the default implementations resolve from
// an inline map (isolation testing), a mounted secret directory, or the
// process environment. A future gateway-gRPC resolver (calling OpenShell's
// GetSandboxProviderEnvironment) plugs in here without touching the plugin.
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

	// Mappings is an optional inline KEY->value map used as the resolver
	// source. Intended for isolation testing; takes precedence over
	// secret_dir and env when non-empty.
	Mappings map[string]string `json:"mappings" description:"Inline KEY->value resolver source (isolation testing)."`

	// SecretDir, when set (and Mappings empty), resolves each KEY by
	// reading <SecretDir>/<KEY> as a credential file.
	SecretDir string `json:"secret_dir" description:"Directory to resolve each KEY from a file named <KEY>."`
}

func (c *placeholderResolveConfig) applyDefaults() {
	if c.Prefix == "" {
		c.Prefix = defaultPrefix
	}
	if len(c.Headers) == 0 {
		c.Headers = []string{"Authorization"}
	}
}

func (c *placeholderResolveConfig) validate() error {
	if c.Prefix == "" {
		return errors.New("prefix must not be empty")
	}
	if len(c.Headers) == 0 {
		return errors.New("headers must list at least one header")
	}
	return nil
}

// Resolver maps a placeholder KEY to its real secret value. ok is false
// when the key is unknown — the plugin fails closed on a false. This is the
// swap seam: the gateway-gRPC source (B2) implements this interface.
type Resolver interface {
	Resolve(ctx context.Context, key string) (string, bool)
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

// ConfigSchema surfaces field metadata to config-aware tooling.
func (p *PlaceholderResolve) ConfigSchema() []pipeline.FieldSchema {
	return pipeline.SchemaOf(placeholderResolveConfig{})
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

	// Pick the resolver source. Mappings wins (deterministic, for tests),
	// then a mounted secret dir, else the process environment. The gateway
	// gRPC source (B2) will be selected here behind a new config option.
	var resolver Resolver
	switch {
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
)
