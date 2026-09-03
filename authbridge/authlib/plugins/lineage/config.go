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
	// dials with TLS and verifies the collector against the system root CAs,
	// or against OTelCAFile when set. An https:// otel_endpoint turns this on
	// automatically; a bare host:port with otel_tls:true is honoured (TLS to
	// that host:port). Two combinations are refused at decode as
	// contradictions rather than silently resolved: an https:// endpoint with
	// an explicit otel_tls:false, and an http:// endpoint with otel_tls:true
	// or OTelCAFile — the scheme states a transport intent, and the knobs
	// must agree with it.
	OTelTLS bool `json:"otel_tls"`

	// OTelCAFile is a PEM bundle of CA certificates to verify the collector's
	// serving certificate against, for a collector whose certificate is not
	// signed by a system root — an in-cluster collector with a cert-manager
	// issued certificate, typically (mount its TLS Secret's ca.crt). Setting it
	// implies OTelTLS=true; an explicit otel_tls:false alongside it, or an
	// http:// endpoint, is a refused contradiction. Read at Init into the cert
	// pool the dial verifies against: an unreadable file, or one with no
	// certificate in it, refuses to start rather than falling back to the
	// system roots. Empty (the default) verifies against the system roots.
	OTelCAFile string `json:"otel_ca_file"`

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
	// Prefixes, not globs — a path is bypassed when it starts with an entry.
	//
	// Setting the key REPLACES this list rather than extending it, the same
	// convention ibac, sparc and cpex use for their bypass keys: an operator
	// who adds one prefix must restate the defaults they want to keep.
	// Entries are trimmed of surrounding whitespace; one that is empty, or
	// "/", is refused at decode because it would match every path and
	// silently turn the plugin off.
	// Default: ["/.well-known/", "/healthz", "/readyz", "/health"]
	BypassPaths []string `json:"bypass_paths"`

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

// validateBypass trims and checks both bypass lists in place. An entry that
// matches everything disables the plugin silently — every exchange takes the
// skip, no span is ever emitted, and Ready() still reports true — so it is a
// boot error rather than a runtime surprise. ibac, sparc and cpex each refuse
// the same shapes with the same reasoning; the wording of the error mirrors
// theirs, including the advice to remove the plugin from the pipeline if
// disabling it is what was meant.
func validateBypass(cfg *Config) error {
	for i, entry := range cfg.BypassPaths {
		entry = strings.TrimSpace(entry)
		if entry == "" || entry == "/" {
			return fmt.Errorf("bypass_paths entry %q matches every path; "+
				"to disable lineage-telemetry, remove it from the pipeline instead", cfg.BypassPaths[i])
		}
		cfg.BypassPaths[i] = entry
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
