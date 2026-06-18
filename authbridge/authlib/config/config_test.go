package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// --- Preset Tests ---

func TestApplyPreset_EnvoySidecar(t *testing.T) {
	cfg := &Config{Mode: ModeEnvoySidecar}
	ApplyPreset(cfg)
	if cfg.Listener.ExtProcAddr != ":9090" {
		t.Errorf("ext_proc_addr = %q, want :9090", cfg.Listener.ExtProcAddr)
	}
	if cfg.Listener.SessionAPIAddr != ":9094" {
		t.Errorf("session_api_addr = %q, want :9094", cfg.Listener.SessionAPIAddr)
	}
}

func TestApplyPreset_Waypoint(t *testing.T) {
	cfg := &Config{Mode: ModeWaypoint}
	ApplyPreset(cfg)
	if cfg.Listener.ExtAuthzAddr != ":9090" {
		t.Errorf("ext_authz_addr = %q, want :9090", cfg.Listener.ExtAuthzAddr)
	}
	if cfg.Listener.ForwardProxyAddr != ":8080" {
		t.Errorf("forward_proxy_addr = %q, want :8080", cfg.Listener.ForwardProxyAddr)
	}
}

func TestApplyPreset_ProxySidecar(t *testing.T) {
	cfg := &Config{Mode: ModeProxySidecar}
	ApplyPreset(cfg)
	if cfg.Listener.ReverseProxyAddr != ":8080" {
		t.Errorf("reverse_proxy_addr = %q, want :8080", cfg.Listener.ReverseProxyAddr)
	}
	if cfg.Listener.ForwardProxyAddr != ":8081" {
		t.Errorf("forward_proxy_addr = %q, want :8081", cfg.Listener.ForwardProxyAddr)
	}
}

func TestApplyPreset_UserOverride(t *testing.T) {
	cfg := &Config{
		Mode:     ModeEnvoySidecar,
		Listener: ListenerConfig{ExtProcAddr: ":19090"}, // user override
	}
	ApplyPreset(cfg)
	if cfg.Listener.ExtProcAddr != ":19090" {
		t.Errorf("user override lost: ext_proc_addr = %q", cfg.Listener.ExtProcAddr)
	}
}

// --- Validation Tests ---

func TestValidate_MissingMode(t *testing.T) {
	cfg := &Config{}
	if err := Validate(cfg); err == nil {
		t.Error("expected error for missing mode")
	}
}

func TestValidate_InvalidMode(t *testing.T) {
	cfg := &Config{Mode: "invalid"}
	if err := Validate(cfg); err == nil {
		t.Error("expected error for invalid mode")
	}
}

func TestValidate_InvalidListenerCombo(t *testing.T) {
	cfg := &Config{
		Mode:     ModeEnvoySidecar,
		Listener: ListenerConfig{ReverseProxyAddr: ":8080"}, // invalid for envoy-sidecar
	}
	if err := Validate(cfg); err == nil {
		t.Error("expected error for envoy-sidecar + reverse_proxy_addr")
	}
}

func TestValidate_WaypointRejectsExtProc(t *testing.T) {
	cfg := &Config{
		Mode:     ModeWaypoint,
		Listener: ListenerConfig{ExtProcAddr: ":9090"},
	}
	if err := Validate(cfg); err == nil {
		t.Error("expected error for waypoint + ext_proc_addr")
	}
}

func TestValidate_ProxySidecarRequiresBackend(t *testing.T) {
	cfg := &Config{Mode: ModeProxySidecar}
	if err := Validate(cfg); err == nil {
		t.Error("expected error for proxy-sidecar without backend")
	}
}

