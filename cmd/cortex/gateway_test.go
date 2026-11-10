package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/rossoctl/cortex/core/config"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata goldens")

func praxisConfig() *config.Config {
	return &config.Config{
		Listener: config.ListenerConfig{SessionAPIAddr: "127.0.0.1:47601"},
		Praxis:   &config.PraxisConfig{Enabled: true},
	}
}

func TestGatewayYAML_Golden(t *testing.T) {
	got, err := gatewayYAML(praxisConfig(), "/CORTEX_DIR/gateway.token")
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "gateway.yaml.golden")
	if *updateGolden {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("gatewayYAML differs from %s (run with -update):\n%s", golden, got)
	}
}

func TestGatewayYAML_Fields(t *testing.T) {
	out, err := gatewayYAML(praxisConfig(), "/d/gateway.token")
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Insecure   map[string]bool `yaml:"insecure_options"`
		BodyLimits map[string]any  `yaml:"body_limits"`
		Listeners  []struct {
			Name         string          `yaml:"name"`
			Address      string          `yaml:"address"`
			ForwardProxy map[string]bool `yaml:"forward_proxy"`
			FilterChains []string        `yaml:"filter_chains"`
		} `yaml:"listeners"`
		FilterChains []struct {
			Name    string           `yaml:"name"`
			Filters []map[string]any `yaml:"filters"`
		} `yaml:"filter_chains"`
	}
	if err := yaml.Unmarshal(out, &g); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"allow_unbounded_body", "allow_private_upstreams", "allow_private_endpoints"} {
		if !g.Insecure[k] {
			t.Errorf("insecure_options.%s = false", k)
		}
	}
	if v, ok := g.BodyLimits["max_request_bytes"]; !ok || v != 33554432 {
		t.Errorf("max_request_bytes = %v", v)
	}
	if v, ok := g.BodyLimits["max_response_bytes"]; !ok || v != nil {
		t.Errorf("max_response_bytes = %v (present %v), want null", v, ok)
	}
	if len(g.Listeners) != 1 {
		t.Fatalf("listeners = %d", len(g.Listeners))
	}
	l := g.Listeners[0]
	if l.Name != "gateway" || l.Address != config.DefaultPraxisListen || !l.ForwardProxy["enabled"] || len(l.FilterChains) != 1 || l.FilterChains[0] != "main" {
		t.Errorf("listener = %+v", l)
	}
	if len(g.FilterChains) != 1 || g.FilterChains[0].Name != "main" {
		t.Fatalf("filter_chains = %+v", g.FilterChains)
	}
	fs := g.FilterChains[0].Filters
	var order []any
	for _, f := range fs {
		order = append(order, f["filter"])
	}
	wantOrder := []any{"ip_acl", "cortex_attribution", "cortex_observe", "access_log", "router", "load_balancer"}
	if len(order) != len(wantOrder) {
		t.Fatalf("filter order = %v", order)
	}
	for i := range wantOrder {
		if order[i] != wantOrder[i] {
			t.Fatalf("filter order = %v, want %v", order, wantOrder)
		}
	}
	acl := fs[0]
	if allow, _ := acl["allow"].([]any); len(allow) != 2 || allow[0] != "127.0.0.1/32" || allow[1] != "::1/128" {
		t.Errorf("ip_acl.allow = %v", acl["allow"])
	}
	if _, ok := acl["deny"]; ok {
		t.Error("ip_acl has deny")
	}
	if _, ok := acl["conditions"]; ok {
		t.Error("ip_acl has conditions")
	}
	if fs[1]["token_file"] != "/d/gateway.token" || fs[2]["token_file"] != "/d/gateway.token" {
		t.Errorf("token_file = %v, %v", fs[1]["token_file"], fs[2]["token_file"])
	}
	if fs[2]["ingest"] != "127.0.0.1:47601" {
		t.Errorf("ingest = %v", fs[2]["ingest"])
	}
	routes, _ := fs[4]["routes"].([]any)
	if len(routes) != 1 {
		t.Fatalf("routes = %v", fs[4]["routes"])
	}
	if r, _ := routes[0].(map[string]any); r["path_prefix"] != "/" || r["cluster"] != "egress" {
		t.Errorf("route = %v", routes[0])
	}
	clusters, _ := fs[5]["clusters"].([]any)
	if len(clusters) != 1 {
		t.Fatalf("clusters = %v", fs[5]["clusters"])
	}
	c, _ := clusters[0].(map[string]any)
	eps, _ := c["endpoints"].([]any)
	if c["name"] != "egress" || len(eps) != 1 || eps[0] != config.DefaultPraxisEgress {
		t.Errorf("cluster = %v", clusters[0])
	}
}

func TestGatewayYAML_BadSessionAddr(t *testing.T) {
	cfg := praxisConfig()
	cfg.Listener.SessionAPIAddr = "bad"
	if _, err := gatewayYAML(cfg, "/t"); err == nil {
		t.Error("want an error for a session API address that does not split")
	}
}

func TestIngestAddr(t *testing.T) {
	for in, want := range map[string]string{
		":47601":      "127.0.0.1:47601",
		"0.0.0.0:1":   "127.0.0.1:1",
		"[::]:1":      "127.0.0.1:1",
		"127.0.0.1:1": "127.0.0.1:1",
	} {
		got, err := ingestAddr(in)
		if err != nil || got != want {
			t.Errorf("ingestAddr(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ingestAddr("bad"); err == nil {
		t.Error(`ingestAddr("bad") gave no error`)
	}
}

func TestWriteGatewayFiles_TokenIs0600AndNew(t *testing.T) {
	dir := t.TempDir()
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	var tokens []string
	for range 2 {
		tok, yml, err := writeGatewayFiles(dir, praxisConfig())
		if err != nil {
			t.Fatal(err)
		}
		if tok != filepath.Join(dir, gatewayTokenName) || yml != filepath.Join(dir, gatewayYAMLName) {
			t.Fatalf("paths = %s, %s", tok, yml)
		}
		for _, p := range []string{tok, yml} {
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if m := fi.Mode().Perm(); m != 0o600 {
				t.Errorf("%s mode = %o, want 600", p, m)
			}
		}
		b, err := os.ReadFile(tok)
		if err != nil {
			t.Fatal(err)
		}
		if !hex64.Match(b) {
			t.Errorf("token = %q, want 64 hex chars", b)
		}
		tokens = append(tokens, string(b))
		y, err := os.ReadFile(yml)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(y, []byte(tok)) {
			t.Errorf("gateway.yaml does not name %s", tok)
		}
	}
	if tokens[0] == tokens[1] {
		t.Error("two calls gave the same token")
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 2 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("dir holds %v, want only the two files", names)
	}
}
