package main

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/cost/pricing"
)

// pinnedConfig is shaped like a config the built-in preset wrote before it priced
// Bob: every listener pin already present, so the pin migration has nothing to do,
// and no pricing: section. This is the upgrade the Bob migration exists for.
const pinnedConfig = `# Built-in config for: cortex --local
mode: proxy-sidecar
listener:
  roles: [forward]
  bind_loopback_only: true
  forward_proxy_addr: 127.0.0.1:47600
  session_api_addr: 127.0.0.1:47601
  health_addr: 127.0.0.1:47604
  transparent_proxy_addr: 127.0.0.1:47603
stats:
  address: 127.0.0.1:47602
tls_bridge:
  mode: enabled
  ca_dir: "CADIR"
  generate_ca: true
pipeline:
  outbound:
    plugins:
      - name: inference-parser
      - name: tool-prune
        config:
          remove: [CronCreate, WebSearch]
`

// withPricing puts a pricing section into pinnedConfig between tls_bridge and
// pipeline, so the insertion has a following key to stay clear of.
func withPricing(section string) string {
	return strings.Replace(pinnedConfig, "pipeline:\n", section+"pipeline:\n", 1)
}

// assertPricesBob resolves through pricing.Build, the path the proxy uses, rather
// than reading the struct: what matters is what a Bob request costs.
func assertPricesBob(t *testing.T, p string) *config.Config {
	t.Helper()
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("migrated config does not load: %v\n%s", err, readFile(t, p))
	}
	tab, err := pricing.Build(cfg.Pricing)
	if err != nil {
		t.Fatalf("pricing.Build: %v", err)
	}
	for _, model := range []string{"premium-ide", "router", "openai/gpt-oss-20b"} {
		rates, prov := tab.Resolve(bobHost, model, 0)
		if prov != pricing.ProvConfigured {
			t.Errorf("%s on %s: provenance %v, want configured\n%s", model, bobHost, prov, readFile(t, p))
			continue
		}
		for tier := pricing.TierInput; tier <= pricing.TierOutput; tier++ {
			if perMillion := rates.Base[tier] * 1e6; !rates.Set[tier] || math.Abs(perMillion-2) > 1e-9 {
				t.Errorf("%s on %s: tier %d = %v per Mtok (set %v), want 2", model, bobHost, tier, perMillion, rates.Set[tier])
			}
		}
		if got := tab.CurrencyFor(bobHost, model); got != "Bobcoins" {
			t.Errorf("%s on %s: unit %q, want Bobcoins", model, bobHost, got)
		}
	}
	return cfg
}

// TestMigrateBobPricing_AddsTheEntry is the point: an install from before the preset
// priced Bob keeps its config forever, and every Bob request on it is unpriced.
func TestMigrateBobPricing_AddsTheEntry(t *testing.T) {
	p := writeCfg(t, pinnedConfig)
	original := readFile(t, p)

	var out bytes.Buffer
	changed, err := migrateBobPricing(p, &out)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !changed {
		t.Fatal("reported no change")
	}
	cfg := assertPricesBob(t, p)

	// Scoped to Bob's host: its "*" model must not reprice anyone else in Bobcoins.
	tab, err := pricing.Build(cfg.Pricing)
	if err != nil {
		t.Fatal(err)
	}
	if _, prov := tab.Resolve("api.anthropic.com", "claude-opus-5-5", 0); prov != pricing.ProvBundled {
		t.Errorf("claude-opus-5-5 on api.anthropic.com: provenance %v, want bundled", prov)
	}
	if !strings.Contains(readFile(t, p), "remove: [CronCreate, WebSearch]") {
		t.Error("the prune list was lost")
	}
	if got, err := os.ReadFile(p + ".before-agentop-pricing"); err != nil || string(got) != original {
		t.Errorf("backup missing or not the original (err %v)", err)
	}
	if !strings.Contains(out.String(), bobHost) {
		t.Errorf("output does not say what was added:\n%s", out.String())
	}
}

