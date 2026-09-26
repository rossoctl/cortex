package lineage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strings"
)

// defaultOTelEndpoint is the OTLP gRPC target used when otel_endpoint is unset:
// an in-pod collector reached over plaintext loopback.
const defaultOTelEndpoint = "localhost:4317"

// defaultMaxPayloadBytes bounds a captured input.value / output.value as a
// deliberate producer-side cap. It is NOT a mirror of any SDK limit: the OTel
// SDK's default attribute-value length limit is unlimited (-1) and Init sets no
// SpanLimits, so an oversized value is not dropped or truncated downstream. This
// bound is our own guard against unbounded spans (and against any backend value
// limit); anything longer is cut here with an explicit marker so the loss is
// visible in the span. 4096 is a conservative default, not a hard requirement.
const defaultMaxPayloadBytes = 4096

// unboundedPayload is the sole opt-out from the cap. Any other negative is
// refused at decode, so a typo cannot quietly attach whole payloads.
const unboundedPayload = -1

// defaultMaxAttrBytes bounds every variable-content string attribute and the
// span name. Several of those values are caller-controlled (url.path, Host,
// mcp.tool from the request body, a2a.session_id) and the SDK never truncates
// on its own — without this cap one request can put a 100 KB span name into
// the backend. 256 comfortably fits real paths, hosts and tool names.
const defaultMaxAttrBytes = 256

