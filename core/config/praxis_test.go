package config

import (
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
	"gopkg.in/yaml.v3"
)

func TestPraxis_DefaultOff(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("mode: proxy-sidecar\n"), &c); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	if c.PraxisEnabled() {
		t.Fatal("PraxisEnabled() is true with no praxis block")
	}
	if err := yaml.Unmarshal([]byte("mode: proxy-sidecar\npraxis:\n  enabled: true\n"), &c); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	if !c.PraxisEnabled() {
		t.Fatal("praxis.enabled: true did not parse")
	}
}

func TestPraxis_NeedsLoopbackOnly(t *testing.T) {
	c := praxisConfig()
	err := Validate(c)
	if err == nil || !strings.Contains(err.Error(), "listener.bind_loopback_only") {
		t.Fatalf("Validate = %v, want an error naming listener.bind_loopback_only", err)
	}
	c.Listener.BindLoopbackOnly = true
	if err := Validate(c); err != nil {
		t.Fatalf("Validate with bind_loopback_only: %v", err)
	}
}

func TestPraxis_RefusedWithMTLS(t *testing.T) {
	c := praxisConfig()
	c.MTLS = &MTLSConfig{}
	c.Listener.BindLoopbackOnly = true
	err := Validate(c)
	if err == nil || !strings.Contains(err.Error(), "mtls") {
		t.Fatalf("Validate = %v, want an error naming mtls", err)
	}
}

func TestPraxis_RefusedWithSessionsOff(t *testing.T) {
	c := praxisConfig()
	c.Listener.BindLoopbackOnly = true
	off := false
	c.Session.Enabled = &off
	err := Validate(c)
	if err == nil || !strings.Contains(err.Error(), "session.enabled") {
		t.Fatalf("Validate = %v, want an error naming session.enabled", err)
	}
}

// praxisConfig is a proxy-sidecar config that passes the listener checks, so
// only the praxis rules can refuse it.
func praxisConfig() *Config {
	c := &Config{Mode: ModeProxySidecar, Praxis: &PraxisConfig{Enabled: true}}
	c.Listener.ReverseProxyBackend = "127.0.0.1:8000"
	return c
}

func TestPraxis_AddrDefaults(t *testing.T) {
	var nilBlock *PraxisConfig
	for _, p := range []*PraxisConfig{nilBlock, {}} {
		if got := p.ListenAddr(); got != DefaultPraxisListen {
			t.Errorf("ListenAddr() = %q, want %q", got, DefaultPraxisListen)
		}
		if got := p.EgressAddr(); got != DefaultPraxisEgress {
			t.Errorf("EgressAddr() = %q, want %q", got, DefaultPraxisEgress)
		}
	}
	p := &PraxisConfig{Listen: "127.0.0.1:1", Egress: "127.0.0.1:2"}
	if p.ListenAddr() != "127.0.0.1:1" || p.EgressAddr() != "127.0.0.1:2" {
		t.Errorf("set fields not returned: %q %q", p.ListenAddr(), p.EgressAddr())
	}
}

func TestPraxis_AddrMustBeLoopback(t *testing.T) {
	for _, tc := range []struct {
		addr string
		ok   bool
	}{
		{"0.0.0.0:47605", false},
		{"localhost:47605", false},
		{"127.0.0.1", false},
		{"127.0.0.1:0", false},
		{"10.0.0.1:1", false},
		{"127.0.0.1:1", true},
		{"[::1]:2", true},
	} {
		for _, field := range []string{"praxis.listen", "praxis.egress"} {
			c := praxisConfig()
			c.Listener.BindLoopbackOnly = true
			if field == "praxis.listen" {
				c.Praxis.Listen = tc.addr
			} else {
				c.Praxis.Egress = tc.addr
			}
			err := Validate(c)
			if tc.ok && err != nil {
				t.Errorf("%s %q: Validate = %v, want nil", field, tc.addr, err)
			}
			if !tc.ok && (err == nil || !strings.Contains(err.Error(), field)) {
				t.Errorf("%s %q: Validate = %v, want an error naming %s", field, tc.addr, err, field)
			}
			// The checks run only in praxis mode.
			c.Praxis.Enabled = false
			if err := Validate(c); err != nil {
				t.Errorf("%s %q with praxis off: Validate = %v, want nil", field, tc.addr, err)
			}
		}
	}
}

// TestPraxis_RefusesUnsafeOutboundPlugin pins #1768: cortex-gateway runs no outbound
// plugin, so a plugin that can reject or add a credential would fail open.
func TestPraxis_RefusesUnsafeOutboundPlugin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refused bool
	}{
		{"inference-parser", false}, {"mcp-parser", false}, {"a2a-parser", false},
		{"tool-prune", false}, {"context-guru", false}, {"lineage", false},
		{"opa", true}, {"script", true}, {"token-exchange", true},
		{"inference-router", true}, {"no-such-plugin", true},
	} {
		c := praxisConfig()
		c.Listener.BindLoopbackOnly = true
		c.Pipeline.Outbound.Plugins = []PluginEntry{{Name: tc.name}}
		err := Validate(c)
		if tc.refused != (err != nil) {
			t.Errorf("%s: Validate = %v, refused want %v", tc.name, err, tc.refused)
			continue
		}
		if tc.refused && !strings.Contains(err.Error(), tc.name) {
			t.Errorf("%s: error %q does not name the plugin", tc.name, err)
		}
	}
}

// An on_error: off entry is never dispatched, in direct mode too, so praxis mode
// cannot fail open on it. observe still dispatches, so it is refused.
func TestPraxis_AllowsOffOutboundPlugin(t *testing.T) {
	for _, tc := range []struct {
		policy  pipeline.ErrorPolicy
		refused bool
	}{
		{pipeline.ErrorPolicyOff, false},
		{pipeline.ErrorPolicyObserve, true},
		{"", true},
	} {
		c := praxisConfig()
		c.Listener.BindLoopbackOnly = true
		c.Pipeline.Outbound.Plugins = []PluginEntry{{Name: "opa", OnError: tc.policy}}
		if err := Validate(c); tc.refused != (err != nil) {
			t.Errorf("on_error %q: Validate = %v, refused want %v", tc.policy, err, tc.refused)
		}
	}
}