// TestMigrateBobPricing_Shapes covers the places a pricing section can already be,
// each needing the entry inserted at a different depth.
func TestMigrateBobPricing_Shapes(t *testing.T) {
	gateway := func(t *testing.T, cfg *config.Config) {
		t.Helper()
		for _, ep := range cfg.Pricing.Endpoints {
			if len(ep.Hosts) == 1 && ep.Hosts[0] == "gw.example.com" {
				if ep.Multiplier == nil || *ep.Multiplier != 0.8 {
					t.Errorf("the existing gateway lost its multiplier: %+v", ep)
				}
				return
			}
		}
		t.Error("the existing gateway entry is gone")
	}
	for _, tc := range []struct {
		name  string
		body  string
		check func(*testing.T, *config.Config)
	}{
		{
			name: "whole document indented",
			body: "  mode: proxy-sidecar\n  listener:\n    roles: [forward]\n    bind_loopback_only: true\n" +
				"    forward_proxy_addr: 127.0.0.1:47600\n  stats:\n    address: 127.0.0.1:47602\n",
		},
		{
			name:  "endpoints with indented items",
			body:  withPricing("pricing:\n  endpoints:\n    - hosts: [\"gw.example.com\"]\n      multiplier: 0.8\n"),
			check: gateway,
		},
		{
			name:  "endpoints with compact items",
			body:  withPricing("pricing:\n  endpoints:\n  - hosts: [\"gw.example.com\"]\n    multiplier: 0.8\n"),
			check: gateway,
		},
		{
			name: "pricing without endpoints",
			body: withPricing("pricing:\n  bundled: true\n"),
			check: func(t *testing.T, cfg *config.Config) {
				if cfg.Pricing.Bundled == nil || !*cfg.Pricing.Bundled {
					t.Error("bundled: true was lost")
				}
			},
		},
		{
			name: "empty pricing",
			body: withPricing("pricing:   # filled in later\n"),
		},
		{
			name: "empty endpoints",
			body: withPricing("pricing:\n  endpoints:\n"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := writeCfg(t, tc.body)
			var out bytes.Buffer
			changed, err := migrateBobPricing(p, &out)
			if err != nil {
				t.Fatalf("migrate: %v\n%s", err, readFile(t, p))
			}
			if !changed {
				t.Fatal("reported no change")
			}
			cfg := assertPricesBob(t, p)
			if tc.check != nil {
				tc.check(t, cfg)
			}
		})
	}
}

// TestMigrateBobPricing_LeavesPricedBobAlone: whatever already prices Bob is the
// operator's choice, however it was written. Only ever ADDS.
func TestMigrateBobPricing_LeavesPricedBobAlone(t *testing.T) {
	for _, tc := range []struct{ name, section string }{
		{"own rate for the host", "pricing:\n  endpoints:\n    - hosts: [\"api.us-east.bob.ibm.com\"]\n" +
			"      models:\n        \"*\":\n          input_cost_per_million: 3.00\n          output_cost_per_million: 3.00\n"},
		{"host glob", "pricing:\n  endpoints:\n    - hosts: [\"*.bob.ibm.com\"]\n" +
			"      models:\n        premium-ide:\n          input_cost_per_million: 1.00\n"},
		{"catch-all rate", "pricing:\n  endpoints:\n    - hosts: [\"*\"]\n" +
			"      models:\n        \"*\":\n          input_cost_per_million: 5.00\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := writeCfg(t, withPricing(tc.section))
			before := readFile(t, p)
			var out bytes.Buffer
			changed, err := migrateBobPricing(p, &out)
			if err != nil {
				t.Fatal(err)
			}
			if changed {
				t.Errorf("changed a config that already prices Bob:\n%s", readFile(t, p))
			}
			if readFile(t, p) != before {
				t.Error("the file was modified")
			}
			if _, err := os.Stat(p + ".before-agentop-pricing"); !os.IsNotExist(err) {
				t.Errorf("a backup was written for a no-op (stat err %v)", err)
			}
		})
	}
}

