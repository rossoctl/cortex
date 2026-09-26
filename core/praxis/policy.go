package praxis

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/pipeline"
)

// This file generates the Praxis *policy document* — the second file the
// `policy` filter needs, referenced by its `config_path`. It is a separate
// document from the proxy config: the proxy config declares the filter chain,
// and the policy document declares the identity plugins and routes the policy
// engine enforces.
//
// The `policy` filter requires Praxis to be built with the `policy-engine`
// cargo feature:
//
//	cargo run --features policy-engine -p praxis-proxy -- -c /tmp/praxis-config.yaml
//
// A default-feature build rejects the filter with "unknown filter type:
// 'policy'", so [Convert] only emits it when a policy document is actually
// being generated alongside — see [Options.PolicyPath].

// Policy plugin wiring constants. These strings are part of the policy
// engine's public API: `identity/jwt` selects the JWT identity resolver, and
// `identity.resolve` is the hook it runs on.
const (
	policyPluginKindJWT  = "identity/jwt"
	policyHookIdentity   = "identity.resolve"
	policyModeSequential = "sequential"
	policyOnErrorFail    = "fail"
	policyClaimMapperStd = "standard"

	// policyDefaultLeewaySec is the clock-skew tolerance, in seconds, written
	// into each trusted issuer.
	//
	// Upstream treats leeway_seconds: 0 as "use the resolver default", and that
	// default is currently 60s (see the identity-jwt resolver: `if
	// issuer.leeway_seconds == 0 { 60 } else { ... }`). So 60 is not a departure
	// from upstream — it is upstream's effective value, stated explicitly rather
	// than left implicit. Emitting it makes the generated policy self-describing
	// and pins the behavior if that internal default ever changes; writing 0
	// would silently inherit whatever a future version picks.
	//
	// It IS slightly more permissive than AuthBridge, which sets no skew
	// tolerance and so takes jwx's strict default: a token up to 60s past `exp`
	// is accepted by the generated policy but rejected by AuthBridge. That is
	// the standard allowance for clock drift between the proxy and the IdP, and
	// tightening it to 0 would make the generated proxy reject tokens for
	// ordinary NTP jitter. Set trusted_issuers[].leeway_seconds in the generated
	// policy to override.
	policyDefaultLeewaySec = 60

	// policyJWTPluginPriority orders the identity plugin within its mode
	// band; lower runs first. Identity resolution must precede anything that
	// reads the resolved identity, so it takes a low number.
	policyJWTPluginPriority = 10

	// policyJWKSRefreshSecs matches the policy engine's own default (10
	// minutes): high enough not to hammer the IdP, low enough that a routine
	// key rotation propagates within a change window. Emitted explicitly so
	// the generated document states its refresh cadence rather than relying
	// on an upstream default that could change.
	policyJWKSRefreshSecs = 600
)

// PolicyDocument is the top-level Praxis policy document (the policy engine's
// "unified config"). Only the fields this converter populates are modeled.
//
// Routing is deliberately left off: with `plugin_settings.routing_enabled`
// absent (false), the engine resolves identity on the request phase and denies
// on a missing or invalid JWT, which is exactly AuthBridge's jwt-validation
// semantics. Turning routing on would shift enforcement to per-entity routes
// and require protocol-classifier metadata, changing behavior rather than
// translating it.
type PolicyDocument struct {
	Plugins []PolicyPlugin `yaml:"plugins"`
}

// PolicyPlugin is one plugin declaration in the policy document.
type PolicyPlugin struct {
	Name        string `yaml:"name"`
	Kind        string `yaml:"kind"`
	Description string `yaml:"description,omitempty"`
	// Hooks are the hook names this plugin handles, e.g. identity.resolve.
	Hooks []string `yaml:"hooks"`
	// Mode is the execution band: sequential can both block and modify,
	// which is what a deny-capable identity gate needs.
	Mode string `yaml:"mode,omitempty"`
	// Priority orders plugins within a mode band; lower runs first.
	Priority int `yaml:"priority,omitempty"`
	// OnError is the failure posture. "fail" halts the pipeline and
	// propagates the error — fail-closed, matching AuthBridge's rejection of
	// requests it cannot validate.
	OnError string `yaml:"on_error,omitempty"`
	// Config is the plugin's own typed configuration.
	Config any `yaml:"config,omitempty"`
}

