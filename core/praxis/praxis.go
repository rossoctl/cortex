// Package praxis converts an AuthBridge [config.Config] into a Praxis
// proxy configuration (https://github.com/praxis-proxy/praxis).
//
// The two proxies solve overlapping problems with different vocabularies.
// AuthBridge describes a sidecar as a mode plus a set of listener addresses,
// and hangs behavior off an ordered plugin pipeline (inbound and outbound).
// Praxis describes a proxy as a list of bound listeners, each referencing
// named filter chains, where the chain terminates in a router that selects a
// cluster and a load_balancer that resolves that cluster to endpoints.
//
// The mapping this package implements:
//
//	AuthBridge                            Praxis
//	───────────────────────────────────── ──────────────────────────────────
//	listener.reverse_proxy_addr           listeners[name=inbound].address
//	listener.reverse_proxy_backend (URL)  load_balancer cluster endpoint
//	listener.transparent_inbound_addr     listeners[name=inbound].address
//	listener.forward_proxy_addr           listeners[name=outbound].address
//	stats.address (host only)             admin.address (host + port 9091)
//	/healthz, /readyz (:9091)             admin /healthy, /ready
//	mtls.mode=permissive                  tls.client_cert_mode=request
//	mtls.mode=strict                      tls.client_cert_mode=require
//	spiffe mirror files                   tls.certificates / tls.client_ca
//	pipeline.inbound.plugins              filter chain (see below)
//	pipeline.outbound.plugins             filter chain (see below)
//
// # Plugin translation and its limits
//
// Praxis's default cargo build (`cargo run -p praxis-proxy`, features
// `default = []`) compiles in no JWT-validation or RFC 8693 token-exchange
// filter. Praxis does ship a `policy` filter whose description covers exactly
// that ground — "multi-source JWT identity, APL route policy, RFC 8693 token
// exchange" — but it sits behind the off-by-default `policy-engine` cargo
// feature and is driven by a separate operator-supplied policy document
// rather than by fields inline in the proxy config. A default-feature binary
// rejects it outright:
//
//	invalid configuration: unknown filter type: 'policy'
//
// Praxis also validates each filter's fields strictly (unknown field on a
// filter entry is a hard error), so there is no way to smuggle AuthBridge's
// per-plugin config through as extra keys.
//
// This converter therefore translates faithfully rather than optimistically.
// Plugins with a real structural counterpart are emitted as Praxis filters
// (see [pluginFilters]); plugins whose enforcement has no default-build
// equivalent are recorded as comments in the generated YAML and returned in
// [Result.Unmapped]. Callers that need the auth enforcement itself should
// build Praxis with `--features policy-engine` and supply a policy document;
// see [Result.Unmapped] for the list to carry across.
//
// The output is designed so that
//
//	cargo run -p praxis-proxy -- -c /tmp/praxis-config.yaml
//
// parses it, passes Praxis's own validation (`praxis -t`), and proxies
// traffic to the same backend AuthBridge would have forwarded to.
package praxis

import (
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/pipeline"
	"gopkg.in/yaml.v3"
)

// Well-known names used in the generated config. Praxis requires listener,
// chain, and cluster names to be unique and to cross-reference exactly, so
// they are constants rather than inline literals.
const (
	// ListenerInbound is the listener name for AuthBridge's inbound
	// (reverse-proxy) side.
	ListenerInbound = "inbound"
	// ListenerOutbound is the listener name for AuthBridge's outbound
	// (forward-proxy) side.
	ListenerOutbound = "outbound"

	// ChainInbound / ChainOutbound are the filter-chain names referenced by
	// the corresponding listeners.
	ChainInbound  = "inbound"
	ChainOutbound = "outbound"

	// ClusterInbound is the cluster the inbound router selects: the
	// application AuthBridge sits in front of.
	ClusterInbound = "agent_backend"
	// ClusterOutbound is the cluster the outbound router selects when a
	// concrete egress destination is known.
	ClusterOutbound = "egress_default"

	// DirectionHeader is the header Envoy injects on AuthBridge's inbound
	// path and that AuthBridge strips before forwarding to the app. The
	// generated inbound chain strips it too, preserving that contract.
	DirectionHeader = "x-authbridge-direction"
)

// AdminPort is the port the generated Praxis admin endpoint binds by default.
//
// 9091 is AuthBridge's health-server port (hardcoded as ":9091" in each
// binary's StartHealthServer call), serving /healthz and /readyz. Praxis's
// admin endpoint is the closest counterpart: its /ready is the same readiness
// probe as /readyz, and /healthy the same liveness probe as /healthz — so a
// probe already pointed at 9091 keeps working against the generated proxy.
//
// The alternative was AuthBridge's stats port, 9093 (/stats, /config,
// /reload/status). Praxis's admin endpoint does also carry /metrics, which is
// stats-shaped, but the readiness/liveness pair is what an orchestrator wires
// up, and moving that would silently break existing probes.
const AdminPort = 9091

// defaultAdminAddr is the fallback admin bind. Loopback because Praxis rejects
// a non-loopback admin bind unless insecure_options.allow_public_admin is set.
const defaultAdminAddr = "127.0.0.1:9091"

