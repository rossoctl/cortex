// Package config provides YAML-based configuration with mode presets
// and startup validation for the AuthBridge auth layer.
package config

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"gopkg.in/yaml.v3"
)

// Config is the top-level AuthBridge configuration.
//
// Plugin-specific settings (inbound JWT validation, outbound token
// exchange, identity, bypass paths, routes) live inside their
// respective entries under Pipeline.* now — see the plugin reference at
// authbridge/docs/plugin-reference.md for how each plugin
// declares its own config schema and defaults.
type Config struct {
	Mode     string         `yaml:"mode" json:"mode"` // "envoy-sidecar", "waypoint", "proxy-sidecar"
	Listener ListenerConfig `yaml:"listener" json:"listener"`
	Pipeline PipelineConfig `yaml:"pipeline" json:"pipeline"`
	Session  SessionConfig  `yaml:"session" json:"session"`
	Stats    StatsConfig    `yaml:"stats" json:"stats"`

	// MTLS, when non-nil, enables transport-level mTLS using SPIRE
	// X.509 SVIDs. Applies symmetrically to inbound (reverse-proxy)
	// and outbound (forward-proxy) traffic in proxy-sidecar mode;
	// envoy-sidecar mode is unaffected (Envoy handles its own TLS via
	// SDS). Pointer so absent block = today's plaintext behavior.
	MTLS *MTLSConfig `yaml:"mtls,omitempty" json:"mtls,omitempty"`

	// SingleUserMode is the single control for the inbound→outbound
	// token-bridging feature. When enabled (the default), jwt-validation
	// caches each successfully-validated user JWT in a process-level
	// store, and token-exchange consults that store on outbound when no
	// Authorization header is present — bridging frameworks that don't
	// propagate the inbound header (LangChain, CrewAI, Claude Code).
	//
	// Pointer-bool with default-true semantics, mirroring SessionConfig.
	// nil means "unset → on"; explicit `false` opts the deployment out.
	//
	// This flag is intentionally at top level rather than per-plugin:
	// the feature requires the inbound and outbound sides to be
	// consistent, and a single knob removes the per-pipeline
	// inconsistency footgun.
	//
	// Constraint: only safe for processes handling one user at a time.
	// See authbridge/docs/plugin-reference.md for the single-player
	// caveat and multi-replica notes.
	SingleUserMode *bool `yaml:"single_user_mode" json:"single_user_mode"`
}

// SingleUserModeEnabled returns true when the inbound→outbound token
// bridge should run. Defaults to true when the field is unset, so
// operators must explicitly opt out with `single_user_mode: false`.
func (c Config) SingleUserModeEnabled() bool {
	if c.SingleUserMode == nil {
		return true
	}
	return *c.SingleUserMode
}

// MTLSMode names the inbound + outbound TLS posture. Vocabulary
// borrows from Istio's PeerAuthentication.mtls.mode for familiarity.
type MTLSMode string

const (
	// MTLSModePermissive accepts both TLS and plaintext on the
	// inbound side (byte-peek listener) and tries TLS on the outbound
	// side, falling back to plain TCP on handshake failure. The
	// rollout-friendly default; when an operator omits the mode
	// field, this is what they get.
	MTLSModePermissive MTLSMode = "permissive"
	// MTLSModeStrict accepts only TLS on the inbound side (byte-peek
	// closes non-TLS connections) and treats outbound TLS handshake
	// failures as hard errors with no fallback. Production posture
	// once the cluster is fully mTLS-capable.
	MTLSModeStrict MTLSMode = "strict"
)

// MTLSConfig is the top-level mTLS schema. One mode applies to both
// directions; if asymmetric needs surface later, this can split into
// separate Inbound / Outbound sub-blocks without breaking the
// existing flat shape.
type MTLSConfig struct {
	// Mode controls the inbound + outbound TLS posture. Defaults to
	// permissive when empty.
	Mode MTLSMode `yaml:"mode" json:"mode"`

	// CertFile / KeyFile / BundleFile point at spiffe-helper output.
	// Defaults to /opt/svid.pem, /opt/svid_key.pem, /opt/svid_bundle.pem
	// (matching the kagenti chart's helper.conf template).
	CertFile   string `yaml:"cert_file" json:"cert_file"`
	KeyFile    string `yaml:"key_file" json:"key_file"`
	BundleFile string `yaml:"bundle_file" json:"bundle_file"`
}

// ResolvedMode returns Mode with the empty-string default applied.
func (m *MTLSConfig) ResolvedMode() MTLSMode {
	if m == nil || m.Mode == "" {
		return MTLSModePermissive
	}
	return m.Mode
}

// Validate rejects unknown mode values at startup. Cert / key /
// bundle paths are validated lazily by the X509Source — operators
// can ship a config that points at not-yet-written files (cold
// start), and the source's wait-for-credential pattern handles it.
func (m *MTLSConfig) Validate() error {
	if m == nil {
		return nil
	}
	switch m.Mode {
	case "", MTLSModePermissive, MTLSModeStrict:
		return nil
	default:
		return fmt.Errorf("mtls.mode: %q is not a recognized value (use %q or %q)",
			m.Mode, MTLSModePermissive, MTLSModeStrict)
	}
}