// JWTIdentityConfig is the `identity/jwt` plugin's configuration.
type JWTIdentityConfig struct {
	// Header is the request header the token is read from; the "Bearer "
	// prefix is stripped if present.
	Header string `yaml:"header"`
	// ClaimMapper selects how claims map onto the identity. "standard" is
	// the OIDC default.
	ClaimMapper string `yaml:"claim_mapper,omitempty"`
	// TrustedIssuers is the set of accepted issuers. At least one required.
	TrustedIssuers []TrustedIssuer `yaml:"trusted_issuers"`
}

// TrustedIssuer is one accepted issuer: which `iss` to expect, which `aud`
// values to accept, which algorithms, and where the verification key comes
// from.
type TrustedIssuer struct {
	Issuer string `yaml:"issuer"`
	// Audiences are the accepted `aud` values (OR semantics). An empty list
	// disables audience validation upstream, so this converter never emits
	// an empty list without saying so — see [BuildPolicy].
	Audiences   []string    `yaml:"audiences,omitempty"`
	Algorithms  []string    `yaml:"algorithms"`
	DecodingKey DecodingKey `yaml:"decoding_key"`
	// LeewaySeconds is the clock-skew tolerance for exp/nbf validation.
	LeewaySeconds int `yaml:"leeway_seconds,omitempty"`
}

// DecodingKey is where JWT signing key material comes from. This converter
// emits the `jwks_url` form, since AuthBridge's jwt-validation verifies
// against a JWKS endpoint.
type DecodingKey struct {
	Kind string `yaml:"kind"`
	URL  string `yaml:"url,omitempty"`
	// InsecureHTTP permits a plaintext http:// JWKS URL. The policy engine
	// rejects http:// JWKS endpoints unless this is set, because anyone on
	// the network path could swap the key material and forge accepted JWTs.
	InsecureHTTP bool `yaml:"insecure_http,omitempty"`
	// RefreshSecs is how often the background task refetches the key set.
	RefreshSecs int `yaml:"refresh_secs,omitempty"`
}

// jwtValidationConfig mirrors the subset of AuthBridge's jwt-validation plugin
// config that the policy translation consumes. Decoded from the plugin entry's
// raw config subtree; unknown fields are ignored here because the plugin's own
// typed decode is the authority on validity.
type jwtValidationConfig struct {
	Issuer           string   `json:"issuer"`
	JWKSURL          string   `json:"jwks_url"`
	KeycloakURL      string   `json:"keycloak_url"`
	KeycloakRealm    string   `json:"keycloak_realm"`
	Audience         string   `json:"audience"`
	AudienceFile     string   `json:"audience_file"`
	AudienceMode     string   `json:"audience_mode"`
	AllowedAudiences []string `json:"allowed_audiences"`
	BypassPaths      []string `json:"bypass_paths"`
	// Algorithms is not a field jwt-validation defines today; it is read here
	// so that if the plugin gains one, the generated policy honors it instead
	// of silently keeping this converter's default. See [defaultJWTAlgorithms]
	// for why a default is needed at all.
	Algorithms []string `json:"algorithms"`
}

// defaultJWTAlgorithms is the algorithm set the generated policy accepts when
// the AuthBridge config names none.
//
// AuthBridge's verifier does not restrict algorithms: it calls
// jwt.Parse(..., jwt.WithKeySet(keySet)) and accepts whatever the JWKS key
// advertises (see core/plugins/jwtvalidation/validation/jwks.go). The policy
// engine, by contrast, REQUIRES a non-empty algorithms list per trusted issuer
// and verifies only those.
//
// So there is no way to express "whatever the JWKS says" in the policy, and any
// list this converter picks is narrower than AuthBridge. Picking RS256 alone —
// the previous behavior — means an ES256 or RS512 realm gets a policy that
// rejects every token AuthBridge accepted: a total inbound outage, with nothing
// in the config to hint at why. Enumerating the asymmetric families Keycloak
// and other OIDC providers actually sign with keeps the common cases working.
//
// HMAC (HS*) is deliberately excluded: those are symmetric, cannot be served
// over JWKS as a verification key in this shape, and accepting them alongside
// an asymmetric issuer invites algorithm-confusion attacks.
//
// Every entry must be a variant the policy engine's Algorithm enum accepts, or
// it rejects the whole document at startup ("unknown variant `X`, expected one
// of ..."). Upstream's asymmetric set is exactly these nine — note ES512 is NOT
// among them, though RS512 and PS512 are. TestBuildPolicy_DefaultAlgorithms_*
// and the binary-backed policy test pin this against the real engine.
var defaultJWTAlgorithms = []string{
	"RS256", "RS384", "RS512",
	"PS256", "PS384", "PS512",
	"ES256", "ES384",
	"EdDSA",
}

