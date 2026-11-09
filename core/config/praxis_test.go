package config

import (
	"strings"
	"testing"

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

// praxisConfig is a proxy-sidecar config that passes the listener checks, so
// only the praxis rules can refuse it.
func praxisConfig() *Config {
	c := &Config{Mode: ModeProxySidecar, Praxis: &PraxisConfig{Enabled: true}}
	c.Listener.ReverseProxyBackend = "127.0.0.1:8000"
	return c
}
