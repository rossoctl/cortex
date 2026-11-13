package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/rossoctl/cortex/core/config"
)

// The gateway binary, and the two files the supervisor writes for it in the
// directory of the --config file.
const (
	gatewayBinary    = "cortex-gateway"
	gatewayTokenName = "gateway.token"
	gatewayYAMLName  = "gateway.yaml"
)

// gatewayMaxRequestBytes is the forward proxy's maxRequestBodySize (32 MiB).
const gatewayMaxRequestBytes = 32 << 20

type gatewayFile struct {
	Insecure     gatewayInsecure `yaml:"insecure_options"`
	BodyLimits   gatewayLimits   `yaml:"body_limits"`
	Listeners    []gatewayListen `yaml:"listeners"`
	FilterChains []gatewayChain  `yaml:"filter_chains"`
}

type gatewayInsecure struct {
	AllowUnboundedBody    bool `yaml:"allow_unbounded_body"`
	AllowPrivateUpstreams bool `yaml:"allow_private_upstreams"`
	AllowPrivateEndpoints bool `yaml:"allow_private_endpoints"`
}

type gatewayLimits struct {
	MaxRequestBytes  int64  `yaml:"max_request_bytes"`
	MaxResponseBytes *int64 `yaml:"max_response_bytes"` // nil writes null: no cap
}

type gatewayListen struct {
	Name         string          `yaml:"name"`
	Address      string          `yaml:"address"`
	ForwardProxy map[string]bool `yaml:"forward_proxy"`
	FilterChains []string        `yaml:"filter_chains"`
}

type gatewayChain struct {
	Name    string          `yaml:"name"`
	Filters []gatewayFilter `yaml:"filters"`
}

// gatewayFilter puts the filter name first; its options follow in key order.
type gatewayFilter struct {
	Filter string         `yaml:"filter"`
	Opts   map[string]any `yaml:",inline"`
}

// gatewayYAML returns the cortex-gateway config for cfg (praxis mode spec,
// Section 4). One catch-all route sends every request to the egress cluster:
// Cortex's egress listener, which dials the origin that X-Cortex-Dest names.
func gatewayYAML(cfg *config.Config, tokenPath string) ([]byte, error) {
	ingest, err := ingestAddr(cfg.Listener.SessionAPIAddr)
	if err != nil {
		return nil, err
	}
	filters := []gatewayFilter{
		{Filter: "ip_acl", Opts: map[string]any{"allow": []string{"127.0.0.1/32", "::1/128"}}},
		{Filter: "cortex_attribution", Opts: map[string]any{"token_file": tokenPath}},
		{Filter: "cortex_observe", Opts: map[string]any{"token_file": tokenPath, "ingest": ingest}},
	}
	// policy comes after cortex_observe, so a denied request still gets a full row.
	if p := cfg.Praxis.Policy; p != nil && p.ConfigPath != "" {
		// The hop sends all traffic here. require_model defaults to true, which refuses each body without a model.
		// max_request_bytes defaults to 10 MiB and applies to every body, so it gets the gateway's limit.
		// An llm: allow-list is not complete: a gzip or zstd body, or a body with no string model, matches no llm: route.
		filters = append(filters, gatewayFilter{Filter: "policy", Opts: map[string]any{
			"config_path": p.ConfigPath,
			"llm":         map[string]any{"require_model": false, "max_request_bytes": gatewayMaxRequestBytes},
		}})
	}
	filters = append(filters,
		gatewayFilter{Filter: "access_log"},
		gatewayFilter{Filter: "router", Opts: map[string]any{"routes": []map[string]string{{"path_prefix": "/", "cluster": "egress"}}}},
		gatewayFilter{Filter: "load_balancer", Opts: map[string]any{"clusters": []map[string]any{{"name": "egress", "endpoints": []string{cfg.Praxis.EgressAddr()}}}}},
	)
	f := gatewayFile{
		Insecure:   gatewayInsecure{true, true, true},
		BodyLimits: gatewayLimits{MaxRequestBytes: gatewayMaxRequestBytes},
		Listeners: []gatewayListen{{
			Name:         "gateway",
			Address:      cfg.Praxis.ListenAddr(),
			ForwardProxy: map[string]bool{"enabled": true},
			FilterChains: []string{"main"},
		}},
		FilterChains: []gatewayChain{{Name: "main", Filters: filters}},
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(f); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// ingestAddr returns the address the gateway posts rows to: the session API
// address, with an empty or wildcard host made 127.0.0.1.
func ingestAddr(sessionAPIAddr string) (string, error) {
	host, port, err := net.SplitHostPort(sessionAPIAddr)
	if err != nil {
		return "", fmt.Errorf("listener.session_api_addr %q: %w", sessionAPIAddr, err)
	}
	if a, err := netip.ParseAddr(host); host == "" || (err == nil && a.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port), nil
}

// writeGatewayFiles writes gateway.yaml into dir, and a random token when dir has
// none. Each file has mode 0600 and is written by rename, so a reader never sees a
// partial file. An existing token is kept: each child reads it once, at its start,
// so a second supervisor on the same dir that wrote a new token would break the
// pair of the first one at its next child restart.
func writeGatewayFiles(dir string, cfg *config.Config) (tokenPath, yamlPath string, err error) {
	tokenPath = filepath.Join(dir, gatewayTokenName)
	yamlPath = filepath.Join(dir, gatewayYAMLName)
	y, err := gatewayYAML(cfg, tokenPath)
	if err != nil {
		return "", "", err
	}
	if b, err := os.ReadFile(tokenPath); err != nil || len(bytes.TrimSpace(b)) == 0 {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return "", "", err
		}
		if err := writeFileAtomic(tokenPath, []byte(hex.EncodeToString(raw))); err != nil {
			return "", "", err
		}
	}
	if err := writeFileAtomic(yamlPath, y); err != nil {
		return "", "", err
	}
	return tokenPath, yamlPath, nil
}

// writeFileAtomic writes b to a 0600 temp file next to path, then renames it.
func writeFileAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