// CheckPathsReadable stats the cert / key / bundle paths and returns
// the list of paths that are NOT yet readable. Empty list means
// everything is in place. Used by the cmd binaries at startup to
// emit an early WARN when a path is misconfigured (typo, wrong
// volume mount) — separates that case from the legitimate cold-start
// "spiffe-helper hasn't written yet" pattern, which the X509Source's
// per-handshake re-read already handles.
//
// The presence of unreadable paths is not a fatal error: returning
// them as a list lets the caller decide. cmd binaries log a WARN and
// continue; tests / verifiers can fail-fast if they want stricter
// semantics.
func (m *MTLSConfig) CheckPathsReadable() []string {
	if m == nil {
		return nil
	}
	var missing []string
	for _, p := range []string{m.CertFile, m.KeyFile, m.BundleFile} {
		if p == "" {
			continue // shouldn't happen post-Load (defaults applied)
		}
		if _, err := os.Stat(p); err != nil {
			missing = append(missing, p)
		}
	}
	return missing
}

// SessionConfig controls in-memory session tracking for cross-request correlation.
// When enabled, the framework records inbound intents and outbound tool calls so
// that guardrail plugins can evaluate sequences across request boundaries.
//
// Enabled is a pointer so the loader can distinguish "unset" (apply default)
// from "explicitly false" (user opted out). Default when unset: enabled.
type SessionConfig struct {
	// Enabled: nil means "unset → default on". Explicit `false` opts out.
	// Do not change to a plain bool — losing the nil sentinel would collapse
	// "user didn't say" with "user said false" and silently flip the default.
	Enabled     *bool  `yaml:"enabled" json:"enabled"`
	TTL         string `yaml:"ttl" json:"ttl"`                   // duration string; default: 30m
	MaxEvents   int    `yaml:"max_events" json:"max_events"`     // max events per session; default: 100
	MaxSessions int    `yaml:"max_sessions" json:"max_sessions"` // max concurrent sessions; default: 100 (0 = unlimited)
}

// SessionEnabled returns true when session tracking should run. Defaults to true
// when Enabled is unset, so operators need to explicitly opt out.
func (s SessionConfig) SessionEnabled() bool {
	if s.Enabled == nil {
		return true
	}
	return *s.Enabled
}

// PipelineConfig holds the plugin pipeline composition. Required:
// the runtime YAML must populate both inbound and outbound lists, or
// plugins.Build will produce empty pipelines and the listener will
// have nothing to invoke. There are no implicit defaults.
type PipelineConfig struct {
	Inbound  PipelineStageConfig `yaml:"inbound" json:"inbound"`
	Outbound PipelineStageConfig `yaml:"outbound" json:"outbound"`
}

// PipelineStageConfig lists the plugins for a pipeline stage in execution order.
type PipelineStageConfig struct {
	Plugins []PluginEntry `yaml:"plugins" json:"plugins"`
}

// PluginEntry names a plugin and optionally carries per-instance config.
//
// The YAML accepts both the bare-name form ("jwt-validation") and the
// full form ({name, id, on_error, config}). The short form keeps
// existing pipeline configs parsing unchanged; the long form is what
// plugins that implement pipeline.Configurable actually need. See
// authbridge/docs/plugin-reference.md for the convention plugins
// follow when decoding Config.
//
// Config is captured as a raw subtree via json.RawMessage so the plugin
// can do its own DisallowUnknownFields decode against a typed struct —
// the framework does not interpret it.
//
// OnError is the framework-owned wrapper policy (see ErrorPolicy).
// Plugin authors do not consume it — it lives outside the plugin's
// own config block so all plugins get the same rollout story without
// each one re-implementing shadow mode.
type PluginEntry struct {
	Name    string               `yaml:"name" json:"name"`
	ID      string               `yaml:"id,omitempty" json:"id,omitempty"`
	OnError pipeline.ErrorPolicy `yaml:"on_error,omitempty" json:"on_error,omitempty"`
	Config  json.RawMessage      `yaml:"-" json:"config,omitempty"`
}