// Config is the root Praxis configuration document.
//
// Field order follows the YAML that Praxis's own examples use (listeners
// first, then chains, then the optional top-level blocks) rather than the
// alphabetical order of the Rust struct, because the generated file is meant
// to be read by operators. Praxis's root struct sets serde
// deny_unknown_fields, so every key emitted here must exist upstream; the
// omitempty tags keep optional blocks out of the document entirely when
// they carry nothing.
type Config struct {
	Listeners    []Listener    `yaml:"listeners"`
	FilterChains []FilterChain `yaml:"filter_chains,omitempty"`
	Admin        *Admin        `yaml:"admin,omitempty"`
	// ShutdownTimeoutSecs mirrors Praxis's graceful-drain window. Emitted
	// only when non-zero so the document stays close to Praxis's defaults.
	ShutdownTimeoutSecs int              `yaml:"shutdown_timeout_secs,omitempty"`
	InsecureOptions     *InsecureOptions `yaml:"insecure_options,omitempty"`
}

// Listener is one bound socket. Name and Address are the only required
// fields upstream; the rest are omitted when empty so Praxis applies its
// own defaults (protocol http, no TLS, unlimited connections).
type Listener struct {
	Name         string   `yaml:"name"`
	Address      string   `yaml:"address"`
	FilterChains []string `yaml:"filter_chains,omitempty"`
	TLS          *TLS     `yaml:"tls,omitempty"`
}

// TLS is a listener's TLS block. AuthBridge's mTLS is symmetric and
// SPIRE-backed, so Certificates points at the SVID mirror files and ClientCA
// at the trust bundle the SPIFFE provider keeps fresh.
type TLS struct {
	Certificates []Certificate `yaml:"certificates,omitempty"`
	ClientCA     *ClientCA     `yaml:"client_ca,omitempty"`
	// ClientCertMode is "request" (accept both TLS and plaintext-authenticated
	// peers) or "require" (reject peers without a valid cert). Praxis rejects
	// request/require without a client_ca, so the two travel together.
	ClientCertMode string `yaml:"client_cert_mode,omitempty"`
}

// Certificate is one serving keypair.
//
// Comments are emitted above cert_path so the generated file explains where
// the referenced files come from — see [Certificate.MarshalYAML].
type Certificate struct {
	CertPath string `yaml:"cert_path"`
	KeyPath  string `yaml:"key_path"`
	// Comments are rendered as YAML comments immediately above cert_path.
	// Not a Praxis field — excluded from the marshalled output.
	Comments []string `yaml:"-"`
}

// MarshalYAML emits the keypair with Comments rendered above cert_path.
//
// Praxis reads certificates from files, but AuthBridge's SVID material comes
// from the in-process SPIFFE provider; the paths here are that provider's disk
// mirror. Whoever reads the generated config needs to know that, because the
// files do not exist unless something is writing them.
func (c Certificate) MarshalYAML() (any, error) {
	node := &yaml.Node{Kind: yaml.MappingNode}
	certKey := &yaml.Node{Kind: yaml.ScalarNode, Value: "cert_path"}
	if len(c.Comments) > 0 {
		// Join first, then wrap once: the comment lines are prose fragments of
		// one paragraph, so wrapping each independently would leave short
		// orphan lines wherever a fragment ended mid-sentence.
		certKey.HeadComment = strings.Join(
			wrapComment(strings.Join(c.Comments, " "), 72), "\n")
	}
	node.Content = append(node.Content,
		certKey, &yaml.Node{Kind: yaml.ScalarNode, Value: c.CertPath},
		&yaml.Node{Kind: yaml.ScalarNode, Value: "key_path"},
		&yaml.Node{Kind: yaml.ScalarNode, Value: c.KeyPath},
	)
	return node, nil
}

// ClientCA is the CA bundle peer certificates are verified against.
type ClientCA struct {
	CAPath string `yaml:"ca_path"`
}

// FilterChain is a named, reusable ordered list of filters.
type FilterChain struct {
	Name    string   `yaml:"name"`
	Filters []Filter `yaml:"filters,omitempty"`
}

// Filter is one entry in a chain.
//
// Praxis filter entries are *flat*: the filter's own typed fields sit
// directly alongside the structural keys (`filter`, `name`, `conditions`,
// `failure_mode`) rather than under a nested `config:` wrapper. Praxis then
// deserializes the per-filter fields into that filter's typed struct with
// unknown-field rejection. Fields is therefore marshalled inline via
// [Filter.MarshalYAML] rather than as a nested mapping — emitting a `config:`
// block would be rejected as an unknown field.
type Filter struct {
	// Type is the registered filter name, e.g. "router", "load_balancer".
	Type string
	// Comments are emitted as YAML comments immediately above the entry.
	// Used to record what an AuthBridge plugin meant when it has no
	// default-build Praxis counterpart.
	Comments []string
	// Fields are the filter's own typed fields, flattened into the entry.
	// Ordered so the generated YAML is deterministic.
	Fields []Field
}

// Field is one key/value pair of a filter's typed configuration.
type Field struct {
	Key   string
	Value any
}

// Route is a single router route. PathPrefix plus Cluster is the form this
// converter emits; Host is set when the AuthBridge route keyed on a host
// pattern that Praxis can express literally.
type Route struct {
	PathPrefix string `yaml:"path_prefix,omitempty"`
	Host       string `yaml:"host,omitempty"`
	Cluster    string `yaml:"cluster"`
}

// Cluster is a named set of upstream endpoints, declared inline on the
// load_balancer filter.
type Cluster struct {
	Name      string   `yaml:"name"`
	Endpoints []string `yaml:"endpoints"`
}

// HeaderPair is a name/value header entry for the headers filter.
type HeaderPair struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