// Config holds the per-plugin configuration decoded from the pipeline YAML.
type Config struct {
	// OTelEndpoint is the OTLP gRPC endpoint (host:port, http://host:port, or
	// https://host:port). An https:// scheme implies OTelTLS=true. Any other
	// URL scheme is rejected at decode (see decodeConfig).
	// Default: "localhost:4317"
	OTelEndpoint string `json:"otel_endpoint" description:"OTLP gRPC target: host:port, http://host:port or https://host:port; any other scheme is refused." default:"localhost:4317"`

	// OTelTLS selects the OTLP transport. False (the default) dials plaintext,
	// which is correct for the in-pod loopback collector but sends spans —
	// including lineage.principal.* on every inbound request, and full payloads
	// under CaptureIO — in cleartext. Set true for any collector off-pod: it
	// dials with TLS and verifies the collector against the system root CAs,
	// or against OTelCAFile when set. An https:// otel_endpoint turns this on
	// automatically; a bare host:port with otel_tls:true is honoured (TLS to
	// that host:port). Two combinations are refused at decode as
	// contradictions rather than silently resolved: an https:// endpoint with
	// an explicit otel_tls:false, and an http:// endpoint with otel_tls:true
	// or OTelCAFile — the scheme states a transport intent, and the knobs
	// must agree with it.
	OTelTLS bool `json:"otel_tls" description:"Dial the collector with TLS, verified against the system roots or otel_ca_file; an https:// endpoint implies it." default:"false"`

	// OTelCAFile is a PEM bundle of CA certificates to verify the collector's
	// serving certificate against, for a collector whose certificate is not
	// signed by a system root — an in-cluster collector with a cert-manager
	// issued certificate, typically (mount its TLS Secret's ca.crt). Setting it
	// implies OTelTLS=true; an explicit otel_tls:false alongside it, or an
	// http:// endpoint, is a refused contradiction. Read at Init into the cert
	// pool the dial verifies against: an unreadable file, or one with no
	// certificate in it, refuses to start rather than falling back to the
	// system roots. Empty (the default) verifies against the system roots.
	OTelCAFile string `json:"otel_ca_file" description:"PEM bundle to verify the collector certificate against (a private CA); implies otel_tls."`

	// CaptureIO when true attaches parsed request/response content as
	// input.value (request span) and output.value (response span)
	// attributes, enabling Phoenix to display message content inline.
	//
	// For A2A (inbound agent calls): input = user message parts, output = artifact.
	// For MCP tools/call: input = tool params JSON, output = tool result JSON.
	// For Inference (LLM): input = messages array JSON, output = completion text.
	//
	// Off by default — enable only if traces do not contain PII or the
	// OTel backend enforces appropriate access controls.
	CaptureIO bool `json:"capture_io" description:"Attach parsed request/response content as input.value / output.value." default:"false"`

	// MaxPayloadBytes caps the size of the input.value / output.value
	// attributes attached under CaptureIO. A payload longer than this is cut on
	// a UTF-8 boundary and suffixed with a truncation marker, so the loss is
	// explicit in the span. This is a deliberate producer-side bound; the OTel
	// SDK does not itself drop or truncate an oversized value (its default
	// attribute-value limit is unlimited and Init sets no SpanLimits), so
	// without this cap the whole payload would be emitted. Zero (or unset) uses
	// defaultMaxPayloadBytes; unboundedPayload (-1) attaches the whole value,
	// and any other negative is refused at decode.
	// Ignored when CaptureIO is false.
	// Default: 4096
	MaxPayloadBytes int `json:"max_payload_bytes" description:"Byte cap on input.value / output.value; 0 or unset takes the default, -1 attaches whole values." default:"4096"`

	// MaxAttrBytes caps every variable-content string attribute (url.path,
	// lineage.peer.host, mcp.tool, a2a.session_id, …) and the span name, cut
	// on a UTF-8 boundary with the same truncation marker as payloads.
	// input.value / output.value keep their own MaxPayloadBytes cap, and
	// fixed-vocabulary facts (lineage.role, lineage.outcome, …) are bounded by
	// construction. Zero (or unset) uses defaultMaxAttrBytes; -1 removes the
	// cap, and any other negative is refused at decode.
	// Default: 256
	MaxAttrBytes int `json:"max_attr_bytes" description:"Byte cap on every variable-content string attribute and the span name; 0 or unset takes the default, -1 removes the cap." default:"256"`

	// MintTraceparent — both directions — forwards a W3C traceparent naming
	// this exchange's request span when the request arrived with no
	// valid traceparent. Without one the next element has nothing to
	// extract: an app's propagate-only shim roots a fresh trace of its own,
	// and the tracestate stamp (which W3C reads only alongside a valid
	// traceparent) never leaves this pod — so the entry exchange lands alone
	// in its own trace and every call it caused derives as a parentless root.
	// Absent, empty and malformed traceparents are all restarted, which is
	// W3C's processing model for an unparseable one; a valid traceparent is
	// never modified. This is the one place the plugin writes a traceparent.
	// Set false for a pure observer that must not add a header the
	// application would see (the exchange then fragments, visibly).
	// Default: true
	MintTraceparent bool `json:"mint_traceparent" description:"Forward a traceparent naming this request span when no valid one arrived; false = a pure observer that writes no traceparent." default:"true"`

	// BypassPaths lists URL path globs that should not generate lineage
	// hops. Useful for suppressing infrastructure polling (agent-card
	// discovery, health checks) that would otherwise flood the lineage graph.
	// Matched by the shared bypass package (path.Match, query stripped, path
	// normalized) — the same package and semantics jwt-validation and sparc
	// use for this key, so a pattern copied between plugins means the same
	// thing. Note path.Match's "*" does not cross "/": "/.well-known/*"
	// matches "/.well-known/agent.json" but not "/.well-known/a/b". An
	// earlier prefix match here silently bypassed real traffic under the
	// "/health" default ("/health-records/...").
	//
	// Setting the key REPLACES this list rather than extending it, the same
	// convention ibac, sparc and cpex use for their bypass keys: an operator
	// who adds one glob must restate the defaults they want to keep.
	// Entries are trimmed of surrounding whitespace; bypass.NewMatcher
	// refuses invalid path.Match syntax and match-everything patterns
	// (empty, "*", "/*") at boot.
	// Default: ["/.well-known/*", "/healthz", "/readyz", "/health"]
	BypassPaths []string `json:"bypass_paths" description:"URL path globs (path.Match) that produce no spans; setting the key replaces the default list." default:"/.well-known/*, /healthz, /readyz, /health"`

	// BypassHosts lists host globs whose exchanges should not generate lineage
	// hops. Useful for suppressing infrastructure outbound calls such as OTel
	// trace exports. Matched with path.Match against the request Host with the
	// port stripped and case folded — see matchesAnyHost — so "otel-collector"
	// matches only that exact name and "otel-collector.*" matches
	// otel-collector.rossoctl-system.svc. This is the glob convention ibac,
	// sparc and cpex already use for the key of the same name; the defaults
	// carry both forms because in-cluster short-name calls are ordinary.
	//
	// Honoured on the outbound phase only: an inbound Host is the caller's own
	// header, and a bypass driven by it would be an opt-out from being graphed.
	//
	// Setting the key REPLACES this list rather than extending it, as with
	// BypassPaths. Entries are trimmed of surrounding whitespace; one that is
	// empty, "*", or not valid path.Match syntax is refused at decode.
	// Default: ["otel-collector", "otel-collector.*", "jaeger", "jaeger.*",
	// "zipkin", "zipkin.*", "prometheus", "prometheus.*"]
	BypassHosts []string `json:"bypass_hosts" description:"Outbound host globs (path.Match, port stripped, case folded) that produce no spans; ignored inbound; replaces the default list." default:"otel-collector, otel-collector.*, jaeger, jaeger.*, zipkin, zipkin.*, prometheus, prometheus.*"`

	// SelfID is the agent's own stable identifier, emitted as the
	// lineage.self.id fact on every span. Typically the Keycloak client ID
	// of this workload. If empty, SelfIDFile is consulted instead. A value
	// containing "/" (a SPIFFE ID) is reduced to its last non-empty path
	// segment before emission — see serviceLabel — so two identities that
	// differ only above that segment emit the same lineage.self.id.
	SelfID string `json:"self_id" description:"This workload identity, emitted as lineage.self.id; a SPIFFE ID is reduced to its last path segment."`

	// SelfIDFile is the path to a file containing the agent's own client ID.
	// Defaults to /shared/client-id.txt (the operator-mounted credential),
	// which can land after the pod starts: until it is readable the plugin is
	// not ready and skips every exchange, polling the file in background (see
	// Init), and /readyz names it meanwhile. Ignored when SelfID is set.
	SelfIDFile string `json:"self_id_file" description:"Read when self_id is empty; until it is readable the plugin is not ready and emits nothing. Refused at start only when self_id is also empty." default:"/shared/client-id.txt"`

	// Namespace is the Kubernetes namespace this workload runs in, emitted as
	// the lineage.self.namespace fact on every span. Required: lineage.self.id
	// alone is not an identity — the same workload name in two namespaces is
	// two workloads, and the consumer keys entity identity on the
	// (namespace, self.id) pair (wire contract §7). The attach kit writes its
	// NAMESPACE here. It is never derived from the SPIFFE ID's path — that
	// layout is a registrar convention, and the kit path has no SPIFFE ID at
	// all. Empty, blank, or not an RFC 1123 DNS label (the only shape a
	// namespace can have) refuses at start (see resolveNamespace); when empty,
	// NamespaceFile is consulted instead.
	Namespace string `json:"namespace" required:"true" description:"This workload's Kubernetes namespace (an RFC 1123 DNS label), emitted as lineage.self.namespace on every span; refused at start when empty or not a label. Alternatively namespace_file."`

	// NamespaceFile is a file carrying the namespace, read once at Init when
	// Namespace is empty — meant for the path the kubelet projects from the
	// pod's own metadata into every container that mounts the service-account
	// volume, /var/run/secrets/kubernetes.io/serviceaccount/namespace. That
	// is the one source that is correct in every copy of a ConfigMap shared
	// across namespaces (the platform's per-namespace authbridge-runtime-config
	// is rendered from one template and copied), where an inline literal would
	// be confidently wrong in every namespace but one. No default and no
	// poller: an absent file is a wrong path or a missing mount, and refuses
	// at start like an empty value. Ignored when Namespace is set.
	NamespaceFile string `json:"namespace_file" description:"Read when namespace is empty, once at start (e.g. /var/run/secrets/kubernetes.io/serviceaccount/namespace); absent, blank or not a DNS label refuses at start."`
}

