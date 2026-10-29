package config

import "testing"

// TestTLSBridge_SessionAPILoopbackWhenAddrUnset: with the bridge enabled and
// NO explicit session_api_addr, the Load-time loopback rewrite runs while the
// address is still empty (forceLocalhost("") == ""), and ApplyPreset then
// fills the wildcard ":9094" — leaving the session API, which carries
// DECRYPTED bodies under the bridge, bound on every interface. This is the
// exact ordering trap the BindLoopbackOnly block's comment in presets.go
// describes. demos/context-guru/context-guru-tls-bridge.yaml has this shape.
func TestTLSBridge_SessionAPILoopbackWhenAddrUnset(t *testing.T) {
	cfg := loadFrom(t, `mode: proxy-sidecar
listener:
  roles: [forward]
tls_bridge:
  mode: enabled
  ca_dir: /etc/authbridge/tls-bridge-ca
`)
	if cfg.Listener.SessionAPIAddr != "127.0.0.1:9094" {
		t.Errorf("session_api_addr = %q, want 127.0.0.1:9094 (bridge on and addr unset => the default must still be pinned to loopback)",
			cfg.Listener.SessionAPIAddr)
	}
}

// The explicit-address arm keeps working (TestLoad_TLSBridgeHardensSessionAPI
// in config_test.go covers it with the address spelled out).