// Admin is the admin listener serving /healthy, /ready, and /metrics.
type Admin struct {
	Address string `yaml:"address"`
	Verbose bool   `yaml:"verbose,omitempty"`
}

// InsecureOptions carries Praxis's security overrides. Only emitted when the
// conversion actually needs one; see [convertInsecureOptions].
type InsecureOptions struct {
	// AllowPublicAdmin lets the admin endpoint bind a non-loopback address.
	// AuthBridge deliberately binds its stats server on all interfaces so the
	// Rossoctl UI can reach it, which Praxis treats as a validation error
	// without this flag.
	AllowPublicAdmin bool `yaml:"allow_public_admin,omitempty"`
}

// Result is the outcome of a conversion: the Praxis config plus an account
// of what could not be represented.
type Result struct {
	// Config is the generated Praxis configuration.
	Config *Config
	// Unmapped lists AuthBridge plugins that have no counterpart in a
	// default-feature Praxis build, in pipeline order. Each entry is
	// human-readable, e.g.
	//   `inbound plugin "jwt-validation": no default-build Praxis filter
	//    performs JWT validation; use the policy filter (requires the
	//    policy-engine cargo feature) with an equivalent policy document`.
	//
	// Empty means every plugin was either translated or is a no-op for
	// Praxis. Non-empty is not an error: the generated config is still valid
	// and still proxies traffic, it just does not enforce those plugins.
	Unmapped []string
	// Warnings records structural facts the operator should know about the
	// generated config that are not per-plugin, e.g. an AuthBridge mode whose
	// listeners live outside AuthBridge itself.
	Warnings []string
}

// Options tunes a conversion.
type Options struct {
	// PolicyPath, when non-empty, is the path the generated Praxis policy
	// document will live at. Setting it makes [Convert] emit a `policy` filter
	// referencing that path in each pipeline stage whose AuthBridge plugins the
	// policy enforces, instead of an inert UNMAPPED marker.
	//
	// The path is written into the generated proxy config as the filter's
	// `config_path`, so it must be the path Praxis will see at runtime — an
	// absolute path, resolvable by the Praxis process. Convert does not create
	// or read the file; the caller writes it (see [BuildPolicy] and
	// [RenderPolicyResult]).
	//
	// Only set this when the policy document is actually being generated AND
	// Praxis is built with the `policy-engine` cargo feature. A default-feature
	// build rejects the `policy` filter outright ("unknown filter type:
	// 'policy'"), so emitting it unconditionally would break the default path.
	PolicyPath string

	// PolicyEnforces names the AuthBridge plugins the policy document at
	// PolicyPath enforces (e.g. "jwt-validation"), as reported by
	// [PolicyResult.Enforced]. Those plugins are then translated as the
	// `policy` filter rather than reported unmapped. Ignored when PolicyPath
	// is empty.
	PolicyEnforces []string
}