func defaultConfig() Config {
	return Config{
		OTelEndpoint:    defaultOTelEndpoint,
		MaxPayloadBytes: defaultMaxPayloadBytes,
		MaxAttrBytes:    defaultMaxAttrBytes,
		MintTraceparent: true,
		BypassPaths:     []string{"/.well-known/*", "/healthz", "/readyz", "/health"},
		BypassHosts: []string{
			"otel-collector", "otel-collector.*",
			"jaeger", "jaeger.*",
			"zipkin", "zipkin.*",
			"prometheus", "prometheus.*",
		},
		SelfIDFile: "/shared/client-id.txt",
	}
}

func decodeConfig(raw json.RawMessage) (Config, error) {
	cfg := defaultConfig()
	if len(raw) == 0 {
		return cfg, nil
	}
	// Unknown keys are a boot error: a typo'd knob (capture-io, selfid_file)
	// must not silently run with defaults.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("lineage-telemetry config: %w", err)
	}
	if cfg.OTelEndpoint == "" {
		cfg.OTelEndpoint = defaultOTelEndpoint
	}
	// Zero means "unset" → the safe default; a negative value is the explicit
	// opt-out (no cap). This keeps an omitted key and an explicit 0 identical.
	if cfg.MaxPayloadBytes == 0 {
		cfg.MaxPayloadBytes = defaultMaxPayloadBytes
	}
	if cfg.MaxPayloadBytes < unboundedPayload {
		return Config{}, fmt.Errorf("lineage-telemetry config: max_payload_bytes %d is invalid; use -1 to attach whole values or a positive byte cap", cfg.MaxPayloadBytes)
	}
	// Same convention as max_payload_bytes: 0 = the default, -1 = no cap.
	if cfg.MaxAttrBytes == 0 {
		cfg.MaxAttrBytes = defaultMaxAttrBytes
	}
	if cfg.MaxAttrBytes < unboundedPayload {
		return Config{}, fmt.Errorf("lineage-telemetry config: max_attr_bytes %d is invalid; use -1 to remove the cap or a positive byte cap", cfg.MaxAttrBytes)
	}
	// gRPC NewClient expects host:port only, so reduce a URL form (e.g.
	// http://collector:4317/v1/traces) to its host — TrimPrefix left any path
	// behind and produced an invalid dial target. A URL scheme also carries an
	// intent about transport: https:// asks for TLS. Honour it (or fail on a
	// contradiction) rather than silently dropping to cleartext.
	if strings.Contains(cfg.OTelEndpoint, "://") {
		u, err := url.Parse(cfg.OTelEndpoint)
		if err != nil || u.Host == "" {
			return Config{}, fmt.Errorf("lineage-telemetry config: invalid otel_endpoint %q", cfg.OTelEndpoint)
		}
		// Only http/https carry a meaningful OTLP transport intent. Reject any
		// other scheme (ftp://, ftps://, …) rather than strip it and dial the
		// bare host:port insecurely — that would silently send principal facts
		// and payloads in cleartext. Fail closed, matching this package's
		// DisallowUnknownFields / https+otel_tls:false posture.
		if u.Scheme != "http" && u.Scheme != "https" {
			return Config{}, fmt.Errorf("lineage-telemetry config: unsupported otel_endpoint scheme %q (want http or https)", u.Scheme)
		}
		if u.Scheme == "https" {
			// An explicit otel_tls:false alongside an https:// endpoint is a
			// contradiction: one asks for encryption, the other for cleartext.
			// Fail closed rather than pick one, consistent with the
			// DisallowUnknownFields fail-on-ambiguity choice this package makes.
			if tlsExplicitlyFalse(raw) {
				return Config{}, fmt.Errorf("lineage-telemetry config: otel_endpoint %q is https but otel_tls is false", cfg.OTelEndpoint)
			}
			cfg.OTelTLS = true
		}
		// The mirror image: an http:// endpoint asks for cleartext, so a TLS
		// knob beside it is the same contradiction the other way round.
		if u.Scheme == "http" && (cfg.OTelTLS || cfg.OTelCAFile != "") {
			return Config{}, fmt.Errorf("lineage-telemetry config: otel_endpoint %q is http but otel_tls or otel_ca_file asks for TLS", cfg.OTelEndpoint)
		}
		cfg.OTelEndpoint = u.Host
	}
	// A CA file is only meaningful for a TLS dial: it implies otel_tls, and
	// pairing it with an explicit otel_tls:false is the same contradiction as
	// https:// + otel_tls:false above.
	if cfg.OTelCAFile != "" {
		if tlsExplicitlyFalse(raw) {
			return Config{}, fmt.Errorf("lineage-telemetry config: otel_ca_file is set but otel_tls is false")
		}
		cfg.OTelTLS = true
	}
	if err := validateBypass(&cfg); err != nil {
		return Config{}, fmt.Errorf("lineage-telemetry config: %w", err)
	}
	return cfg, nil
}