func TestValidate_ValidConfigs(t *testing.T) {
	withPipeline := func(c *Config) *Config {
		c.Pipeline = PipelineConfig{
			Inbound:  PipelineStageConfig{Plugins: []PluginEntry{{Name: "jwt-validation"}}},
			Outbound: PipelineStageConfig{Plugins: []PluginEntry{{Name: "token-exchange"}}},
		}
		return c
	}
	for _, cfg := range []*Config{
		withPipeline(&Config{Mode: ModeEnvoySidecar}),
		withPipeline(&Config{Mode: ModeWaypoint}),
		withPipeline(&Config{Mode: ModeProxySidecar, Listener: ListenerConfig{ReverseProxyBackend: "http://upstream"}}),
	} {
		if err := Validate(cfg); err != nil {
			t.Errorf("unexpected error for mode %s: %v", cfg.Mode, err)
		}
	}
}

// TestValidate_EmptyPipelineAllowed pins the "empty pipeline = pass-through"
// contract. An operator who wants to run AuthBridge without inbound
// validation (e.g. for testing, or asymmetric deployments where only
// outbound token exchange is desired) should be able to do so without
// adding a new opt-in config item. WarnEmptyPipelines emits a startup
// WARN so the open-proxy condition is still visible in logs.
//
// Previously this case was a hard rejection. The schema-migration
// scenario it guarded against (operators upgrading from the pre-migration
// top-level schema and silently landing on an empty pipeline) is now
// surfaced by the WARN log instead of by a boot failure.
func TestValidate_EmptyPipelineAllowed(t *testing.T) {
	cfg := &Config{Mode: ModeEnvoySidecar}
	if err := Validate(cfg); err != nil {
		t.Fatalf("empty pipeline should be allowed, got: %v", err)
	}
}

func TestValidate_EmptyOutboundPipelineAllowed(t *testing.T) {
	cfg := &Config{
		Mode: ModeEnvoySidecar,
		Pipeline: PipelineConfig{
			Inbound: PipelineStageConfig{Plugins: []PluginEntry{{Name: "jwt-validation"}}},
			// Outbound intentionally empty
		},
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("empty outbound pipeline should be allowed, got: %v", err)
	}
}

// PluginEntry's UnmarshalYAML treats `config: null` as no config at
// all. A literal null must not be forwarded to Configurable-gate as
// four bytes "null" — that would spuriously reject non-Configurable
// plugins that the operator explicitly authored with a null config
// (e.g. to emphasize "no settings").
func TestPluginEntry_NullConfigIsTreatedAsUnset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `mode: envoy-sidecar
pipeline:
  inbound:
    plugins:
      - name: jwt-validation
      - name: a2a-parser
        config: null
  outbound:
    plugins:
      - token-exchange
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	entries := cfg.Pipeline.Inbound.Plugins
	if len(entries) != 2 {
		t.Fatalf("entries len = %d, want 2", len(entries))
	}
	if len(entries[1].Config) != 0 {
		t.Errorf("a2a-parser Config = %q, want empty after config: null normalization",
			entries[1].Config)
	}
}

// --- Load Tests ---

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `mode: waypoint
listener:
  ext_authz_addr: "${TEST_ADDR}"
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	os.Setenv("TEST_ADDR", ":19090")
	defer os.Unsetenv("TEST_ADDR")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Mode != ModeWaypoint {
		t.Errorf("mode = %q, want waypoint", cfg.Mode)
	}
	if cfg.Listener.ExtAuthzAddr != ":19090" {
		t.Errorf("ext_authz_addr = %q, want expanded value", cfg.Listener.ExtAuthzAddr)
	}
}

// --- PluginEntry YAML parsing ---