// resolveJWKSURL reproduces jwt-validation's derivation priority so the
// generated policy fetches keys from the same endpoint AuthBridge would:
//
//  1. explicit jwks_url
//  2. keycloak_url + keycloak_realm (the internal URL, for split-horizon)
//  3. issuer (single-horizon fallback)
//
// Keeping this in step with the plugin matters: pointing the policy at a
// different JWKS endpoint than AuthBridge used would either fail to verify
// tokens that AuthBridge accepted, or verify against the wrong key material.
func (c jwtValidationConfig) resolveJWKSURL() string {
	if c.JWKSURL != "" {
		return c.JWKSURL
	}
	if c.KeycloakURL != "" && c.KeycloakRealm != "" {
		return strings.TrimRight(c.KeycloakURL, "/") + "/realms/" + c.KeycloakRealm +
			"/protocol/openid-connect/certs"
	}
	if c.Issuer != "" {
		return strings.TrimRight(c.Issuer, "/") + "/protocol/openid-connect/certs"
	}
	return ""
}

// audiences returns the accepted audience values, mirroring jwt-validation's
// OR semantics: allowed_audiences entries in config order, then the literal
// audience, then fileAudience (the resolved contents of an audience file).
// Deduplicated, first occurrence winning.
//
// fileAudience is passed in rather than read here so that reading the
// filesystem stays an explicit, caller-controlled step — see
// [PolicyOptions.AudienceFile]. Empty means no file audience was resolved.
func (c jwtValidationConfig) audiences(fileAudience string) []string {
	var (
		out  []string
		seen = map[string]bool{}
	)
	add := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, a := range c.AllowedAudiences {
		add(a)
	}
	add(c.Audience)
	add(fileAudience)
	return out
}

// PolicyOptions tunes policy generation.
type PolicyOptions struct {
	// AudienceFile is a file to read the expected inbound audience from when
	// the plugin config does not state one literally.
	//
	// jwt-validation's own default is to read the audience from
	// /shared/client-id.txt — the Rossoctl convention, where the operator
	// mounts the workload's client ID as a Secret. That is a real audience, it
	// just is not in the YAML, so refusing to convert such a config would
	// reject the most common in-cluster shape. Naming the file here lets the
	// audience be resolved into the generated policy.
	//
	// Reading the filesystem is opt-in and explicit rather than automatic on
	// jwt-validation's `audience_file`: the generator may run somewhere other
	// than the pod that will serve traffic, and silently baking in whatever
	// happened to be on the generating machine's disk — under a path the
	// operator never named — would produce a policy whose audience nobody
	// chose. Passing the path is the operator saying "this file is the
	// authority."
	//
	// When empty, no file is read. When set but unreadable, missing, or empty,
	// [BuildPolicy] returns an error rather than falling through to an
	// audience-less policy, since that would be fail-open.
	//
	// Precedence matches jwt-validation: an explicit `audience` /
	// `allowed_audiences` in the plugin config is used as well, unioned with
	// the file's value under the same OR semantics.
	AudienceFile string
}