// Convert builds a Praxis configuration from an AuthBridge config.
//
// The AuthBridge config should already have had [config.ApplyPreset] applied
// (as the binaries do at boot), since Convert reads the resolved listener
// addresses rather than re-deriving mode defaults. A nil cfg is an error.
//
// Convert does not fail on plugins it cannot represent — those are reported
// via [Result.Unmapped] so the caller can decide. It fails only when the
// AuthBridge config cannot yield a usable Praxis document at all (e.g. no
// listener address to bind).
//
// Passing a nil opts is equivalent to the zero Options: no policy document, so
// auth plugins are reported unmapped. See [ConvertWithPolicy] for the common
// path of generating the policy and the proxy config together.
func Convert(cfg *config.Config, opts *Options) (*Result, error) {
	if cfg == nil {
		return nil, fmt.Errorf("praxis: nil AuthBridge config")
	}
	if opts == nil {
		opts = &Options{}
	}

	res := &Result{Config: &Config{}}

	switch cfg.Mode {
	case config.ModeProxySidecar, "":
		// The shape this converter targets: AuthBridge owns the listeners.
	case config.ModeEnvoySidecar:
		res.Warnings = append(res.Warnings,
			"mode envoy-sidecar: AuthBridge runs as an Envoy ext_proc callout and binds no "+
				"data-plane listener, so there is no listener address to translate. Praxis "+
				"replaces Envoy itself rather than the callout; the generated config uses the "+
				"proxy-sidecar listener fields if present.")
	default:
		return nil, fmt.Errorf("praxis: unknown AuthBridge mode %q", cfg.Mode)
	}

	roles := cfg.Listener.ActiveRoles()
	tls, tlsWarnings := convertMTLS(cfg)
	res.Warnings = append(res.Warnings, tlsWarnings...)

	// ── Inbound ──────────────────────────────────────────────────────────
	// Two inbound shapes map to one Praxis listener with different backends.
	// reverse-proxy interception has a fixed backend URL; transparent
	// interception recovers the backend per connection via SO_ORIGINAL_DST,
	// which Praxis has no equivalent for — so that becomes a warning and the
	// listener is emitted only when a concrete backend is known.
	if roles[config.RoleReverse] {
		addr, backend, err := inboundAddrAndBackend(cfg, res)
		if err != nil {
			return nil, err
		}
		if addr != "" && backend != "" {
			chain, unmapped := pluginFilters(cfg.Pipeline.Inbound.Plugins, directionInbound, opts)
			// Strip the direction header before the app sees it, matching
			// AuthBridge's inbound contract.
			chain = append(chain, Filter{
				Type: "headers",
				Comments: []string{
					"AuthBridge strips the Envoy-injected direction header before forwarding",
					"to the application; preserve that contract here.",
				},
				Fields: []Field{{Key: "request_remove", Value: []string{DirectionHeader}}},
			})
			chain = append(chain, routerAndLoadBalancer(ClusterInbound, []string{backend})...)

			res.Config.Listeners = append(res.Config.Listeners, Listener{
				Name:         ListenerInbound,
				Address:      normalizeBindAddr(addr),
				FilterChains: []string{ChainInbound},
				TLS:          tls,
			})
			res.Config.FilterChains = append(res.Config.FilterChains, FilterChain{
				Name:    ChainInbound,
				Filters: chain,
			})
			res.Unmapped = append(res.Unmapped, unmapped...)
		}
	}

	// ── Outbound ─────────────────────────────────────────────────────────
	// AuthBridge's outbound side is an HTTP *forward* proxy: the destination
	// comes from each request (absolute-form URI or CONNECT), not from
	// config. Praxis is a reverse proxy — its router selects a preconfigured
	// cluster — so there is no faithful translation of "forward proxy" as
	// such. What is translatable is per-host egress routing: when the
	// outbound pipeline declares concrete destination hosts, they become
	// router routes matched on Host.
	if roles[config.RoleForward] && cfg.Listener.ForwardProxyAddr != "" {
		chain, unmapped := pluginFilters(cfg.Pipeline.Outbound.Plugins, directionOutbound, opts)
		res.Unmapped = append(res.Unmapped, unmapped...)
		res.Warnings = append(res.Warnings,
			"listener.forward_proxy_addr translated as a reverse-proxy listener: AuthBridge's "+
				"outbound side is an HTTP forward proxy that resolves each request's destination "+
				"at request time (absolute-form URI or CONNECT), which Praxis's router — which "+
				"selects a preconfigured cluster — cannot express. Egress destinations must be "+
				"declared as routes/clusters; the generated listener carries a static_response "+
				"placeholder until they are.")

		// With no known egress destination there is nothing for a router to
		// select, and Praxis rejects a load_balancer whose cluster is never
		// selected. Emit a terminal static_response so the listener is valid
		// and its behavior is explicit rather than accidentally open.
		chain = append(chain, Filter{
			Type: "static_response",
			Comments: []string{
				"No egress destinations were derivable from the AuthBridge outbound",
				"pipeline. Replace with router + load_balancer entries naming the",
				"hosts this workload is allowed to reach.",
			},
			Fields: []Field{
				{Key: "status", Value: 502},
				{Key: "body", Value: "praxis: no egress route configured for this destination\n"},
			},
		})

		res.Config.Listeners = append(res.Config.Listeners, Listener{
			Name:         ListenerOutbound,
			Address:      normalizeBindAddr(cfg.Listener.ForwardProxyAddr),
			FilterChains: []string{ChainOutbound},
			TLS:          tls,
		})
		res.Config.FilterChains = append(res.Config.FilterChains, FilterChain{
			Name:    ChainOutbound,
			Filters: chain,
		})
	}

	if len(res.Config.Listeners) == 0 {
		return nil, fmt.Errorf(
			"praxis: AuthBridge config yields no Praxis listener (mode %q, roles %v): "+
				"Praxis requires at least one listener", cfg.Mode, cfg.Listener.Roles)
	}

	// ── Outbound listener fields with no Praxis counterpart ──────────────
	// Reported outside the forward-role branch above because both fields are
	// set independently of it, and because silence here is the failure mode
	// that matters: each one removes an enforcement boundary.
	if cfg.Listener.TransparentProxyAddr != "" {
		// The outbound transparent listener is AuthBridge's HARD egress guard:
		// iptables REDIRECTs the agent's bypass egress to it, and unlike
		// skip_hosts it is deliberately not self-exemptable by the agent's
		// chosen destination. The proxy-sidecar and lite presets default it to
		// :8082, so it is effectively always on for those shapes — which means
		// converting such a config silently removes a guard the operator never
		// explicitly enabled and so may not think to check for.
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"listener.transparent_proxy_addr (%s) has no Praxis counterpart and is NOT translated: "+
				"it is AuthBridge's hard egress guard, receiving iptables-REDIRECTed traffic and "+
				"recovering the original destination via SO_ORIGINAL_DST — a mechanism Praxis, which "+
				"routes to preconfigured clusters, cannot express. Egress that AuthBridge forced "+
				"through the outbound pipeline is unconstrained by the generated config. Note the "+
				"proxy-sidecar and lite presets set this by default, so it may be active without "+
				"appearing in the source config.", cfg.Listener.TransparentProxyAddr))
	}
	if len(cfg.Listener.SkipHosts) > 0 {
		// Direction of risk is the opposite of the guard above: skip_hosts
		// makes AuthBridge MORE permissive, so not translating it is not a
		// security regression. It still changes behavior — those hosts now run
		// the generated chain — and it silently drops the operator's intent.
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"listener.skip_hosts %v is NOT translated: AuthBridge bypasses the plugin pipeline and "+
				"session recording entirely for these destinations. The generated config has no "+
				"equivalent bypass, so traffic to them runs the outbound chain like any other. This "+
				"is more restrictive, not less — but if these are infrastructure destinations that "+
				"must not be subject to egress policy, express that with router routes instead.",
			cfg.Listener.SkipHosts))
	}

	// ── Top-level blocks ─────────────────────────────────────────────────
	res.Config.Admin, res.Config.InsecureOptions = convertAdmin(cfg)
	if cfg.TLSBridge != nil && cfg.TLSBridge.Mode == "enabled" {
		res.Warnings = append(res.Warnings,
			"tls_bridge is enabled but has no Praxis counterpart: AuthBridge terminates the "+
				"agent's outbound TLS with a per-agent signing CA so the outbound pipeline sees "+
				"decrypted HTTPS. Praxis terminates TLS only for traffic addressed to its own "+
				"listeners and does not forge leaves for arbitrary upstream hosts.")
	}
	if cfg.Session.SessionEnabled() {
		res.Warnings = append(res.Warnings,
			"session tracking is enabled but has no Praxis counterpart: AuthBridge's in-memory "+
				"session store and its :9094 events API are AuthBridge-specific. Praxis exposes "+
				"request metrics on the admin endpoint (/metrics) and per-request records via "+
				"the access_log filter instead.")
	}

	return res, nil
}

