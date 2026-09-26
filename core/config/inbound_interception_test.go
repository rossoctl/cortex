package config

import "testing"

// TestApplyPreset_TransparentInbound covers the mutual exclusion between the
// two inbound mechanisms. Filling reverse_proxy_addr alongside transparent
// interception would bind a port nothing routes to — and if it collided with
// the agent's own port (which transparent mode deliberately leaves in place),
// the pod would fail to start.
func TestApplyPreset_TransparentInbound(t *testing.T) {
	cfg := &Config{Mode: ModeProxySidecar, Listener: ListenerConfig{
		InboundInterception: InboundInterceptionTransparent,
	}}
	ApplyPreset(cfg)

	if cfg.Listener.TransparentInboundAddr != ":8083" {
		t.Errorf("transparent_inbound_addr = %q, want :8083", cfg.Listener.TransparentInboundAddr)
	}
	if cfg.Listener.ReverseProxyAddr != "" {
		t.Errorf("transparent inbound must not fill reverse_proxy_addr, got %q", cfg.Listener.ReverseProxyAddr)
	}
	// The forward role is still active by default, so egress is untouched.
	if cfg.Listener.ForwardProxyAddr != ":8081" || cfg.Listener.TransparentProxyAddr != ":8082" {
		t.Errorf("egress defaults changed: forward=%q transparent-out=%q",
			cfg.Listener.ForwardProxyAddr, cfg.Listener.TransparentProxyAddr)
	}
}

// TestApplyPreset_DefaultIsReverseProxy locks the opt-in contract: an unset
// inbound_interception must leave the preset byte-identical to today.
func TestApplyPreset_DefaultIsReverseProxy(t *testing.T) {
	cfg := &Config{Mode: ModeProxySidecar}
	ApplyPreset(cfg)

	if cfg.Listener.ReverseProxyAddr != ":8080" {
		t.Errorf("reverse_proxy_addr = %q, want :8080 (default mechanism unchanged)", cfg.Listener.ReverseProxyAddr)
	}
	if cfg.Listener.TransparentInboundAddr != "" {
		t.Errorf("default must not fill transparent_inbound_addr, got %q", cfg.Listener.TransparentInboundAddr)
	}
}

// TestApplyPreset_TransparentInboundUserOverride ensures an operator-chosen port
// survives the preset — it has to match proxy-init's INBOUND_TRANSPARENT_PORT,
// so silently overwriting it would break ingress.
func TestApplyPreset_TransparentInboundUserOverride(t *testing.T) {
	cfg := &Config{Mode: ModeProxySidecar, Listener: ListenerConfig{
		InboundInterception:    InboundInterceptionTransparent,
		TransparentInboundAddr: ":19083",
	}}
	ApplyPreset(cfg)
	if cfg.Listener.TransparentInboundAddr != ":19083" {
		t.Errorf("transparent_inbound_addr = %q, want the operator's :19083", cfg.Listener.TransparentInboundAddr)
	}
}

func TestInboundTransparent(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false},
		{InboundInterceptionReverseProxy, false},
		{InboundInterceptionTransparent, true},
	} {
		if got := (ListenerConfig{InboundInterception: tc.value}).InboundTransparent(); got != tc.want {
			t.Errorf("InboundTransparent(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

func TestValidate_InboundInterception(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *Config
		wantErr bool
	}{
		{
			name: "transparent needs no reverse_proxy_backend",
			cfg: &Config{Mode: ModeProxySidecar, Listener: ListenerConfig{
				InboundInterception: InboundInterceptionTransparent,
			}},
		},
		{
			name: "reverse-proxy still requires a backend",
			cfg: &Config{Mode: ModeProxySidecar, Listener: ListenerConfig{
				InboundInterception: InboundInterceptionReverseProxy,
			}},
			wantErr: true,
		},
		{
			name: "default still requires a backend",
			cfg:  &Config{Mode: ModeProxySidecar},
			// unchanged behavior for existing configs
			wantErr: true,
		},
		{
			name: "unknown value is rejected at startup",
			cfg: &Config{Mode: ModeProxySidecar, Listener: ListenerConfig{
				InboundInterception: "transparant", // typo an operator would make
				ReverseProxyBackend: "http://127.0.0.1:8000",
			}},
			wantErr: true,
		},
		{
			name: "transparent without the reverse role is a no-op, so rejected",
			cfg: &Config{Mode: ModeProxySidecar, Listener: ListenerConfig{
				Roles:               []string{RoleForward},
				InboundInterception: InboundInterceptionTransparent,
			}},
			wantErr: true,
		},
		{
			name: "envoy-sidecar rejects the field (Envoy already intercepts inbound)",
			cfg: &Config{Mode: ModeEnvoySidecar, Listener: ListenerConfig{
				InboundInterception: InboundInterceptionTransparent,
			}},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.cfg)
			if tc.wantErr && err == nil {
				t.Fatal("expected a validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}