// resolveAudienceFile returns the audience read from opts.AudienceFile, or ""
// when no file was configured. The plugin's own `audience_file` value is used
// only for diagnostics — it names the runtime path, which may not exist here.
func resolveAudienceFile(opts *PolicyOptions) (string, error) {
	if opts == nil || opts.AudienceFile == "" {
		return "", nil
	}
	aud, err := config.ReadCredentialFile(opts.AudienceFile)
	if err != nil {
		return "", fmt.Errorf(
			"praxis: reading audience file %q: %w (it was named explicitly, so an unreadable "+
				"or empty file is an error rather than a fallback — a policy without an audience "+
				"accepts any token from the trusted issuer)", opts.AudienceFile, err)
	}
	return aud, nil
}

// PolicyResult is the outcome of building a policy document.
type PolicyResult struct {
	// Document is the generated policy document. Nil when no AuthBridge
	// plugin in the pipeline maps onto a policy plugin, in which case no
	// policy file should be written and no `policy` filter emitted.
	Document *PolicyDocument
	// Enforced lists the AuthBridge plugins whose enforcement the policy
	// document carries, e.g. "jwt-validation".
	Enforced []string
	// Warnings records fidelity gaps in the generated policy — settings that
	// could not be translated exactly and that change what the proxy accepts.
	Warnings []string
}

// BuildPolicy generates a Praxis policy document enforcing JWT validation from
// an AuthBridge config's inbound pipeline.
//
// It reads the `jwt-validation` plugin's config (issuer, JWKS URL derivation,
// audiences) and emits an `identity/jwt` policy plugin that verifies the same
// tokens: same issuer, same audiences, same JWKS endpoint, fail-closed on a
// missing or invalid token.
//
// Returns a Document of nil when the inbound pipeline declares no plugin that
// maps onto a policy plugin — there is nothing to enforce, so no policy file
// should be written. A jwt-validation entry disabled with `on_error: off` is
// skipped, matching AuthBridge dropping it from the pipeline.
//
// An error is returned only when a jwt-validation plugin is present but cannot
// be translated into something that would actually verify tokens (no issuer, no
// derivable JWKS URL, or no resolvable audience) — emitting a policy that
// accepts everything, or one the engine rejects at startup, would both be worse
// than failing here.
//
// opts may be nil. Set [PolicyOptions.AudienceFile] to convert a config whose
// audience lives in a mounted file rather than in the YAML — the common
// in-cluster shape, since jwt-validation defaults to reading
// /shared/client-id.txt.
func BuildPolicy(cfg *config.Config, opts *PolicyOptions) (*PolicyResult, error) {
	if cfg == nil {
		return nil, fmt.Errorf("praxis: nil AuthBridge config")
	}
	res := &PolicyResult{}

	// Read the audience file once, before the loop: every jwt-validation entry
	// in a stage resolves against the same file, and a read error should fail
	// the conversion regardless of how many plugins would have used it.
	fileAudience, err := resolveAudienceFile(opts)
	if err != nil {
		return nil, err
	}
	if fileAudience != "" {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"the inbound audience %q was read from %q at generation time and baked into the "+
				"policy. AuthBridge re-reads that file at runtime and picks up changes on restart; "+
				"the generated policy does not. If the workload's client ID is rotated, regenerate "+
				"the policy.", fileAudience, opts.AudienceFile))
	}

	for _, p := range cfg.Pipeline.Inbound.Plugins {
		if p.Name != "jwt-validation" {
			continue
		}
		// off is a kill-switch: AuthBridge does not dispatch the plugin at all,
		// so there is nothing to translate and no gap to report.
		if p.OnError.Resolved() == pipeline.ErrorPolicyOff {
			continue
		}
		// observe is shadow mode: the plugin still evaluates and may still
		// return Reject, but the framework converts that Reject into a
		// pass-through and records it as Shadow=true. So an operator canarying
		// jwt-validation in observe is DELIBERATELY letting unauthenticated
		// traffic reach the app while they watch the shadow-deny counter.
		//
		// The policy engine has no equivalent — its OnError covers plugin
		// errors, not deny decisions, and there is no dry-run mode — so the
		// generated plugin necessarily gets on_error: fail and enforces for
		// real. That inverts the operator's intent: traffic they were
		// intentionally admitting starts getting 401s.
		//
		// This is a warning rather than an error because the flip is toward
		// MORE enforcement, not less: the generated proxy is stricter than the
		// AuthBridge config, so no request that AuthBridge would have blocked
		// gets through. Failing the conversion would block a legitimate,
		// documented rollout shape over a difference that cannot cause a
		// bypass. But it can cause an outage, so it must be stated loudly.
		if p.OnError.Resolved() == pipeline.ErrorPolicyObserve {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"jwt-validation is configured with on_error: %s (shadow mode), which in AuthBridge "+
					"evaluates the plugin but converts a rejection into a pass-through — "+
					"unauthenticated requests reach the application and are only counted. The policy "+
					"engine has no shadow equivalent, so the generated policy ENFORCES: requests that "+
					"AuthBridge currently admits will get 401. If this is a canary rollout, expect "+
					"the generated proxy to be stricter than what you are canarying.",
				pipeline.ErrorPolicyObserve))
		}
		plugin, warnings, err := jwtPolicyPlugin(p, fileAudience, opts)
		if err != nil {
			return nil, err
		}
		if res.Document == nil {
			res.Document = &PolicyDocument{}
		}
		res.Document.Plugins = append(res.Document.Plugins, plugin)
		res.Enforced = append(res.Enforced, p.Name)
		res.Warnings = append(res.Warnings, warnings...)
	}

	return res, nil
}