// inboundAddrAndBackend resolves the inbound bind address and the single
// upstream endpoint the router should select, across AuthBridge's two
// inbound interception shapes.
func inboundAddrAndBackend(cfg *config.Config, res *Result) (addr, backend string, err error) {
	if cfg.Listener.InboundTransparent() {
		// SO_ORIGINAL_DST has no Praxis equivalent: Praxis routes to
		// preconfigured clusters and never learns the port the client
		// originally addressed. Report it rather than inventing a backend.
		res.Warnings = append(res.Warnings,
			"listener.inbound_interception=transparent has no Praxis counterpart: AuthBridge "+
				"recovers each connection's original destination via SO_ORIGINAL_DST and forwards "+
				"there over loopback. Praxis routes to preconfigured clusters, so no inbound "+
				"listener was generated. Set listener.reverse_proxy_backend to the application's "+
				"address to emit one.")
		return "", "", nil
	}
	// Without both an address to bind and a backend to forward to there is no
	// inbound listener to generate. Warn rather than returning silently: the
	// inbound chain is where the `policy` filter lives, so dropping it drops
	// JWT enforcement — and BuildPolicy still produced a document, so the
	// caller would otherwise log "wrote Praxis policy, enforces=[jwt-validation]"
	// for a policy no generated listener loads. The transparent branch above
	// warns for the same reason; this keeps the two symmetric.
	if cfg.Listener.ReverseProxyAddr == "" || cfg.Listener.ReverseProxyBackend == "" {
		missing := "listener.reverse_proxy_backend"
		if cfg.Listener.ReverseProxyAddr == "" {
			missing = "listener.reverse_proxy_addr"
			if cfg.Listener.ReverseProxyBackend == "" {
				missing = "listener.reverse_proxy_addr and listener.reverse_proxy_backend"
			}
		}
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"the reverse role is active but %s is empty, so NO inbound listener was generated. "+
				"Any inbound plugins — including jwt-validation — are therefore not enforced by "+
				"the generated config, even if a policy document was written for them. Set the "+
				"missing field to emit the inbound listener.", missing))
		return "", "", nil
	}
	endpoint, err := endpointFromBackendURL(cfg.Listener.ReverseProxyBackend)
	if err != nil {
		return "", "", err
	}
	return cfg.Listener.ReverseProxyAddr, endpoint, nil
}

// routerAndLoadBalancer emits the terminal pair every Praxis HTTP chain that
// proxies to an upstream needs. Praxis validates that a load_balancer is
// preceded by a cluster-selecting filter and that every cluster a router
// selects is defined on the load_balancer, so the two are always emitted
// together and share the cluster name.
func routerAndLoadBalancer(cluster string, endpoints []string) []Filter {
	return []Filter{
		{
			Type: "router",
			Fields: []Field{{Key: "routes", Value: []Route{
				{PathPrefix: "/", Cluster: cluster},
			}}},
		},
		{
			Type: "load_balancer",
			Fields: []Field{{Key: "clusters", Value: []Cluster{
				{Name: cluster, Endpoints: endpoints},
			}}},
		},
	}
}