// validateBypass trims and checks the bypass lists in place. An entry that
// matches everything disables the plugin silently — every exchange takes the
// skip, no span is ever emitted, and Ready() still reports true — so it is a
// boot error rather than a runtime surprise. Paths are only trimmed here:
// their validation (invalid path.Match syntax, match-everything patterns)
// lives in bypass.NewMatcher, called at Configure. Hosts are checked here,
// mirroring ibac / sparc / cpex, including the advice to remove the plugin
// from the pipeline if disabling it is what was meant.
func validateBypass(cfg *Config) error {
	for i, entry := range cfg.BypassPaths {
		cfg.BypassPaths[i] = strings.TrimSpace(entry)
	}
	for i, entry := range cfg.BypassHosts {
		entry = strings.TrimSpace(entry)
		if _, err := path.Match(entry, ""); err != nil {
			return fmt.Errorf("invalid bypass_hosts glob %q: %w", cfg.BypassHosts[i], err)
		}
		if entry == "" || entry == "*" {
			return fmt.Errorf("bypass_hosts entry %q matches every host; "+
				"to disable lineage-telemetry, remove it from the pipeline instead", cfg.BypassHosts[i])
		}
		cfg.BypassHosts[i] = entry
	}
	return nil
}

// tlsExplicitlyFalse reports whether the raw config carries otel_tls set to a
// literal false, as opposed to being absent (whose decoded value is also false
// but carries no intent). Used only to reject the https:// + otel_tls:false
// contradiction; a decode failure here is treated as "not explicitly false"
// since the DisallowUnknownFields pass above already validated the shape.
func tlsExplicitlyFalse(raw json.RawMessage) bool {
	var probe struct {
		OTelTLS *bool `json:"otel_tls"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	return probe.OTelTLS != nil && !*probe.OTelTLS
}