// jwtPolicyPlugin translates one jwt-validation entry into an identity/jwt
// policy plugin. fileAudience is the value resolved from
// [PolicyOptions.AudienceFile], or "" when no file was configured.
func jwtPolicyPlugin(entry config.PluginEntry, fileAudience string, opts *PolicyOptions) (PolicyPlugin, []string, error) {
	var jc jwtValidationConfig
	if len(entry.Config) > 0 {
		if err := json.Unmarshal(entry.Config, &jc); err != nil {
			return PolicyPlugin{}, nil, fmt.Errorf(
				"praxis: decoding jwt-validation config: %w", err)
		}
	}

	if jc.Issuer == "" {
		return PolicyPlugin{}, nil, fmt.Errorf(
			"praxis: jwt-validation has no issuer, so no policy could be generated that " +
				"verifies tokens; set issuer in the plugin config")
	}
	jwks := jc.resolveJWKSURL()
	if jwks == "" {
		return PolicyPlugin{}, nil, fmt.Errorf(
			"praxis: jwt-validation jwks_url could not be derived for issuer %q; set "+
				"jwks_url, or keycloak_url + keycloak_realm", jc.Issuer)
	}

	var warnings []string

	// The JWKS URL must be a well-formed http:// or https:// URL, and nothing
	// else, before it is written into the policy.
	//
	// resolveJWKSURL builds this by string concatenation from issuer /
	// keycloak_url, so a malformed keycloak_url produces a malformed result. If
	// that were merely parsed-and-ignored, insecure_http would stay false and no
	// warning would fire, while the garbage URL was still written to
	// DecodingKey.URL — a key source that is undefined at runtime but reads as
	// secure in the file. Requiring a recognized scheme up front turns that into
	// a startup-time conversion error instead.
	u, parseErr := url.Parse(jwks)
	switch {
	case parseErr != nil:
		return PolicyPlugin{}, nil, fmt.Errorf(
			"praxis: jwt-validation JWKS endpoint %q is not a valid URL: %w (derived from "+
				"jwks_url, or keycloak_url + keycloak_realm — check those for typos)",
			jwks, parseErr)
	case u.Scheme != "http" && u.Scheme != "https":
		return PolicyPlugin{}, nil, fmt.Errorf(
			"praxis: jwt-validation JWKS endpoint %q has scheme %q; the policy engine fetches "+
				"JWKS over http or https only. A missing scheme usually means keycloak_url was "+
				"set without one", jwks, u.Scheme)
	case u.Host == "":
		return PolicyPlugin{}, nil, fmt.Errorf(
			"praxis: jwt-validation JWKS endpoint %q has no host", jwks)
	}

	// The policy engine rejects a plaintext http:// JWKS URL unless
	// insecure_http is set. AuthBridge does not have this guard, so a local /
	// demo config that works under AuthBridge would fail Praxis startup
	// outright. Set the flag to preserve behavior, and say so — over plaintext
	// anyone on the path can swap the key material and forge accepted JWTs.
	insecureHTTP := u.Scheme == "http"
	if insecureHTTP {
		warnings = append(warnings, fmt.Sprintf(
			"jwt-validation JWKS endpoint %q is plaintext http://, so the generated policy sets "+
				"decoding_key.insecure_http: true (the policy engine rejects http:// JWKS URLs "+
				"otherwise). Anyone on the network path to that endpoint can substitute key "+
				"material and forge tokens this proxy will accept. Acceptable for local "+
				"development; use https for anything else.", jwks))
	}

	// Algorithms: honor the plugin's list if it ever grows one, else default to
	// the asymmetric families. See [defaultJWTAlgorithms] for why the default
	// cannot be a single algorithm.
	algs := jc.Algorithms
	if len(algs) == 0 {
		algs = defaultJWTAlgorithms
		warnings = append(warnings, fmt.Sprintf(
			"jwt-validation names no signing algorithms (it does not restrict them: its verifier "+
				"accepts whatever the JWKS key advertises), but the policy engine requires an "+
				"explicit list per issuer. The generated policy accepts %v. If issuer %q signs "+
				"with something outside that set, every token will be rejected — set "+
				"trusted_issuers[0].algorithms in the generated policy to match the realm's keys.",
			algs, jc.Issuer))
	}

	// The audience may come from the plugin config or from a file the caller
	// named (see [PolicyOptions.AudienceFile]) — either is enough to produce a
	// policy. Conversion fails only when NEITHER yields one.
	//
	// That last case is an error rather than a warning because it is fail-open.
	// Upstream treats an empty `audiences` list as "disable aud validation", and
	// TrustedIssuer's Audiences field is `omitempty` — so an empty slice does
	// not emit an empty list, it omits the key entirely and the engine accepts
	// ANY token from the trusted issuer. That silently defeats the exact check
	// jwt-validation exists to perform, and a warning is the wrong instrument:
	// warnings are advisory, and the policy would still be deployed. Missing
	// issuer and undecidable JWKS are already hard errors above; an
	// unresolvable audience belongs with them.
	auds := jc.audiences(fileAudience)
	if len(auds) == 0 {
		// Name the audience_file the plugin itself points at, since that is
		// most likely what the operator should pass as AudienceFile. Fall back
		// to the plugin's documented default when the field is empty, because
		// applyDefaults fills it in at runtime even when the YAML omits it.
		suggest := jc.AudienceFile
		if suggest == "" {
			suggest = "/shared/client-id.txt"
		}
		switch {
		case jc.AudienceMode == "per-host":
			return PolicyPlugin{}, nil, fmt.Errorf(
				"praxis: jwt-validation uses audience_mode: per-host, which derives the expected " +
					"audience from each request's Host; the policy engine's trusted_issuers take a " +
					"static audience list and cannot express that. Generating a policy anyway would " +
					"omit the audiences key entirely and accept ANY token from the issuer. Set an " +
					"explicit `audience` / `allowed_audiences` on the plugin to convert")
		case jc.AudienceFile != "":
			return PolicyPlugin{}, nil, fmt.Errorf(
				"praxis: jwt-validation reads its expected audience from %q at runtime and the "+
					"plugin config names no literal audience, so none could be resolved; a policy "+
					"without one accepts ANY token from issuer %q. Either point the converter at "+
					"that file (--audience-file %s) so its value is baked into the policy, or set "+
					"an explicit `audience` on the plugin",
				jc.AudienceFile, jc.Issuer, suggest)
		default:
			return PolicyPlugin{}, nil, fmt.Errorf(
				"praxis: jwt-validation declares no audience, so no policy could be generated that "+
					"validates the aud claim; without it any token from issuer %q would be accepted. "+
					"Set `audience` / `allowed_audiences` on the plugin, or point the converter at "+
					"the file the audience is mounted from (--audience-file %s)", jc.Issuer, suggest)
		}
	}

	if len(jc.BypassPaths) > 0 {
		// jwt-validation skips validation on these path globs. The identity
		// plugin has no per-path exemption, so those paths would now require a
		// token. That flips health probes and .well-known endpoints from open
		// to 401 — worth stating explicitly.
		warnings = append(warnings, fmt.Sprintf(
			"jwt-validation exempts the paths %v from validation, which the identity plugin "+
				"has no equivalent for: in the generated policy those paths require a valid "+
				"token too. Health and readiness probes to them will get 401. Praxis serves "+
				"its own liveness/readiness on the admin endpoint, which does not run this "+
				"chain.", jc.BypassPaths))
	}

	return PolicyPlugin{
		Name:        "jwt-validation",
		Kind:        policyPluginKindJWT,
		Description: "Inbound JWT validation, translated from AuthBridge's jwt-validation plugin.",
		Hooks:       []string{policyHookIdentity},
		Mode:        policyModeSequential,
		Priority:    policyJWTPluginPriority,
		OnError:     policyOnErrorFail,
		Config: JWTIdentityConfig{
			Header:      "Authorization",
			ClaimMapper: policyClaimMapperStd,
			TrustedIssuers: []TrustedIssuer{{
				Issuer:     jc.Issuer,
				Audiences:  auds,
				Algorithms: algs,
				DecodingKey: DecodingKey{
					Kind:         "jwks_url",
					URL:          jwks,
					InsecureHTTP: insecureHTTP,
					RefreshSecs:  policyJWKSRefreshSecs,
				},
				LeewaySeconds: policyDefaultLeewaySec,
			}},
		},
	}, warnings, nil
}