// convertMTLS maps AuthBridge's single symmetric mTLS mode onto a Praxis
// listener TLS block, and reports when the referenced files will not exist.
//
// AuthBridge sources SVID material from the in-process SPIFFE provider and
// mirrors it to disk for external readers; Praxis reads certificates from
// files, so the mirror paths are the integration point. Two things therefore
// have to be true for the generated TLS block to work, and neither is implied
// by the mtls block alone:
//
//   - A SPIFFE provider must be running (a top-level `spiffe:` block), or
//     nothing writes the files at all.
//   - Its file mirror must be on (`spiffe.mirror_files`, default true), or the
//     provider keeps SVIDs in memory only and Praxis has nothing to read.
//
// A returned warning is not fatal: the Praxis config still validates (Praxis
// checks cert paths at listener startup, not at config parse), so the operator
// gets a document plus an explicit statement of what is missing.
func convertMTLS(cfg *config.Config) (*TLS, []string) {
	if cfg.MTLS == nil {
		return nil, nil
	}
	mode := "request" // permissive: ask for a cert, allow peers without one
	if cfg.MTLS.ResolvedMode() == config.MTLSModeStrict {
		mode = "require" // strict: reject peers without a valid cert
	}
	dir := "/opt"
	if cfg.SPIFFE != nil && cfg.SPIFFE.MirrorDir != "" {
		dir = cfg.SPIFFE.MirrorDir
	}
	certPath := dir + "/svid.pem"
	keyPath := dir + "/svid_key.pem"
	bundlePath := dir + "/svid_bundle.pem"

	var (
		warnings []string
		comments []string
	)
	switch {
	case cfg.SPIFFE == nil:
		// The case the weather-service example hits: mtls is set but the
		// spiffe block is absent (or commented out), so no provider runs and
		// no SVID files are ever written.
		warnings = append(warnings, fmt.Sprintf(
			"mtls is configured (mode: %s) but the config has no top-level `spiffe:` block, so no "+
				"SPIFFE provider runs and %s will NOT be generated — nothing writes it. The "+
				"generated Praxis listeners reference %s, %s, and %s, and Praxis will fail to bind "+
				"them at startup while those files are absent. Add a `spiffe:` block (the provider "+
				"mirrors the files on every rotation), or drop the `mtls:` block to generate "+
				"plaintext listeners.",
			cfg.MTLS.ResolvedMode(), certPath, certPath, keyPath, bundlePath))
		comments = append(comments,
			"WARNING: these files will NOT exist. The AuthBridge config sets `mtls:` but has",
			"no top-level `spiffe:` block, so no SPIFFE provider runs and nothing writes",
			certPath+". Praxis will fail to bind this listener until the files are present.",
			"Add a `spiffe:` block to the AuthBridge config, or drop `mtls:` for plaintext.")
	case cfg.SPIFFE.MirrorFiles != nil && !*cfg.SPIFFE.MirrorFiles:
		// A provider runs but was explicitly told not to mirror, so the SVIDs
		// stay in memory where Praxis cannot reach them.
		warnings = append(warnings, fmt.Sprintf(
			"mtls is configured but spiffe.mirror_files is explicitly false, so the SPIFFE "+
				"provider keeps SVIDs in memory and does not write %s. Praxis reads certificates "+
				"from files and has no way to consume the in-process source, so it will fail to "+
				"bind these listeners. Set spiffe.mirror_files: true (the default) to generate "+
				"the files.", certPath))
		comments = append(comments,
			"WARNING: these files will NOT exist. spiffe.mirror_files is explicitly false, so",
			"the SPIFFE provider keeps SVIDs in memory and never writes "+certPath+".",
			"Set spiffe.mirror_files: true (the default) so Praxis can read them.")
	default:
		comments = append(comments,
			"AuthBridge's in-process spiffe.Provider writes "+certPath+" (and",
			"the key and trust bundle alongside it), refreshing them on every SVID",
			"rotation. Praxis reads its certificates from files and cannot consume the",
			"provider's in-memory source, so this disk mirror is the integration point.",
			"The files exist only while that provider is running with mirroring enabled.")
	}

	return &TLS{
		Certificates: []Certificate{{
			CertPath: certPath,
			KeyPath:  keyPath,
			Comments: comments,
		}},
		ClientCA:       &ClientCA{CAPath: bundlePath},
		ClientCertMode: mode,
	}, warnings
}

// convertAdmin maps AuthBridge's stats server onto Praxis's admin endpoint,
// which serves /healthy, /ready, and /metrics — covering the health and
// stats servers AuthBridge runs separately.
//
// AuthBridge deliberately binds its stats address on all interfaces so the
// Rossoctl UI can scrape it. Praxis rejects a non-loopback admin bind unless
// insecure_options.allow_public_admin is set, so a public bind is carried
// across together with the flag that makes it valid rather than being
// silently rewritten to loopback (which would break the UI).
// The admin endpoint binds on [AdminPort] rather than carrying the stats port
// across, because /ready and /healthy correspond to AuthBridge's /readyz and
// /healthz on 9091, not to the stats endpoints on 9093. Only the HOST from
// stats.address is reused — that carries the operator's intent about
// reachability (loopback vs all interfaces), which the port does not.
func convertAdmin(cfg *config.Config) (*Admin, *InsecureOptions) {
	addr := cfg.Stats.StatsAddress
	if addr == "" {
		return &Admin{Address: defaultAdminAddr}, nil
	}
	// Take the host from stats.address and pair it with the health port.
	// A malformed stats.address falls back to the default rather than being
	// propagated: it would fail Praxis's admin SocketAddr parse either way,
	// and this keeps one clear failure instead of two.
	host, _, err := net.SplitHostPort(normalizeBindAddr(addr))
	if err != nil {
		return &Admin{Address: defaultAdminAddr}, nil
	}
	adminAddr := net.JoinHostPort(host, strconv.Itoa(AdminPort))
	if isLoopbackAddr(adminAddr) {
		return &Admin{Address: adminAddr}, nil
	}
	// AuthBridge binds stats on all interfaces so the Rossoctl UI can scrape
	// it; Praxis rejects a non-loopback admin bind without this override.
	// Carried across with the flag that makes it valid rather than rewritten
	// to loopback, which would silently break that reachability.
	return &Admin{Address: adminAddr}, &InsecureOptions{AllowPublicAdmin: true}
}

// endpointFromBackendURL turns AuthBridge's backend URL
// ("http://localhost:8001") into a Praxis endpoint ("localhost:8001").
//
// Praxis endpoints are host:port authorities, not URLs. A missing port is
// filled from the scheme so the endpoint is always explicit.
func endpointFromBackendURL(raw string) (string, error) {
	// A bare authority ("localhost:8001") is accepted as-is: url.Parse would
	// read "localhost" as the scheme and leave no host.
	if !strings.Contains(raw, "//") {
		if _, _, err := net.SplitHostPort(raw); err == nil {
			return raw, nil
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("praxis: parsing listener.reverse_proxy_backend %q: %w", raw, err)
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf(
			"praxis: listener.reverse_proxy_backend %q has no host", raw)
	}
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		default:
			port = "80"
		}
	}
	return net.JoinHostPort(host, port), nil
}