// TestMigrateBobPricing_ACatchAllMultiplierIsNotARate: a discount on everything prices
// nothing Bob serves, so it must not count as the operator having priced Bob.
// Checked against the resolver, which is why a text search could not do this.
func TestMigrateBobPricing_ACatchAllMultiplierIsNotARate(t *testing.T) {
	p := writeCfg(t, withPricing("pricing:\n  endpoints:\n    - hosts: [\"*\"]\n      multiplier: 0.8\n"))
	var out bytes.Buffer
	changed, err := migrateBobPricing(p, &out)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("left Bob unpriced behind a catch-all multiplier")
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	tab, err := pricing.Build(cfg.Pricing)
	if err != nil {
		t.Fatal(err)
	}
	if _, prov := tab.Resolve(bobHost, "premium-ide", 0); prov != pricing.ProvConfigured {
		t.Errorf("premium-ide: provenance %v, want configured", prov)
	}
}

// TestMigrateBobPricing_Idempotent: this runs on every service install.
func TestMigrateBobPricing_Idempotent(t *testing.T) {
	p := writeCfg(t, pinnedConfig)
	var out bytes.Buffer
	if changed, err := migrateBobPricing(p, &out); err != nil || !changed {
		t.Fatalf("first: changed=%v err=%v", changed, err)
	}
	first := readFile(t, p)
	changed, err := migrateBobPricing(p, &out)
	if err != nil {
		t.Fatal(err)
	}
	if changed || readFile(t, p) != first {
		t.Error("second run changed the file again")
	}
}

// TestMigrateBobPricing_RefusesFlowStyle: block-style lines cannot go inside a flow
// collection. Say so rather than leave a bare parse error, and touch nothing.
func TestMigrateBobPricing_RefusesFlowStyle(t *testing.T) {
	for _, tc := range []struct{ name, section string }{
		{"flow pricing", "pricing: {bundled: true}\n"},
		{"flow endpoints", "pricing:\n  endpoints: []\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := writeCfg(t, withPricing(tc.section))
			before := readFile(t, p)
			var out bytes.Buffer
			_, err := migrateBobPricing(p, &out)
			if err == nil || !strings.Contains(err.Error(), "flow style") {
				t.Fatalf("err = %v, want a flow-style refusal", err)
			}
			if readFile(t, p) != before {
				t.Error("the file was modified")
			}
		})
	}
}

// TestMigrateBobPricing_RefusesAnUnparseableConfig: rewriting a file we cannot read
// would destroy settings we never understood.
func TestMigrateBobPricing_RefusesAnUnparseableConfig(t *testing.T) {
	p := writeCfg(t, "listener:\n  roles: [forward\n")
	var out bytes.Buffer
	if _, err := migrateBobPricing(p, &out); err == nil {
		t.Fatal("accepted an unparseable config")
	}
	if !strings.Contains(readFile(t, p), "roles: [forward") {
		t.Error("the unparseable file was modified")
	}
}

// TestReplaceConfig_VerifyFailureKeepsTheOriginal: a result that loads but fails the
// caller's check must not reach the file, and must not leave its temp file behind.
func TestReplaceConfig_VerifyFailureKeepsTheOriginal(t *testing.T) {
	p := writeCfg(t, pinnedConfig)
	before := readFile(t, p)
	err := replaceConfig(p, before+"pricing:\n  bundled: false\n", func(*config.Config) error {
		return os.ErrInvalid
	})
	if err == nil {
		t.Fatal("a failed verify was not reported")
	}
	if readFile(t, p) != before {
		t.Error("the file was replaced despite the failed verify")
	}
	// The temp name is CreateTemp's (".<base>.tmp<random>"), so the check is a
	// directory glob: a stat of a fixed path+".tmp" sees nothing either way.
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(p), "."+filepath.Base(p)+".tmp*")); len(left) > 0 {
		t.Errorf("temp file left behind: %v", left)
	}
}
