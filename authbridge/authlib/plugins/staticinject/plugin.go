package staticinject

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins"
)

// Source values for staticInjectConfig.Source.
const (
	sourceSecretDir = "secret_dir"
	sourceMappings  = "mappings"
)

// KeyBy values for staticInjectConfig.KeyBy.
const (
	keyByHost   = "host"
	keyByStatic = "static"
)

// staticInjectConfig is the plugin's local config schema. Named with a
// package-specific prefix (rather than the repo's common bare `config`)
// because this file also imports the shared authlib/config package under
// its own name.
//
// Field tags drive both runtime decoding (json) and operator-facing
// schema introspection (description / required / default / enum). See
// pipeline/schema.go for the consumer contract.
type staticInjectConfig struct {
	// Source selects where credential values come from: "secret_dir"
	// reads a file per key from SecretDir; "mappings" uses the inline
	// Mappings map (tests/dev only — do not put real secrets in YAML).
	Source string `json:"source" required:"true" description:"Credential source." enum:"secret_dir,mappings"`

	// SecretDir is the directory containing one file per credential
	// key, used when source=secret_dir.
	SecretDir string `json:"secret_dir" description:"Directory of per-key credential files; used when source=secret_dir."`

	// Mappings is an inline key->value credential map, used when
	// source=mappings. Tests/dev only.
	Mappings map[string]string `json:"mappings" description:"Inline key to credential map; used when source=mappings (tests/dev only)."`

	// KeyBy selects how the resolver key is derived: "host" (default)
	// uses the outbound request's destination host; "static" always
	// uses the configured Key.
	KeyBy string `json:"key_by" description:"How to derive the resolver key." default:"host" enum:"host,static"`

	// Key is the single lookup key used when key_by=static.
	Key string `json:"key" description:"Lookup key used when key_by=static."`

	// Placeholder, when set, requires the inbound bearer to equal this
	// exact string before injection proceeds. Enforces that the
	// workload never presents (and therefore never holds) a real
	// credential — only the agreed-upon placeholder.
	Placeholder string `json:"placeholder" description:"When set, only inject if the inbound bearer equals this exact placeholder string."`
}

func (c *staticInjectConfig) applyDefaults() {
	if c.KeyBy == "" {
		c.KeyBy = keyByHost
	}
}

func (c *staticInjectConfig) validate() error {
	switch c.Source {
	case sourceSecretDir:
		if c.SecretDir == "" {
			return fmt.Errorf("secret_dir is required when source is %q", sourceSecretDir)
		}
	case sourceMappings:
		if len(c.Mappings) == 0 {
			return fmt.Errorf("mappings is required when source is %q", sourceMappings)
		}
	default:
		return fmt.Errorf("source must be %q or %q, got %q", sourceSecretDir, sourceMappings, c.Source)
	}

	switch c.KeyBy {
	case keyByHost:
	case keyByStatic:
		if c.Key == "" {
			return fmt.Errorf("key is required when key_by is %q", keyByStatic)
		}
	default:
		return fmt.Errorf("key_by must be %q or %q, got %q", keyByHost, keyByStatic, c.KeyBy)
	}
	return nil
}

// buildResolver constructs the Resolver implied by c.Source. Callers must
// have already validated c.
func buildResolver(c staticInjectConfig) Resolver {
	switch c.Source {
	case sourceSecretDir:
		return FileResolver{Dir: c.SecretDir}
	case sourceMappings:
		return MapResolver(c.Mappings)
	default:
		// Unreachable after validate(), but return a resolver that
		// always fails closed rather than a nil interface that would
		// panic on use.
		return MapResolver(nil)
	}
}

// StaticInject is an outbound plugin that swaps a placeholder credential on
// the Authorization header for a real static credential resolved from a
// configured source (a secret-mounted file or, for tests/dev, an inline
// map). Built once via Configure; the zero value is a deny-everything
// plugin (fail-closed) until Configure succeeds.
type StaticInject struct {
	cfg      staticInjectConfig
	resolver Resolver
}

// New constructs an unconfigured plugin. Configure must be called before
// the pipeline accepts traffic.
func New() *StaticInject { return &StaticInject{} }

func init() {
	plugins.RegisterPlugin("static-inject", func() pipeline.Plugin { return New() })
}

func (p *StaticInject) Name() string { return "static-inject" }

func (p *StaticInject) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		Description: "Swaps a placeholder credential for a real static credential on outbound requests.",
	}
}

// ConfigSchema implements pipeline.SchemaProvider; surfaces field metadata
// to abctl edit templates and other config-aware tooling.
func (p *StaticInject) ConfigSchema() []pipeline.FieldSchema {
	return pipeline.SchemaOf(staticInjectConfig{})
}

func (p *StaticInject) Configure(raw json.RawMessage) error {
	var c staticInjectConfig
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return fmt.Errorf("static-inject config: %w", err)
		}
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return fmt.Errorf("static-inject config: %w", err)
	}

	resolver := buildResolver(c)

	// Commit cfg+resolver to the struct only after all validation and
	// construction succeeds — a failed Configure leaves the plugin in
	// its zero deny-state.
	p.cfg = c
	p.resolver = resolver

	return nil
}

// OnRequest implements the fail-closed injection sequence: missing/mismatched
// placeholder, unresolved key, or an unsafe resolved value all deny with 401
// and leave the Authorization header unchanged. Only a fully successful
// resolution swaps the header.
func (p *StaticInject) OnRequest(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
	if p.resolver == nil {
		return pipeline.DenyStatus(401, "static-inject.unconfigured", "static-inject plugin not configured")
	}

	bearer := auth.ExtractBearer(pctx.Headers.Get("Authorization"))
	if bearer == "" {
		return pipeline.DenyStatus(401, "static-inject.missing-auth", "missing bearer token on outbound request")
	}

	if p.cfg.Placeholder != "" && bearer != p.cfg.Placeholder {
		return pipeline.DenyStatus(401, "static-inject.placeholder-mismatch", "workload did not present the configured placeholder")
	}

	var key string
	switch p.cfg.KeyBy {
	case keyByStatic:
		key = p.cfg.Key
	default: // keyByHost
		key = pctx.Host
	}

	value, ok := p.resolver.Resolve(ctx, key)
	if !ok {
		return pipeline.DenyStatus(401, "static-inject.unresolved-key", "no credential available for the resolved key")
	}

	if !SafeSetHeader(pctx.Headers, "Authorization", "Bearer "+value) {
		return pipeline.DenyStatus(401, "static-inject.unsafe-value", "resolved credential value is unsafe to set as a header")
	}

	return pipeline.Action{Type: pipeline.Continue}
}

func (p *StaticInject) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// Compile-time interface checks.
var (
	_ pipeline.Plugin         = (*StaticInject)(nil)
	_ pipeline.Configurable   = (*StaticInject)(nil)
	_ pipeline.SchemaProvider = (*StaticInject)(nil)
)