// normalizeBindAddr turns AuthBridge's shorthand bind addresses into the
// explicit host:port form Praxis requires. AuthBridge accepts ":8080"
// (all interfaces); Praxis parses its address as a socket address and needs
// a host part.
func normalizeBindAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "0.0.0.0" + addr
	}
	return addr
}

// isLoopbackAddr reports whether a host:port binds only the loopback
// interface, matching Praxis's admin-endpoint rule (127.0.0.1 or [::1]).
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// direction labels a pipeline stage for diagnostics.
type direction string

const (
	directionInbound  direction = "inbound"
	directionOutbound direction = "outbound"
)

// pluginTranslation describes how one AuthBridge plugin maps to Praxis.
type pluginTranslation struct {
	// filters, when non-nil, is called to produce the Praxis filters this
	// plugin becomes. Nil means the plugin has no filter representation.
	filters func() []Filter
	// note explains the gap when filters is nil. Recorded in
	// [Result.Unmapped] and as a comment in the generated YAML.
	note string
}

// translations maps AuthBridge plugin names to their Praxis translation.
//
// Only plugins whose *enforcement* survives the translation get filters.
// AuthBridge's auth plugins (jwt-validation, token-exchange) and its
// policy/guardrail plugins have no default-build Praxis filter: Praxis's
// `policy` filter covers that ground but requires the off-by-default
// policy-engine cargo feature plus a separate policy document, so a
// default-feature binary rejects it as an unknown filter type. Those are
// recorded rather than dropped silently, because emitting nothing for a
// deny-capable plugin turns a closed door into an open one.
var translations = map[string]pluginTranslation{
	"jwt-validation": {
		note: "validates inbound JWTs (signature via JWKS, issuer, audience) and rejects with " +
			"401. No default-build Praxis filter performs JWT validation; Praxis's `policy` " +
			"filter does, but requires the policy-engine cargo feature and a policy document. " +
			"Without it the generated listener does NOT authenticate inbound requests.",
	},
	"token-exchange": {
		note: "performs RFC 8693 token exchange per outbound route and injects the result as " +
			"Authorization. Praxis's `policy` filter covers RFC 8693 delegation but requires " +
			"the policy-engine cargo feature; `credential_injection` injects only static " +
			"per-cluster credentials, not exchanged tokens.",
	},
	"opa":          {note: "evaluates OPA policy over the request. No default-build Praxis equivalent."},
	"ibac":         {note: "aligns outbound tool calls against the recorded inbound user intent. Requires AuthBridge's session store; no Praxis equivalent."},
	"sparc":        {note: "rewrites request bodies per policy. No default-build Praxis equivalent."},
	"context-guru": {note: "compacts LLM context in outbound inference bodies. No Praxis equivalent."},
	"token-budget": {note: "enforces a token budget across a session. Requires AuthBridge's session store; no Praxis equivalent."},
	"token-broker": {note: "brokers tokens via an external service. No default-build Praxis equivalent."},
	"cpex":         {note: "routes hooks through the CPEX policy framework. No Praxis equivalent."},
	"spiffe-identity": {note: "surfaces the peer's SPIFFE identity to later plugins. Praxis's " +
		"`peer_identity_trust` filter validates a downstream mTLS peer against a configured " +
		"allowlist, which is related but not equivalent (it gates rather than annotates)."},

	// Protocol parsers annotate the request for downstream plugins rather
	// than enforcing anything. Praxis has structurally similar filters, but
	// they promote fields to headers for routing rather than populating an
	// AuthBridge pipeline context, so translating them would imply an
	// equivalence that does not hold. Recorded as observations.
	"mcp-parser": {note: "parses MCP JSON-RPC bodies and classifies action vs protocol mechanics for " +
		"downstream guardrails. Praxis's `json_rpc` filter extracts JSON-RPC envelope metadata " +
		"(method/id/kind) to headers for routing — similar parsing, different purpose; it feeds " +
		"no guardrail. Left out so the generated config does not imply enforcement."},
	"a2a-parser":       {note: "parses A2A message bodies for downstream guardrails. No Praxis equivalent."},
	"inference-parser": {note: "parses LLM inference bodies (OpenAI/Anthropic wire) for downstream plugins. No Praxis equivalent."},
}