// UnmarshalYAML accepts either a bare string or a map. The string form
// is equivalent to {name: <string>} with no config.
func (p *PluginEntry) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		p.Name = node.Value
		return nil
	case yaml.MappingNode:
		// Walk the mapping's Content pairs directly so we can preserve
		// the config subtree as raw bytes. yaml.v3's struct decode into
		// a *yaml.Node field produces nil in this version; iterating
		// Content is the reliable path.
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, val := node.Content[i], node.Content[i+1]
			if key.Kind != yaml.ScalarNode {
				return fmt.Errorf("plugin entry: non-scalar key %q", key.Value)
			}
			switch key.Value {
			case "name":
				if err := val.Decode(&p.Name); err != nil {
					return fmt.Errorf("plugin entry name: %w", err)
				}
			case "id":
				if err := val.Decode(&p.ID); err != nil {
					return fmt.Errorf("plugin entry id: %w", err)
				}
			case "on_error":
				var raw string
				if err := val.Decode(&raw); err != nil {
					return fmt.Errorf("plugin entry on_error: %w", err)
				}
				policy := pipeline.ErrorPolicy(raw)
				if !policy.Valid() {
					return fmt.Errorf("plugin entry on_error: %q is not a valid policy (expected: enforce, observe, off)", raw)
				}
				p.OnError = policy
			case "config":
				// Explicit `config: null` (or `config:` with no value)
				// decodes to a null-tagged scalar node. Normalize to
				// nil here — otherwise yamlNodeToJSON would emit the
				// literal bytes "null" and the Build-time "plugin does
				// not accept configuration" gate would fire
				// spuriously on non-Configurable plugins that a user
				// explicitly declared with a null config block.
				if val.Kind == yaml.ScalarNode && val.Tag == "!!null" {
					p.Config = nil
					continue
				}
				raw, err := yamlNodeToJSON(val)
				if err != nil {
					return fmt.Errorf("plugin %q config: %w", p.Name, err)
				}
				p.Config = raw
			default:
				return fmt.Errorf("plugin entry: unknown field %q", key.Value)
			}
		}
		return nil
	default:
		return fmt.Errorf("plugin entry: expected string or map, got kind %d", node.Kind)
	}
}

// yamlNodeToJSON converts a YAML node to JSON bytes by round-tripping
// through a generic Go value. Sufficient for config sub-trees, which
// only contain scalars, sequences, and maps.
func yamlNodeToJSON(n *yaml.Node) ([]byte, error) {
	var v any
	if err := n.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(normalizeYAMLMaps(v))
}

// normalizeYAMLMaps converts map[any]any (which yaml.v3 can produce when
// decoding into an untyped `any`) into map[string]any so json.Marshal
// accepts it. YAML allows non-string keys but config files never use them.
func normalizeYAMLMaps(v any) any {
	switch x := v.(type) {
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			ks, ok := k.(string)
			if !ok {
				ks = fmt.Sprintf("%v", k)
			}
			out[ks] = normalizeYAMLMaps(val)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = normalizeYAMLMaps(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = normalizeYAMLMaps(val)
		}
		return out
	default:
		return v
	}
}

// ListenerConfig holds per-mode listener addresses.
type ListenerConfig struct {
	ExtProcAddr         string `yaml:"ext_proc_addr" json:"ext_proc_addr"`
	ExtAuthzAddr        string `yaml:"ext_authz_addr" json:"ext_authz_addr"`
	ForwardProxyAddr    string `yaml:"forward_proxy_addr" json:"forward_proxy_addr"`
	ReverseProxyAddr    string `yaml:"reverse_proxy_addr" json:"reverse_proxy_addr"`
	ReverseProxyBackend string `yaml:"reverse_proxy_backend" json:"reverse_proxy_backend"`

	// SessionAPIAddr is the bind address for the session events HTTP server
	// (JSON snapshots + SSE stream consumed by abctl or curl). Default per
	// mode preset is ":9094". Set to empty string to disable the endpoint.
	SessionAPIAddr string `yaml:"session_api_addr" json:"session_api_addr"`
}

// StatsConfig represents the configuration for reporting config and statistics
type StatsConfig struct {
	StatsAddress string `yaml:"address" json:"address"` // for example, ":9093"
}

// Valid mode strings.
const (
	ModeEnvoySidecar = "envoy-sidecar"
	ModeWaypoint     = "waypoint"
	ModeProxySidecar = "proxy-sidecar"
)

// Load reads and parses a YAML config file with environment variable expansion.
// Defined env vars are expanded; undefined references like ${UNDEFINED} are left as-is
// to avoid silent empty-string substitution.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	expanded := os.Expand(string(data), func(key string) string {
		if val, ok := os.LookupEnv(key); ok {
			return val
		}
		return "${" + key + "}" // preserve undefined references
	})
	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	// Default stats server address
	if cfg.Stats.StatsAddress == "" {
		// Note that we default to an open port, not localhost 127.0.0.1:9093,
		// because the Kagenti UI needs to see this.  (If there are concerns
		// about the data exposed, use TLS or redact fields.)
		cfg.Stats.StatsAddress = ":9093"
	}

	// mTLS validation + path defaults. The cert paths default to
	// spiffe-helper's known output locations so operators can flip
	// `mtls: { mode: permissive }` without spelling them out.
	if err := cfg.MTLS.Validate(); err != nil {
		return nil, err
	}
	if cfg.MTLS != nil {
		if cfg.MTLS.CertFile == "" {
			cfg.MTLS.CertFile = "/opt/svid.pem"
		}
		if cfg.MTLS.KeyFile == "" {
			cfg.MTLS.KeyFile = "/opt/svid_key.pem"
		}
		if cfg.MTLS.BundleFile == "" {
			cfg.MTLS.BundleFile = "/opt/svid_bundle.pem"
		}
	}

	return &cfg, nil
}