// policyHeader is the comment block prepended to a generated policy document.
const policyHeader = `# Praxis POLICY DOCUMENT — GENERATED from an AuthBridge config.
#
# Generated by core/praxis (BuildPolicy). Edits are overwritten
# on the next run; change the AuthBridge config instead.
#
# This is the document the proxy config's ` + "`policy`" + ` filter loads via its
# ` + "`config_path`" + `. It declares the identity plugins the Praxis Policy Engine
# enforces — here, inbound JWT validation translated from AuthBridge's
# jwt-validation plugin.
#
# Requires Praxis built with the policy-engine cargo feature:
#   cargo run --features policy-engine -p praxis-proxy -- -c %s
#
# Enforcement: the engine resolves identity on the request phase and denies a
# missing or invalid token with 401 (WWW-Authenticate: Bearer, plus an
# X-Policy-Violation header naming the failure). ` + "`on_error: fail`" + ` makes the
# plugin fail closed.
#
# Routing is deliberately not enabled. With plugin_settings.routing_enabled
# absent, identity resolution alone gates every request — which is what
# AuthBridge's jwt-validation does. Enabling routes would move enforcement to
# per-entity rules and require protocol-classifier metadata in the chain.
`

// RenderPolicy marshals a policy document to YAML with an explanatory header.
//
// proxyConfigPath is used only to make the header's example command
// copy-pasteable.
func RenderPolicy(doc *PolicyDocument, proxyConfigPath string) ([]byte, error) {
	if doc == nil {
		return nil, fmt.Errorf("praxis: nil policy document")
	}
	return renderYAML(fmt.Sprintf(policyHeader, proxyConfigPath), doc)
}

// RenderPolicyResult marshals a policy document, appending the fidelity
// warnings as a trailing comment block so the generated file carries its own
// caveats — the places where it accepts more than AuthBridge would.
func RenderPolicyResult(res *PolicyResult, proxyConfigPath string) ([]byte, error) {
	if res == nil || res.Document == nil {
		return nil, fmt.Errorf("praxis: no policy document to render")
	}
	out, err := RenderPolicy(res.Document, proxyConfigPath)
	if err != nil {
		return nil, err
	}
	if len(res.Warnings) == 0 {
		return out, nil
	}
	var buf bytes.Buffer
	buf.Write(out)
	buf.WriteString("\n# ── POLICY FIDELITY NOTES ")
	buf.WriteString(strings.Repeat("─", 45))
	buf.WriteString("\n#\n")
	buf.WriteString("# Where this policy differs from the AuthBridge config it came from:\n#\n")
	for _, w := range res.Warnings {
		writeCommentBlock(&buf, w)
	}
	return buf.Bytes(), nil
}