// pluginFilters translates a pipeline stage's plugins into Praxis filters,
// returning the filters plus a note per plugin that could not be translated.
//
// Plugins disabled via on_error: off are skipped entirely — AuthBridge drops
// them from the pipeline, so they are not gaps in the translation.
func pluginFilters(plugins []config.PluginEntry, dir direction, opts *Options) ([]Filter, []string) {
	var (
		filters  []Filter
		unmapped []string
	)
	// Plugins the generated policy document enforces. Those become a single
	// `policy` filter at the position of the first one, rather than an inert
	// marker — the policy engine runs them all from one document.
	//
	// Gated on the INBOUND direction: BuildPolicy reads only
	// cfg.Pipeline.Inbound.Plugins, so the document it produces describes
	// inbound identity alone. Matching on name irrespective of direction would
	// let an outbound jwt-validation entry emit a `policy` filter on the
	// outbound chain pointing at a document built from inbound plugins —
	// enforcing the wrong stage's rules on egress, and reporting the plugin as
	// translated when nothing in the document corresponds to it. If the policy
	// ever grows outbound plugins, this gate is what must change with it.
	enforced := map[string]bool{}
	if opts != nil && opts.PolicyPath != "" && dir == directionInbound {
		for _, n := range opts.PolicyEnforces {
			enforced[n] = true
		}
	}
	policyEmitted := false
	// A single access_log entry per chain gives the generated config the
	// per-request visibility AuthBridge provides through its session events,
	// and request_id supplies the correlation ID.
	filters = append(filters,
		Filter{Type: "request_id"},
		Filter{Type: "access_log"},
	)

	for _, p := range plugins {
		// off is a kill-switch: AuthBridge does not dispatch the plugin, so it is
		// not a translation gap.
		if p.OnError.Resolved() == pipeline.ErrorPolicyOff {
			continue
		}
		name := p.Name
		t, known := translations[name]
		switch {
		case enforced[name]:
			// Enforced by the generated policy document. Emit the `policy`
			// filter once per chain: one document carries every enforced
			// plugin, so a second entry would re-run the same engine.
			if policyEmitted {
				continue
			}
			policyEmitted = true
			filters = append(filters, policyFilter(opts.PolicyPath, opts.PolicyEnforces))
		case known && t.filters != nil:
			filters = append(filters, t.filters()...)
		case known:
			unmapped = append(unmapped, fmt.Sprintf("%s plugin %q: %s", dir, name, t.note))
			filters = append(filters, Filter{
				Type: "headers",
				Comments: []string{
					fmt.Sprintf("UNMAPPED AuthBridge plugin %q (%s):", name, dir),
					wrapNote(t.note),
					"This entry is inert — it records the gap; it does not enforce the plugin.",
				},
				Fields: []Field{{Key: "request_add", Value: []HeaderPair{{
					Name:  "x-authbridge-unmapped-" + name,
					Value: "not-enforced",
				}}}},
			})
		default:
			unmapped = append(unmapped, fmt.Sprintf(
				"%s plugin %q: unrecognized by the Praxis converter; no filter emitted", dir, name))
		}
	}
	return filters, unmapped
}

// policyFilter builds the `policy` filter entry referencing the generated
// policy document.
//
// require_protocol_metadata is set to false deliberately. It defaults to true
// upstream and fail-closes when the chain carries no `mcp.method` metadata from
// a protocol classifier filter — that classifier ships in the separate
// `praxis-ai` package and is not in the chains this converter generates. With
// the default left on, every request would be rejected for missing metadata
// rather than being judged on its token. False selects the identity-only
// enforcement path, which is exactly what AuthBridge's jwt-validation does.
// The flag is only consulted when the policy declares entity routes, and the
// generated policy declares none, but it is set explicitly so the intent
// survives someone later adding routes to the document.
func policyFilter(path string, enforces []string) Filter {
	comments := []string{
		"Praxis Policy Engine — enforces the AuthBridge plugins listed below.",
		"REQUIRES Praxis built with the policy-engine cargo feature:",
		"  cargo run --features policy-engine -p praxis-proxy -- -c <this file>",
		"A default-feature build rejects this filter as an unknown filter type.",
	}
	if len(enforces) > 0 {
		comments = append(comments,
			"Enforcing: "+strings.Join(enforces, ", "))
	}
	comments = append(comments,
		"require_protocol_metadata: false selects identity-only enforcement; the",
		"protocol classifier filter it would otherwise require ships in praxis-ai.")

	return Filter{
		Type:     "policy",
		Comments: comments,
		Fields: []Field{
			{Key: "config_path", Value: path},
			{Key: "require_protocol_metadata", Value: false},
		},
	}
}

// ConvertWithPolicy builds both documents together: the Praxis policy document
// enforcing what it can of the AuthBridge pipeline, and a proxy config whose
// `policy` filter references that document at policyPath.
//
// policyPath is the path Praxis will load the policy from at runtime; the
// caller is responsible for writing the rendered policy there. When the
// AuthBridge config declares nothing the policy engine can enforce, the
// returned PolicyResult has a nil Document and the proxy config carries no
// `policy` filter — so the caller should skip writing a policy file entirely.
//
// The returned Result carries the policy's fidelity warnings alongside its own,
// so a single caller-side loop surfaces every caveat about the generated pair.
// polOpts may be nil; see [PolicyOptions] for what it tunes (notably resolving
// the inbound audience from a mounted file).
func ConvertWithPolicy(cfg *config.Config, policyPath string, polOpts *PolicyOptions) (*Result, *PolicyResult, error) {
	pol, err := BuildPolicy(cfg, polOpts)
	if err != nil {
		return nil, nil, err
	}
	opts := &Options{}
	if pol.Document != nil {
		opts.PolicyPath = policyPath
		opts.PolicyEnforces = pol.Enforced
	}
	res, err := Convert(cfg, opts)
	if err != nil {
		return nil, nil, err
	}
	res.Warnings = append(res.Warnings, pol.Warnings...)
	return res, pol, nil
}

// wrapNote collapses a note to a single comment line, trimming interior
// whitespace so the emitted YAML comment stays on one line.
func wrapNote(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// sortedPluginNames returns the plugin names this converter knows about, for
// diagnostics and tests.
func sortedPluginNames() []string {
	out := make([]string, 0, len(translations))
	for k := range translations {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// KnownPlugins returns the AuthBridge plugin names the converter recognizes,
// sorted. A plugin absent from this list converts to a generic "unrecognized"
// entry in [Result.Unmapped] rather than being silently ignored.
func KnownPlugins() []string { return sortedPluginNames() }
