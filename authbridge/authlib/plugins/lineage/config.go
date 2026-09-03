package lineage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
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

// Config holds the per-plugin configuration decoded from the pipeline YAML.
type Config struct {
	// OTelEndpoint is the OTLP gRPC endpoint (host:port, http://host:port, or
	// https://host:port). An https:// scheme implies OTelTLS=true. Any other
	// URL scheme is rejected at decode (see decodeConfig).
	// Default: "localhost:4317"
	OTelEndpoint string `json:"otel_endpoint"`

	// OTelTLS selects the OTLP transport. False (the default) dials plaintext,
	// which is correct for the in-pod loopback collector but sends spans —
	// including lineage.principal.* on every inbound request, and full payloads
	// under CaptureIO — in cleartext. Set true for any collector off-pod: it
	// dials with TLS against the system root CAs. An https:// otel_endpoint
	// turns this on automatically; a plaintext otel_endpoint with otel_tls:true
	// is honoured (TLS to a host:port). The one rejected combination is an
	// https:// endpoint with an explicit otel_tls:false (see decodeConfig): a
	// contradiction that would otherwise silently downgrade to cleartext.
	OTelTLS bool `json:"otel_tls"`

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
	CaptureIO bool `json:"capture_io"`

	// MaxPayloadBytes caps the size of the input.value / output.value
	// attributes attached under CaptureIO. A payload longer than this is cut on
	// a UTF-8 boundary and suffixed with a truncation marker, so the loss is
	// explicit in the span. This is a deliberate producer-side bound; the OTel
	// SDK does not itself drop or truncate an oversized value (its default
	// attribute-value limit is unlimited and Init sets no SpanLimits), so
	// without this cap the whole payload would be emitted. Zero (or unset) uses
	// defaultMaxPayloadBytes; a negative value disables the cap (attach whole).
	// Ignored when CaptureIO is false.
	// Default: 4096
	MaxPayloadBytes int `json:"max_payload_bytes"`

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
	MintTraceparent bool `json:"mint_traceparent"`

	// BypassPaths lists URL path prefixes that should not generate lineage
	// hops. Useful for suppressing infrastructure polling (agent-card
	// discovery, health checks) that would otherwise flood the lineage graph.
	// Default: ["/.well-known/", "/healthz", "/readyz", "/health"]
	BypassPaths []string `json:"bypass_paths"`

	// BypassHosts lists target host substrings (matched against pctx.Host)
	// that should not generate lineage hops. Useful for suppressing
	// infrastructure outbound calls such as OTel trace exports.
	// Default: ["otel-collector", "jaeger", "zipkin", "prometheus"]
	BypassHosts []string `json:"bypass_hosts"`

	// SelfID is the agent's own stable identifier, emitted as the
	// lineage.self.id fact on every span. Typically the Keycloak client ID
	// of this workload. If empty, SelfIDFile is consulted instead. A value
	// containing "/" (a SPIFFE ID) is reduced to its last non-empty path
	// segment before emission — see serviceLabel — so two identities that
	// differ only above that segment emit the same lineage.self.id.
	SelfID string `json:"self_id"`

	// SelfIDFile is the path to a file containing the agent's own client ID.
	// Defaults to /shared/client-id.txt (the operator-mounted credential).
	// Ignored when SelfID is set.
	SelfIDFile string `json:"self_id_file"`
}

func defaultConfig() Config {
	return Config{
		OTelEndpoint:    defaultOTelEndpoint,
		MaxPayloadBytes: defaultMaxPayloadBytes,
		MintTraceparent: true,
		BypassPaths:     []string{"/.well-known/", "/healthz", "/readyz", "/health"},
		BypassHosts:     []string{"otel-collector", "jaeger", "zipkin", "prometheus"},
		SelfIDFile:      "/shared/client-id.txt",
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
		cfg.OTelEndpoint = u.Host
	}
	return cfg, nil
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
