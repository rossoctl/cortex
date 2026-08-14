package lineage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Config holds the per-plugin configuration decoded from the pipeline YAML.
type Config struct {
	// OTelEndpoint is the OTLP gRPC endpoint (host:port or http://host:port).
	// Default: "localhost:4317"
	OTelEndpoint string `json:"otel_endpoint"`

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
	// of this workload. If empty, SelfIDFile is consulted instead.
	SelfID string `json:"self_id"`

	// SelfIDFile is the path to a file containing the agent's own client ID.
	// Defaults to /shared/client-id.txt (the operator-mounted credential).
	// Ignored when SelfID is set.
	SelfIDFile string `json:"self_id_file"`
}

func defaultConfig() Config {
	return Config{
		OTelEndpoint: "localhost:4317",
		BypassPaths:  []string{"/.well-known/", "/healthz", "/readyz", "/health"},
		BypassHosts:  []string{"otel-collector", "jaeger", "zipkin", "prometheus"},
		SelfIDFile:   "/shared/client-id.txt",
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
		cfg.OTelEndpoint = "localhost:4317"
	}
	// Strip http:// or https:// prefix — gRPC NewClient expects host:port only.
	cfg.OTelEndpoint = strings.TrimPrefix(cfg.OTelEndpoint, "https://")
	cfg.OTelEndpoint = strings.TrimPrefix(cfg.OTelEndpoint, "http://")
	return cfg, nil
}