// Pipeline configs continue to accept bare plugin names. Bare names
// mean "this plugin with no config," which is the right behavior for
// parsers (a2a-parser / mcp-parser / inference-parser) — they don't
// implement Configurable and have nothing to configure.
func TestPluginEntry_BareStringForm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `mode: envoy-sidecar
pipeline:
  inbound:
    plugins:
      - jwt-validation
      - a2a-parser
  outbound:
    plugins:
      - token-exchange
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	inbound := cfg.Pipeline.Inbound.Plugins
	if len(inbound) != 2 {
		t.Fatalf("inbound len = %d, want 2", len(inbound))
	}
	if inbound[0].Name != "jwt-validation" || len(inbound[0].Config) != 0 {
		t.Errorf("inbound[0] = %+v, want {jwt-validation, nil config}", inbound[0])
	}
	if inbound[1].Name != "a2a-parser" || len(inbound[1].Config) != 0 {
		t.Errorf("inbound[1] = %+v, want {a2a-parser, nil config}", inbound[1])
	}
}

// The richer form captures config as a raw JSON subtree. The framework
// doesn't interpret it; the plugin decodes against its own typed
// struct. Assert the bytes round-trip the operator's YAML faithfully
// (scalars preserved, nested maps intact) because that's the contract
// plugins rely on for DisallowUnknownFields decoding.
func TestPluginEntry_FullForm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `mode: envoy-sidecar
pipeline:
  inbound:
    plugins:
      - name: jwt-validation
        id: jwt-validation
        config:
          issuer: "http://keycloak.example/realms/kagenti"
          bypass_paths:
            - /healthz
            - /.well-known/*
          nested:
            count: 3
            enabled: true
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	entries := cfg.Pipeline.Inbound.Plugins
	if len(entries) != 1 {
		t.Fatalf("entries len = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Name != "jwt-validation" || e.ID != "jwt-validation" {
		t.Errorf("name/id = %q/%q, want jwt-validation/jwt-validation", e.Name, e.ID)
	}
	var decoded map[string]any
	if err := json.Unmarshal(e.Config, &decoded); err != nil {
		t.Fatalf("config JSON parse: %v\nbytes: %s", err, e.Config)
	}
	if decoded["issuer"] != "http://keycloak.example/realms/kagenti" {
		t.Errorf("issuer round-trip lost: %v", decoded["issuer"])
	}
	paths, ok := decoded["bypass_paths"].([]any)
	if !ok || len(paths) != 2 {
		t.Errorf("bypass_paths = %v, want 2-element list", decoded["bypass_paths"])
	}
	nested, ok := decoded["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested lost shape: %v", decoded["nested"])
	}
	if got, want := nested["count"], float64(3); got != want {
		t.Errorf("nested.count = %v, want 3", got)
	}
	if nested["enabled"] != true {
		t.Errorf("nested.enabled = %v, want true", nested["enabled"])
	}
}

// ID stays empty when omitted; callers default to Name themselves (at
// Build time, which this test does not exercise).
func TestPluginEntry_IDOmittedStaysEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `mode: envoy-sidecar
pipeline:
  inbound:
    plugins:
      - name: jwt-validation
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	e := cfg.Pipeline.Inbound.Plugins[0]
	if e.ID != "" {
		t.Errorf("omitted id = %q, want empty (defaulting happens at Build)", e.ID)
	}
}

// --- Credential file helpers ---

func TestReadCredentialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client-id.txt")
	if err := os.WriteFile(path, []byte("  my-client-id\n"), 0600); err != nil {
		t.Fatal(err)
	}
	v, err := ReadCredentialFile(path)
	if err != nil {
		t.Fatalf("ReadCredentialFile: %v", err)
	}
	if v != "my-client-id" {
		t.Errorf("got %q, want %q (trimmed)", v, "my-client-id")
	}
}

func TestReadCredentialFile_Missing(t *testing.T) {
	_, err := ReadCredentialFile(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestReadCredentialFile_Empty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(path, []byte{}, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := ReadCredentialFile(path)
	if err == nil {
		t.Error("expected error for empty file")
	}
}

func TestWaitForCredentialFile_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := WaitForCredentialFile(ctx, filepath.Join(t.TempDir(), "never"))
	if err == nil {
		t.Error("expected error when ctx cancels before file appears")
	}
}

// TestWaitForCredentialFile_HeartbeatFires verifies that the
// heartbeat log path is reachable while the file is absent. The actual
// slog output isn't captured (stdlib slog has no test hook without a
// handler swap) — this test just ensures the heartbeat branch in the
// select loop is wired up, by lowering the interval and letting ctx
// time out after a heartbeat has fired.
func TestWaitForCredentialFile_HeartbeatFires(t *testing.T) {
	orig := heartbeatInterval
	heartbeatInterval = 50 * time.Millisecond
	defer func() { heartbeatInterval = orig }()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	// 200ms is enough for at least one heartbeat tick even under CI
	// load. The assertion below is indirect: if the heartbeat branch
	// panicked (missing slog import, nil deref on the ticker, etc.),
	// the goroutine would crash and the test harness would surface it.
	_, err := WaitForCredentialFile(ctx, filepath.Join(t.TempDir(), "never"))
	if err == nil {
		t.Error("expected error when ctx cancels before file appears")
	}
}

// --- SessionConfig tri-state ---

func TestSessionConfig_SessionEnabled(t *testing.T) {
	trueP := func(b bool) *bool { return &b }

	tests := []struct {
		name string
		cfg  SessionConfig
		want bool
	}{
		{"unset defaults to true", SessionConfig{Enabled: nil}, true},
		{"explicit true", SessionConfig{Enabled: trueP(true)}, true},
		{"explicit false", SessionConfig{Enabled: trueP(false)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.SessionEnabled(); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// TestPluginEntry_OnError covers the three accepted policy values plus
// the omitted case. An invalid policy must fail loud at YAML parse
// time so operators catch typos before the pod boots.
func TestPluginEntry_OnError(t *testing.T) {
	cases := []struct {
		name     string
		yaml     string
		want     pipeline.ErrorPolicy
		wantFail bool
	}{
		{"enforce explicit", "enforce", pipeline.ErrorPolicyEnforce, false},
		{"observe", "observe", pipeline.ErrorPolicyObserve, false},
		{"off", "off", pipeline.ErrorPolicyOff, false},
		{"typo rejected", "observer", "", true},
		{"upper rejected", "ENFORCE", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			content := "mode: envoy-sidecar\n" +
				"pipeline:\n" +
				"  inbound:\n" +
				"    plugins:\n" +
				"      - name: custom-guardrail\n" +
				"        on_error: " + c.yaml + "\n"
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if c.wantFail {
				if err == nil {
					t.Fatalf("expected parse error for on_error=%q", c.yaml)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got := cfg.Pipeline.Inbound.Plugins[0].OnError
			if got != c.want {
				t.Errorf("OnError = %q, want %q", got, c.want)
			}
		})
	}
}

// TestPluginEntry_OnError_Omitted verifies the empty-string default.
// Resolved() should treat an absent on_error as enforce; the parsed
// field itself stays empty so round-tripping YAML doesn't invent a
// value the operator didn't write.
func TestPluginEntry_OnError_Omitted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "mode: envoy-sidecar\n" +
		"pipeline:\n" +
		"  inbound:\n" +
		"    plugins:\n" +
		"      - name: custom-guardrail\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.Pipeline.Inbound.Plugins[0].OnError
	if got != "" {
		t.Errorf("omitted OnError parsed as %q, want empty", got)
	}
	if got.Resolved() != pipeline.ErrorPolicyEnforce {
		t.Errorf("Resolved() of empty = %q, want enforce", got.Resolved())
	}
}

// --- mTLS config ---

// MTLSConfig.Validate accepts the two named modes plus empty (which
// resolves to permissive). Anything else is rejected so a typo in
// the YAML doesn't silently fall through to "no TLS."
func TestMTLSConfig_Validate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     *MTLSConfig
		wantErr bool
	}{
		{"nil — fine, no mTLS", nil, false},
		{"empty mode — permissive default", &MTLSConfig{}, false},
		{"explicit permissive", &MTLSConfig{Mode: MTLSModePermissive}, false},
		{"explicit strict", &MTLSConfig{Mode: MTLSModeStrict}, false},
		{"unknown mode", &MTLSConfig{Mode: "loose"}, true},
		{"typo PERMISSIVE", &MTLSConfig{Mode: "PERMISSIVE"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// ResolvedMode applies the empty-string default. Locks the
// MTLSModePermissive default so a future code change can't silently
// flip it.
func TestMTLSConfig_ResolvedMode(t *testing.T) {
	cases := []struct {
		name string
		cfg  *MTLSConfig
		want MTLSMode
	}{
		{"nil → permissive", nil, MTLSModePermissive},
		{"empty mode → permissive", &MTLSConfig{}, MTLSModePermissive},
		{"explicit strict stays strict", &MTLSConfig{Mode: MTLSModeStrict}, MTLSModeStrict},
		{"explicit permissive stays permissive", &MTLSConfig{Mode: MTLSModePermissive}, MTLSModePermissive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.ResolvedMode(); got != tc.want {
				t.Errorf("ResolvedMode = %q, want %q", got, tc.want)
			}
		})
	}
}

// Load preserves the mtls.mode value; SVID material is sourced from the
// SPIFFE Provider (see TestSPIFFEConfig_Defaults) and is no longer part
// of the mtls block.
func TestLoad_MTLS_ModeOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `mode: proxy-sidecar
listener:
  reverse_proxy_addr: ":8080"
  forward_proxy_addr: ":8081"
  reverse_proxy_backend: "http://localhost:8001"
mtls:
  mode: strict
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MTLS == nil {
		t.Fatal("MTLS unexpectedly nil")
	}
	if cfg.MTLS.Mode != MTLSModeStrict {
		t.Errorf("Mode = %q, want strict", cfg.MTLS.Mode)
	}
}

// Load surfaces an unknown mode as a configuration error rather than
// silently ignoring it (which would leave operators thinking they
// have mTLS when they don't).
func TestLoad_MTLS_UnknownModeRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `mode: proxy-sidecar
listener:
  reverse_proxy_addr: ":8080"
  forward_proxy_addr: ":8081"
  reverse_proxy_backend: "http://localhost:8001"
mtls:
  mode: loose
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load: expected error on unknown mtls.mode")
	}
}

// --- SPIFFE config ---

// Load applies SPIFFE defaults that match today's spiffe-helper-driven
// setup: the SPIRE agent socket path, mirror-files-on, and /opt mirror
// directory. JWT audience lives on the per-plugin tokenexchange.identity
// block now, not here.
func TestSPIFFEConfig_Defaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `mode: proxy-sidecar
listener:
  reverse_proxy_addr: ":8080"
  forward_proxy_addr: ":8081"
  reverse_proxy_backend: "http://localhost:8001"
spiffe: {}
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SPIFFE == nil {
		t.Fatal("SPIFFE block missing")
	}
	if cfg.SPIFFE.Socket != "unix:///spiffe-workload-api/spire-agent.sock" {
		t.Errorf("Socket = %q, want default", cfg.SPIFFE.Socket)
	}
	if cfg.SPIFFE.MirrorFiles == nil || !*cfg.SPIFFE.MirrorFiles {
		t.Error("MirrorFiles should default true")
	}
	if cfg.SPIFFE.MirrorDir != "/opt" {
		t.Errorf("MirrorDir = %q, want /opt", cfg.SPIFFE.MirrorDir)
	}
}

// SPIFFEConfig.Validate rejects sockets that aren't unix:// URLs. The
// Workload API only speaks over a unix domain socket in our deployment
// model; a tcp:// or http:// scheme is almost certainly an operator
// typo and should fail loud at startup rather than at first dial.
func TestSPIFFEConfig_Validate_BadSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `mode: proxy-sidecar
listener:
  reverse_proxy_addr: ":8080"
  forward_proxy_addr: ":8081"
  reverse_proxy_backend: "http://localhost:8001"
spiffe:
  socket: "tcp://oops"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected validation error for non-unix:// socket")
	}
}

// A config that doesn't mention spiffe at all should still load —
// today's deployments without the new block must keep working.
func TestSPIFFEConfig_NotPresent_NoError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `mode: proxy-sidecar
listener:
  reverse_proxy_addr: ":8080"
  forward_proxy_addr: ":8081"
  reverse_proxy_backend: "http://localhost:8001"
mtls:
  mode: permissive
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SPIFFE != nil {
		t.Errorf("SPIFFE should be nil when block not present, got %+v", cfg.SPIFFE)
	}
}

// TestLoad_UnknownMTLSFields_Ignored pins back-compat for in-flight chart
// configs that still carry the legacy mtls.cert_file / key_file / bundle_file
// keys. After the SPIFFE Provider migration (Task 7) those fields are gone
// from MTLSConfig, but yaml.v3's default decoder silently drops unknown keys
// — this test fails loudly if a future change tightens the loader to
// reject unknowns (e.g. by adding KnownFields(true)) and would otherwise
// break running deployments mid-migration.
func TestLoad_UnknownMTLSFields_Ignored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `mode: proxy-sidecar
listener:
  reverse_proxy_addr: ":8080"
  forward_proxy_addr: ":8081"
  reverse_proxy_backend: "http://localhost:8001"
mtls:
  mode: permissive
  cert_file: /opt/svid.pem
  key_file: /opt/svid_key.pem
  bundle_file: /opt/svid_bundle.pem
spiffe: {}
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load should ignore unknown mtls.cert_file/key_file/bundle_file, got %v", err)
	}
	if cfg.MTLS == nil {
		t.Fatal("mtls block missing after load")
	}
	if cfg.MTLS.Mode != MTLSModePermissive {
		t.Errorf("mtls.mode = %q, want %q", cfg.MTLS.Mode, MTLSModePermissive)
	}
}

// --- TLS bridge config ---

// The tls_bridge block decodes into TLSBridgeConfig and Load surfaces it.
func TestConfig_TLSBridgeBlockDecodes(t *testing.T) {
	y := []byte("mode: proxy-sidecar\n" +
		"tls_bridge:\n" +
		"  enabled: true\n" +
		"  scope: external\n" +
		"  ca_source: ephemeral\n" +
		"  skip_hosts: [\"pinned.example.com\"]\n")
	p := filepath.Join(t.TempDir(), "cfg.yaml")
	if err := os.WriteFile(p, y, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.TLSBridge == nil || !cfg.TLSBridge.Enabled || cfg.TLSBridge.Scope != "external" {
		t.Fatalf("tls_bridge block did not decode: %+v", cfg.TLSBridge)
	}
}

func TestTLSBridgeConfig_Validate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     TLSBridgeConfig
		wantErr bool
	}{
		{"valid empty", TLSBridgeConfig{}, false},
		{"valid full", TLSBridgeConfig{Scope: "all", CASource: "ephemeral", InternalCIDRs: []string{"10.0.0.0/8", "172.30.0.0/16"}}, false},
		{"bad scope", TLSBridgeConfig{Scope: "internal"}, true},
		{"bad ca_source", TLSBridgeConfig{CASource: "vault"}, true},
		{"file ca without paths", TLSBridgeConfig{CASource: "file"}, true},
		{"file ca with paths", TLSBridgeConfig{CASource: "file", CACertPath: "/c", CAKeyPath: "/k"}, false},
		{"bad cidr typo", TLSBridgeConfig{InternalCIDRs: []string{"10.0.0.0/8", "10.0.0.0./8"}}, true},
		{"bad cidr missing mask", TLSBridgeConfig{InternalCIDRs: []string{"10.0.0.0"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// Absent mtls block leaves cfg.MTLS nil — today's behavior, no TLS.
func TestLoad_MTLS_AbsentBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `mode: proxy-sidecar
listener:
  reverse_proxy_addr: ":8080"
  forward_proxy_addr: ":8081"
  reverse_proxy_backend: "http://localhost:8001"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MTLS != nil {
		t.Errorf("MTLS = %+v, want nil (absent block)", cfg.MTLS)
	}
}
