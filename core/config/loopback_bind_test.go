package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadFrom(t *testing.T, body string) *Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Mirror the binaries: main.go calls Load and then ApplyPreset. Testing Load
	// alone hides ordering bugs — the first version of this rule sat in Load, ran
	// before the wildcard defaults existed, and protected nothing.
	ApplyPreset(cfg)
	return cfg
}

// TestBindLoopbackOnly_CoversListenersNobodyPinned is the whole point: the two
// addresses a laptop config forgot were serving on every interface. The flag has to
// protect the ones nobody named, including any added later.
func TestBindLoopbackOnly_CoversListenersNobodyPinned(t *testing.T) {
	cfg := loadFrom(t, `mode: proxy-sidecar
listener:
  roles: [forward]
  bind_loopback_only: true
  forward_proxy_addr: 127.0.0.1:47600
  session_api_addr: 127.0.0.1:47601
`)
	// health and transparent were never named; the preset gave them :9091 / :8082.
	for name, got := range map[string]string{
		"health_addr":            cfg.Listener.HealthAddr,
		"transparent_proxy_addr": cfg.Listener.TransparentProxyAddr,
		"stats address":          cfg.Stats.StatsAddress,
	} {
		if got == "" {
			continue // not defaulted in this mode; nothing to protect
		}
		if !strings.HasPrefix(got, "127.0.0.1:") {
			t.Errorf("%s = %q, want a 127.0.0.1 bind — this is the *:9091 bug", name, got)
		}
	}
	// Ports must survive: this rewrites the host, it does not renumber anything.
	if cfg.Listener.HealthAddr != "127.0.0.1:9091" {
		t.Errorf("health_addr = %q, want the preset port kept", cfg.Listener.HealthAddr)
	}
	// And an address the operator DID pin is untouched.
	if cfg.Listener.ForwardProxyAddr != "127.0.0.1:47600" {
		t.Errorf("forward_proxy_addr = %q", cfg.Listener.ForwardProxyAddr)
	}
}

// TestBindLoopbackOnly_OffByDefault: Kubernetes must keep wildcard binds. Forcing
// health to loopback would fail every kubelet liveness probe.
func TestBindLoopbackOnly_OffByDefault(t *testing.T) {
	cfg := loadFrom(t, "mode: proxy-sidecar\nlistener:\n  roles: [forward]\n")
	if cfg.Listener.BindLoopbackOnly {
		t.Fatal("bind_loopback_only defaulted to true; that would break kubelet probes")
	}
	if cfg.Listener.HealthAddr != ":9091" {
		t.Errorf("health_addr = %q, want the wildcard preset untouched", cfg.Listener.HealthAddr)
	}
}

// TestBindLoopbackOnly_RewritesExplicitWildcards: someone may write 0.0.0.0 or
// [::] by hand. The flag has to win over that too, or it is advisory.
func TestBindLoopbackOnly_RewritesExplicitWildcards(t *testing.T) {
	cfg := loadFrom(t, `mode: proxy-sidecar
listener:
  roles: [forward]
  bind_loopback_only: true
  forward_proxy_addr: 0.0.0.0:47600
  session_api_addr: "[::]:47601"
  health_addr: ":47604"
`)
	for name, got := range map[string]string{
		"forward": cfg.Listener.ForwardProxyAddr,
		"session": cfg.Listener.SessionAPIAddr,
		"health":  cfg.Listener.HealthAddr,
	} {
		if !strings.HasPrefix(got, "127.0.0.1:") {
			t.Errorf("%s = %q, want 127.0.0.1", name, got)
		}
	}
	if cfg.Listener.SessionAPIAddr != "127.0.0.1:47601" {
		t.Errorf("IPv6 wildcard mishandled: %q", cfg.Listener.SessionAPIAddr)
	}
}

// TestBindLoopbackOnly_LeavesEmptyAddressesEmpty: an address that is genuinely
// unset must not become a live 127.0.0.1 listener. (Note: an explicit
// session_api_addr: "" does NOT stay empty — setDefault treats "" as unset and the
// preset refills it. That is pre-existing, documented behaviour, not this rule.)
func TestBindLoopbackOnly_LeavesEmptyAddressesEmpty(t *testing.T) {
	cfg := loadFrom(t, `mode: proxy-sidecar
listener:
  roles: [forward]
  bind_loopback_only: true
`)
	// ext_proc is only defaulted for envoy-sidecar, so in this mode it stays unset.
	if cfg.Listener.ExtProcAddr != "" {
		t.Errorf("ext_proc_addr = %q, want it to stay unset rather than become a listener",
			cfg.Listener.ExtProcAddr)
	}
}

// TestForceLocalhost_EmptyStaysEmpty pins the primitive directly, since the rule
// above leans on it for every address.
func TestForceLocalhost_EmptyStaysEmpty(t *testing.T) {
	if got := forceLocalhost(""); got != "" {
		t.Errorf("forceLocalhost(\"\") = %q, want empty", got)
	}
}
