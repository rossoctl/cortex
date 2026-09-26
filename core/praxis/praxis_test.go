package praxis

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/config"
	"gopkg.in/yaml.v3"
)

// proxySidecar builds a minimal proxy-sidecar config with presets applied, the
// way the binaries do at boot.
func proxySidecar(t *testing.T, mutate func(*config.Config)) *config.Config {
	t.Helper()
	cfg := &config.Config{
		Mode: config.ModeProxySidecar,
		Listener: config.ListenerConfig{
			ReverseProxyBackend: "http://localhost:8001",
		},
	}
	if mutate != nil {
		mutate(cfg)
	}
	config.ApplyPreset(cfg)
	if err := config.Validate(cfg); err != nil {
		t.Fatalf("fixture config is invalid: %v", err)
	}
	return cfg
}

func TestConvert_NilConfig(t *testing.T) {
	if _, err := Convert(nil, nil); err == nil {
		t.Fatal("expected an error for a nil config")
	}
}

func TestConvert_UnknownMode(t *testing.T) {
	if _, err := Convert(&config.Config{Mode: "bogus"}, nil); err == nil {
		t.Fatal("expected an error for an unknown mode")
	}
}

// The default proxy-sidecar shape runs both roles, so it should yield an
// inbound and an outbound listener, each with its own chain.
func TestConvert_BothRoles(t *testing.T) {
	res, err := Convert(proxySidecar(t, nil), nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if got, want := len(res.Config.Listeners), 2; got != want {
		t.Fatalf("listeners = %d, want %d", got, want)
	}
	names := []string{res.Config.Listeners[0].Name, res.Config.Listeners[1].Name}
	if names[0] != ListenerInbound || names[1] != ListenerOutbound {
		t.Errorf("listener names = %v, want [%s %s]", names, ListenerInbound, ListenerOutbound)
	}
	// ":8080" must become an explicit host:port; Praxis parses its address as
	// a socket address and rejects a bare ":port".
	if got, want := res.Config.Listeners[0].Address, "0.0.0.0:8080"; got != want {
		t.Errorf("inbound address = %q, want %q", got, want)
	}
	if len(res.Config.FilterChains) != 2 {
		t.Fatalf("filter chains = %d, want 2", len(res.Config.FilterChains))
	}
}

// A forward-only deployment has no application backend and must not emit an
// inbound listener.
func TestConvert_ForwardOnly(t *testing.T) {
	cfg := &config.Config{
		Mode:     config.ModeProxySidecar,
		Listener: config.ListenerConfig{Roles: []string{config.RoleForward}},
	}
	config.ApplyPreset(cfg)
	res, err := Convert(cfg, nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if len(res.Config.Listeners) != 1 {
		t.Fatalf("listeners = %d, want 1", len(res.Config.Listeners))
	}
	if res.Config.Listeners[0].Name != ListenerOutbound {
		t.Errorf("listener = %q, want %q", res.Config.Listeners[0].Name, ListenerOutbound)
	}
}

// A reverse-only deployment emits just the inbound listener, routed at the
// application backend.
func TestConvert_ReverseOnly(t *testing.T) {
	cfg := proxySidecar(t, func(c *config.Config) {
		c.Listener.Roles = []string{config.RoleReverse}
	})
	res, err := Convert(cfg, nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if len(res.Config.Listeners) != 1 {
		t.Fatalf("listeners = %d, want 1", len(res.Config.Listeners))
	}
	lb := lastFilter(t, res.Config.FilterChains[0], "load_balancer")
	clusters, ok := lb.Fields[0].Value.([]Cluster)
	if !ok {
		t.Fatalf("load_balancer clusters field has type %T", lb.Fields[0].Value)
	}
	if got, want := clusters[0].Endpoints[0], "localhost:8001"; got != want {
		t.Errorf("endpoint = %q, want %q", got, want)
	}
}

// Praxis requires that every cluster a router selects is defined on the
// load_balancer in the same chain. Guard the invariant directly, since a
// mismatch is a hard validation error at Praxis startup.
func TestConvert_RouterClusterIsDefined(t *testing.T) {
	res, err := Convert(proxySidecar(t, func(c *config.Config) {
		c.Listener.Roles = []string{config.RoleReverse}
	}), nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	chain := res.Config.FilterChains[0]
	router := lastFilter(t, chain, "router")
	routes, ok := router.Fields[0].Value.([]Route)
	if !ok {
		t.Fatalf("router routes field has type %T", router.Fields[0].Value)
	}
	lb := lastFilter(t, chain, "load_balancer")
	clusters := lb.Fields[0].Value.([]Cluster)

	defined := map[string]bool{}
	for _, c := range clusters {
		defined[c.Name] = true
	}
	for _, r := range routes {
		if !defined[r.Cluster] {
			t.Errorf("router selects cluster %q which no load_balancer defines", r.Cluster)
		}
	}
}

// Praxis rejects a load_balancer that is not preceded by a cluster-selecting
// filter, so router must come before load_balancer in the emitted order.
func TestConvert_RouterPrecedesLoadBalancer(t *testing.T) {
	res, err := Convert(proxySidecar(t, func(c *config.Config) {
		c.Listener.Roles = []string{config.RoleReverse}
	}), nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	var routerIdx, lbIdx = -1, -1
	for i, f := range res.Config.FilterChains[0].Filters {
		switch f.Type {
		case "router":
			routerIdx = i
		case "load_balancer":
			lbIdx = i
		}
	}
	if routerIdx == -1 || lbIdx == -1 {
		t.Fatalf("expected both router and load_balancer, got router=%d lb=%d", routerIdx, lbIdx)
	}
	if routerIdx > lbIdx {
		t.Errorf("router at %d must precede load_balancer at %d", routerIdx, lbIdx)
	}
}

func TestConvert_MTLSModes(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode config.MTLSMode
		want string
	}{
		{"permissive maps to request", config.MTLSModePermissive, "request"},
		{"empty defaults to permissive", "", "request"},
		{"strict maps to require", config.MTLSModeStrict, "require"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Convert(proxySidecar(t, func(c *config.Config) {
				c.MTLS = &config.MTLSConfig{Mode: tc.mode}
			}), nil)
			if err != nil {
				t.Fatalf("Convert: %v", err)
			}
			tls := res.Config.Listeners[0].TLS
			if tls == nil {
				t.Fatal("expected a TLS block")
			}
			if tls.ClientCertMode != tc.want {
				t.Errorf("client_cert_mode = %q, want %q", tls.ClientCertMode, tc.want)
			}
			// Praxis rejects request/require without a client_ca.
			if tls.ClientCA == nil || tls.ClientCA.CAPath == "" {
				t.Error("client_cert_mode set without a client_ca; Praxis rejects this")
			}
			if len(tls.Certificates) == 0 {
				t.Error("expected a serving certificate")
			}
		})
	}
}

// No mtls block means today's plaintext behavior, so no TLS should be emitted.
func TestConvert_NoMTLS_NoTLSBlock(t *testing.T) {
	res, err := Convert(proxySidecar(t, nil), nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if res.Config.Listeners[0].TLS != nil {
		t.Error("expected no TLS block when mtls is absent")
	}
}

// The SPIFFE mirror directory is where Praxis reads SVID material from, so a
// non-default mirror_dir must be reflected in the cert paths.
func TestConvert_MTLSUsesSPIFFEMirrorDir(t *testing.T) {
	res, err := Convert(proxySidecar(t, func(c *config.Config) {
		c.MTLS = &config.MTLSConfig{Mode: config.MTLSModeStrict}
		c.SPIFFE = &config.SPIFFEConfig{MirrorDir: "/var/run/svid"}
	}), nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	tls := res.Config.Listeners[0].TLS
	if got, want := tls.Certificates[0].CertPath, "/var/run/svid/svid.pem"; got != want {
		t.Errorf("cert_path = %q, want %q", got, want)
	}
	if got, want := tls.ClientCA.CAPath, "/var/run/svid/svid_bundle.pem"; got != want {
		t.Errorf("ca_path = %q, want %q", got, want)
	}
}

// certComment returns the comment block attached to the first listener's
// serving certificate.
func certComment(t *testing.T, res *Result) string {
	t.Helper()
	tls := res.Config.Listeners[0].TLS
	if tls == nil || len(tls.Certificates) == 0 {
		t.Fatal("expected a TLS block with a certificate")
	}
	return strings.Join(tls.Certificates[0].Comments, " ")
}

// The generated config must say where the referenced SVID files come from:
// they are not static assets, they are a running provider's disk mirror.
func TestConvert_MTLS_ExplainsSPIFFEProviderWritesCert(t *testing.T) {
	res, err := Convert(proxySidecar(t, func(c *config.Config) {
		c.MTLS = &config.MTLSConfig{Mode: config.MTLSModeStrict}
		c.SPIFFE = &config.SPIFFEConfig{Socket: "unix:///x.sock"}
	}), nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	comment := certComment(t, res)
	if !strings.Contains(comment, "spiffe.Provider") {
		t.Errorf("comment should name spiffe.Provider, got %q", comment)
	}
	if !strings.Contains(comment, "/opt/svid.pem") {
		t.Errorf("comment should name /opt/svid.pem, got %q", comment)
	}
	// A healthy provider is not a problem, so nothing should be warned about.
	if containsSubstring(res.Warnings, "will NOT be generated") {
		t.Errorf("unexpected missing-provider warning: %v", res.Warnings)
	}

	// The comment must survive into the rendered file, not just the struct.
	out, err := RenderResult(res, "/tmp/c.yaml")
	if err != nil {
		t.Fatalf("RenderResult: %v", err)
	}
	if !strings.Contains(string(out), "spiffe.Provider writes /opt/svid.pem") {
		t.Errorf("rendered config should explain the cert source:\n%s", out)
	}
}

// mtls with no spiffe block is the weather-service example's shape: nothing
// writes the SVID files, so Praxis cannot bind the listener. That must be a
// warning AND a comment, not a silently broken config.
func TestConvert_MTLSWithoutSPIFFE_WarnsAndComments(t *testing.T) {
	res, err := Convert(proxySidecar(t, func(c *config.Config) {
		c.MTLS = &config.MTLSConfig{Mode: config.MTLSModePermissive}
		// No SPIFFE block at all.
	}), nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if !containsSubstring(res.Warnings, "/opt/svid.pem") {
		t.Errorf("expected a warning naming /opt/svid.pem, got %v", res.Warnings)
	}
	if !containsSubstring(res.Warnings, "will NOT be generated") {
		t.Errorf("expected the warning to say the file is not generated, got %v", res.Warnings)
	}
	if !containsSubstring(res.Warnings, "spiffe") {
		t.Errorf("expected the warning to point at the spiffe block, got %v", res.Warnings)
	}

	comment := certComment(t, res)
	if !strings.Contains(comment, "will NOT exist") {
		t.Errorf("comment should warn the files are absent, got %q", comment)
	}
	if !strings.Contains(comment, "/opt/svid.pem") {
		t.Errorf("comment should name /opt/svid.pem, got %q", comment)
	}

	out, err := RenderResult(res, "/tmp/c.yaml")
	if err != nil {
		t.Fatalf("RenderResult: %v", err)
	}
	if !strings.Contains(string(out), "WARNING: these files will NOT exist") {
		t.Errorf("rendered config should carry the warning comment:\n%s", out)
	}
}

// A provider that runs with mirroring explicitly off keeps SVIDs in memory,
// which Praxis cannot read — same practical outcome as no provider.
func TestConvert_MTLSWithMirrorFilesDisabled_Warns(t *testing.T) {
	off := false
	res, err := Convert(proxySidecar(t, func(c *config.Config) {
		c.MTLS = &config.MTLSConfig{Mode: config.MTLSModeStrict}
		c.SPIFFE = &config.SPIFFEConfig{Socket: "unix:///x.sock", MirrorFiles: &off}
	}), nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if !containsSubstring(res.Warnings, "mirror_files") {
		t.Errorf("expected a mirror_files warning, got %v", res.Warnings)
	}
	if !strings.Contains(certComment(t, res), "will NOT exist") {
		t.Errorf("comment should warn the files are absent: %q", certComment(t, res))
	}
}

// mirror_files unset means the default (true), which is the working case.
func TestConvert_MTLSWithMirrorFilesUnset_NoWarning(t *testing.T) {
	res, err := Convert(proxySidecar(t, func(c *config.Config) {
		c.MTLS = &config.MTLSConfig{Mode: config.MTLSModeStrict}
		c.SPIFFE = &config.SPIFFEConfig{Socket: "unix:///x.sock"}
	}), nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if containsSubstring(res.Warnings, "mirror_files") {
		t.Errorf("mirror_files unset defaults to true and must not warn: %v", res.Warnings)
	}
}

// The comment must track a custom mirror_dir, or it would name a path the
// provider is not writing.
func TestConvert_MTLSCommentUsesMirrorDir(t *testing.T) {
	res, err := Convert(proxySidecar(t, func(c *config.Config) {
		c.MTLS = &config.MTLSConfig{Mode: config.MTLSModeStrict}
		c.SPIFFE = &config.SPIFFEConfig{Socket: "unix:///x.sock", MirrorDir: "/var/run/svid"}
	}), nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	comment := certComment(t, res)
	if !strings.Contains(comment, "/var/run/svid/svid.pem") {
		t.Errorf("comment should name the configured mirror dir, got %q", comment)
	}
}

// No mtls means no TLS block at all, so there is nothing to warn about even
// without a SPIFFE provider.
func TestConvert_NoMTLS_NoSVIDWarning(t *testing.T) {
	res, err := Convert(proxySidecar(t, nil), nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if containsSubstring(res.Warnings, "svid.pem") {
		t.Errorf("no mtls means no SVID warning, got %v", res.Warnings)
	}
}

// The outbound transparent listener is AuthBridge's hard egress guard —
// deliberately not self-exemptable — and the proxy-sidecar preset defaults it
// on. Dropping it silently would remove an enforcement boundary the operator
// never explicitly enabled.
func TestConvert_TransparentProxyAddr_Reported(t *testing.T) {
	res, err := Convert(proxySidecar(t, nil), nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	// The preset fills this for proxy-sidecar, so it should be reported without
	// the test setting it explicitly.
	if got := res.Config.Listeners; len(got) == 0 {
		t.Fatal("expected listeners")
	}
	if !containsSubstring(res.Warnings, "transparent_proxy_addr") {
		t.Errorf("expected transparent_proxy_addr reported, got %v", res.Warnings)
	}
	if !containsSubstring(res.Warnings, "egress guard") {
		t.Errorf("the warning should say what is lost, got %v", res.Warnings)
	}
}

func TestConvert_SkipHosts_Reported(t *testing.T) {
	res, err := Convert(proxySidecar(t, func(c *config.Config) {
		c.Listener.SkipHosts = []string{"otel-collector*", "*.metrics.local"}
	}), nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if !containsSubstring(res.Warnings, "skip_hosts") {
		t.Errorf("expected skip_hosts reported, got %v", res.Warnings)
	}
	if !containsSubstring(res.Warnings, "otel-collector*") {
		t.Errorf("the warning should name the patterns, got %v", res.Warnings)
	}
}

// No skip_hosts configured means nothing to report about them.
func TestConvert_NoSkipHosts_NotReported(t *testing.T) {
	res, err := Convert(proxySidecar(t, nil), nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if containsSubstring(res.Warnings, "skip_hosts") {
		t.Errorf("skip_hosts is unset and must not be reported: %v", res.Warnings)
	}
}

// Transparent inbound interception has no Praxis counterpart: Praxis cannot
// recover the original destination per connection. It must be reported rather
// than silently producing a listener pointed somewhere invented.
func TestConvert_TransparentInbound_Warns(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ModeProxySidecar,
		Listener: config.ListenerConfig{
			Roles:               []string{config.RoleReverse, config.RoleForward},
			InboundInterception: config.InboundInterceptionTransparent,
		},
	}
	config.ApplyPreset(cfg)
	res, err := Convert(cfg, nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	for _, l := range res.Config.Listeners {
		if l.Name == ListenerInbound {
			t.Error("expected no inbound listener for transparent interception")
		}
	}
	if !containsSubstring(res.Warnings, "SO_ORIGINAL_DST") {
		t.Errorf("expected a warning about SO_ORIGINAL_DST, got %v", res.Warnings)
	}
}

// A config that yields no listener at all is an error: Praxis requires at
// least one, so emitting an empty document would just fail later and further
// from the cause.
func TestConvert_NoListeners_IsError(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ModeProxySidecar,
		Listener: config.ListenerConfig{
			Roles:               []string{config.RoleReverse},
			InboundInterception: config.InboundInterceptionTransparent,
		},
	}
	if _, err := Convert(cfg, nil); err == nil {
		t.Fatal("expected an error when no listener can be generated")
	}
}

// Auth plugins must be reported as unmapped, not dropped silently: a
// generated proxy that no longer validates JWTs is a security-relevant
// difference from the AuthBridge config it came from.
func TestConvert_AuthPluginsReportedUnmapped(t *testing.T) {
	res, err := Convert(proxySidecar(t, func(c *config.Config) {
		c.Pipeline.Inbound.Plugins = []config.PluginEntry{{Name: "jwt-validation"}}
		c.Pipeline.Outbound.Plugins = []config.PluginEntry{{Name: "token-exchange"}}
	}), nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if !containsSubstring(res.Unmapped, "jwt-validation") {
		t.Errorf("expected jwt-validation reported unmapped, got %v", res.Unmapped)
	}
	if !containsSubstring(res.Unmapped, "token-exchange") {
		t.Errorf("expected token-exchange reported unmapped, got %v", res.Unmapped)
	}
}

// on_error: off means AuthBridge drops the plugin entirely, so it is not a
// translation gap and must not be reported as one.
func TestConvert_DisabledPluginNotReported(t *testing.T) {
	res, err := Convert(proxySidecar(t, func(c *config.Config) {
		c.Pipeline.Inbound.Plugins = []config.PluginEntry{{Name: "jwt-validation", OnError: "off"}}
	}), nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if containsSubstring(res.Unmapped, "jwt-validation") {
		t.Errorf("a plugin disabled with on_error: off must not be reported unmapped: %v", res.Unmapped)
	}
}

func TestConvert_UnrecognizedPluginReported(t *testing.T) {
	res, err := Convert(proxySidecar(t, func(c *config.Config) {
		c.Pipeline.Inbound.Plugins = []config.PluginEntry{{Name: "some-future-plugin"}}
	}), nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if !containsSubstring(res.Unmapped, "some-future-plugin") {
		t.Errorf("expected the unknown plugin reported, got %v", res.Unmapped)
	}
}

// The inbound chain must strip the direction header, matching AuthBridge's
// contract with the application behind it.
func TestConvert_StripsDirectionHeader(t *testing.T) {
	res, err := Convert(proxySidecar(t, func(c *config.Config) {
		c.Listener.Roles = []string{config.RoleReverse}
	}), nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	found := false
	for _, f := range res.Config.FilterChains[0].Filters {
		if f.Type != "headers" {
			continue
		}
		for _, fl := range f.Fields {
			if fl.Key != "request_remove" {
				continue
			}
			if names, ok := fl.Value.([]string); ok {
				for _, n := range names {
					if n == DirectionHeader {
						found = true
					}
				}
			}
		}
	}
	if !found {
		t.Errorf("expected the inbound chain to remove %q", DirectionHeader)
	}
}

func TestConvert_AdminFromStatsAddress(t *testing.T) {
	// The admin endpoint takes the HOST from stats.address but binds the health
	// port (9091), because /ready and /healthy correspond to AuthBridge's
	// /readyz and /healthz — not to the stats endpoints on 9093.
	t.Run("loopback needs no override", func(t *testing.T) {
		res, err := Convert(proxySidecar(t, func(c *config.Config) {
			c.Stats.StatsAddress = "127.0.0.1:9093"
		}), nil)
		if err != nil {
			t.Fatalf("Convert: %v", err)
		}
		if res.Config.Admin.Address != "127.0.0.1:9091" {
			t.Errorf("admin address = %q, want 127.0.0.1:9091", res.Config.Admin.Address)
		}
		if res.Config.InsecureOptions != nil {
			t.Error("loopback admin must not require allow_public_admin")
		}
	})

	// AuthBridge deliberately binds stats on all interfaces for the Rossoctl
	// UI. Praxis rejects that unless allow_public_admin is set, so the flag
	// must travel with the address rather than the address being rewritten.
	t.Run("public bind carries the override", func(t *testing.T) {
		res, err := Convert(proxySidecar(t, func(c *config.Config) {
			c.Stats.StatsAddress = ":9093"
		}), nil)
		if err != nil {
			t.Fatalf("Convert: %v", err)
		}
		if res.Config.Admin.Address != "0.0.0.0:9091" {
			t.Errorf("admin address = %q, want 0.0.0.0:9091", res.Config.Admin.Address)
		}
		if res.Config.InsecureOptions == nil || !res.Config.InsecureOptions.AllowPublicAdmin {
			t.Error("a non-loopback admin bind requires allow_public_admin")
		}
	})

	// config.Load always fills stats.address with :9093 when it is empty, so
	// this is the shape the binaries actually hand the converter. The admin
	// endpoint must still land on the health port.
	t.Run("stats port is not carried across", func(t *testing.T) {
		res, err := Convert(proxySidecar(t, func(c *config.Config) {
			c.Stats.StatsAddress = ":9093"
		}), nil)
		if err != nil {
			t.Fatalf("Convert: %v", err)
		}
		if strings.HasSuffix(res.Config.Admin.Address, ":9093") {
			t.Errorf("admin must not bind the stats port, got %q", res.Config.Admin.Address)
		}
	})

	// A non-default stats host must be preserved: it carries the operator's
	// reachability intent, which the port does not.
	t.Run("non-default host is preserved", func(t *testing.T) {
		res, err := Convert(proxySidecar(t, func(c *config.Config) {
			c.Stats.StatsAddress = "10.1.2.3:9999"
		}), nil)
		if err != nil {
			t.Fatalf("Convert: %v", err)
		}
		if res.Config.Admin.Address != "10.1.2.3:9091" {
			t.Errorf("admin address = %q, want 10.1.2.3:9091", res.Config.Admin.Address)
		}
	})

	// An unset stats address should still yield a loopback admin bind on the
	// health port, needing no insecure override.
	t.Run("unset stats address falls back to loopback health port", func(t *testing.T) {
		res, err := Convert(proxySidecar(t, func(c *config.Config) {
			c.Stats.StatsAddress = ""
		}), nil)
		if err != nil {
			t.Fatalf("Convert: %v", err)
		}
		if res.Config.Admin.Address != "127.0.0.1:9091" {
			t.Errorf("admin address = %q, want 127.0.0.1:9091", res.Config.Admin.Address)
		}
		if res.Config.InsecureOptions != nil {
			t.Error("loopback admin must not require allow_public_admin")
		}
	})

	// A malformed stats address must not be propagated into the admin bind:
	// Praxis parses admin.address as a SocketAddr and would reject it, so the
	// fallback keeps one clear failure mode instead of two.
	t.Run("malformed stats address falls back", func(t *testing.T) {
		res, err := Convert(proxySidecar(t, func(c *config.Config) {
			c.Stats.StatsAddress = "not-an-address"
		}), nil)
		if err != nil {
			t.Fatalf("Convert: %v", err)
		}
		if res.Config.Admin.Address != defaultAdminAddr {
			t.Errorf("admin address = %q, want the default %q",
				res.Config.Admin.Address, defaultAdminAddr)
		}
	})
}

// AdminPort must match the port AuthBridge's binaries hardcode for their
// health server, since that is the whole reason for choosing it: a readiness
// probe already pointed at 9091 keeps working against the generated proxy.
func TestAdminPort_MatchesAuthBridgeHealthPort(t *testing.T) {
	if AdminPort != 9091 {
		t.Errorf("AdminPort = %d, want 9091 (AuthBridge's /healthz + /readyz port)", AdminPort)
	}
}

func TestEndpointFromBackendURL(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "http://localhost:8001", want: "localhost:8001"},
		{in: "https://app:8443", want: "app:8443"},
		{in: "http://127.0.0.1:8001", want: "127.0.0.1:8001"},
		{in: "http://app", want: "app:80"},
		{in: "https://app", want: "app:443"},
		{in: "localhost:8001", want: "localhost:8001"},
		{in: "http://", wantErr: true},
	} {
		got, err := endpointFromBackendURL(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("endpointFromBackendURL(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("endpointFromBackendURL(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("endpointFromBackendURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeBindAddr(t *testing.T) {
	for in, want := range map[string]string{
		":8080":          "0.0.0.0:8080",
		"0.0.0.0:8080":   "0.0.0.0:8080",
		"127.0.0.1:8080": "127.0.0.1:8080",
		"localhost:8081": "localhost:8081",
	} {
		if got := normalizeBindAddr(in); got != want {
			t.Errorf("normalizeBindAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

// Filter entries must marshal flat — the filter's typed fields as siblings of
// `filter`, not nested under a `config:` key. Praxis rejects unknown fields on
// a filter entry, so a nested wrapper would fail to parse.
func TestFilter_MarshalsFlat(t *testing.T) {
	f := Filter{
		Type:   "router",
		Fields: []Field{{Key: "routes", Value: []Route{{PathPrefix: "/", Cluster: "c"}}}},
	}
	out, err := yaml.Marshal(f)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var probe map[string]any
	if err := yaml.Unmarshal(out, &probe); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, nested := probe["config"]; nested {
		t.Errorf("filter entry must not nest fields under 'config':\n%s", out)
	}
	if probe["filter"] != "router" {
		t.Errorf("filter key = %v, want router", probe["filter"])
	}
	if _, ok := probe["routes"]; !ok {
		t.Errorf("expected 'routes' as a sibling of 'filter':\n%s", out)
	}
}

// The rendered document must parse as YAML and round-trip to the same
// structure, so a malformed comment block can't silently corrupt it.
func TestRenderResult_ParsesAsYAML(t *testing.T) {
	res, err := Convert(proxySidecar(t, func(c *config.Config) {
		c.Pipeline.Inbound.Plugins = []config.PluginEntry{{Name: "jwt-validation"}}
	}), nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	out, err := RenderResult(res, "/tmp/praxis-config.yaml")
	if err != nil {
		t.Fatalf("RenderResult: %v", err)
	}
	var probe struct {
		Listeners []struct {
			Name    string `yaml:"name"`
			Address string `yaml:"address"`
		} `yaml:"listeners"`
		FilterChains []struct {
			Name string `yaml:"name"`
		} `yaml:"filter_chains"`
	}
	if err := yaml.Unmarshal(out, &probe); err != nil {
		t.Fatalf("generated YAML does not parse: %v\n%s", err, out)
	}
	if len(probe.Listeners) != len(res.Config.Listeners) {
		t.Errorf("round-tripped %d listeners, want %d", len(probe.Listeners), len(res.Config.Listeners))
	}
	// The unmapped account must survive into the file, not just the Result.
	if !strings.Contains(string(out), "jwt-validation") {
		t.Error("expected the unmapped plugin recorded in the rendered file")
	}
}

func TestKnownPlugins_IsSorted(t *testing.T) {
	got := KnownPlugins()
	if len(got) == 0 {
		t.Fatal("expected some known plugins")
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Errorf("KnownPlugins is not sorted at %d: %q > %q", i, got[i-1], got[i])
		}
	}
}

// TestGeneratedConfig_ValidatesWithPraxis runs the real Praxis binary against
// generated configs. This is the test that actually pins the goal — that
// `praxis -c <generated>` parses and accepts the output — since Praxis's
// validator enforces rules (filter ordering, cluster cross-references, field
// names, admin loopback) that no amount of Go-side assertion can stand in for.
//
// Skipped when no Praxis binary is available, so the suite still runs in CI
// without a Rust toolchain. Set PRAXIS_BIN to point at one explicitly.
func TestGeneratedConfig_ValidatesWithPraxis(t *testing.T) {
	bin := findPraxisBinary(t)
	if bin == "" {
		t.Skip("no praxis binary found; set PRAXIS_BIN to enable this test")
	}

	cases := []struct {
		name   string
		mutate func(*config.Config)
	}{
		{name: "both roles, no plugins", mutate: nil},
		{
			name: "reverse only",
			mutate: func(c *config.Config) {
				c.Listener.Roles = []string{config.RoleReverse}
			},
		},
		{
			name: "forward only",
			mutate: func(c *config.Config) {
				c.Listener.Roles = []string{config.RoleForward}
			},
		},
		{
			name: "full auth pipeline",
			mutate: func(c *config.Config) {
				c.Pipeline.Inbound.Plugins = []config.PluginEntry{{Name: "jwt-validation"}}
				c.Pipeline.Outbound.Plugins = []config.PluginEntry{
					{Name: "mcp-parser"}, {Name: "token-exchange"},
				}
			},
		},
		{
			name: "strict mTLS",
			mutate: func(c *config.Config) {
				c.MTLS = &config.MTLSConfig{Mode: config.MTLSModeStrict}
			},
		},
		{
			name: "permissive mTLS",
			mutate: func(c *config.Config) {
				c.MTLS = &config.MTLSConfig{Mode: config.MTLSModePermissive}
			},
		},
		{
			name: "public stats bind",
			mutate: func(c *config.Config) {
				c.Stats.StatsAddress = ":9093"
			},
		},
		{
			name: "loopback stats bind",
			mutate: func(c *config.Config) {
				c.Stats.StatsAddress = "127.0.0.1:9093"
			},
		},
		{
			name: "every known plugin",
			mutate: func(c *config.Config) {
				for _, n := range KnownPlugins() {
					c.Pipeline.Outbound.Plugins = append(
						c.Pipeline.Outbound.Plugins, config.PluginEntry{Name: n})
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				Mode: config.ModeProxySidecar,
				Listener: config.ListenerConfig{
					ReverseProxyBackend: "http://localhost:8001",
				},
			}
			if tc.mutate != nil {
				tc.mutate(cfg)
			}
			config.ApplyPreset(cfg)

			res, err := Convert(cfg, nil)
			if err != nil {
				t.Fatalf("Convert: %v", err)
			}
			out, err := RenderResult(res, "generated.yaml")
			if err != nil {
				t.Fatalf("RenderResult: %v", err)
			}
			path := filepath.Join(t.TempDir(), "praxis-config.yaml")
			if err := os.WriteFile(path, out, 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			cmd := exec.Command(bin, "-t", "-c", path)
			combined, err := cmd.CombinedOutput()
			if err != nil {
				t.Errorf("praxis rejected the generated config: %v\n--- praxis output ---\n%s\n--- config ---\n%s",
					err, combined, out)
			}
		})
	}
}

// findPraxisBinary locates a Praxis binary to validate against: PRAXIS_BIN
// first, then the usual cargo target directories under ~/src/praxis.
func findPraxisBinary(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("PRAXIS_BIN"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		t.Fatalf("PRAXIS_BIN=%q does not exist", p)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	for _, rel := range []string{"src/praxis/target/debug/praxis", "src/praxis/target/release/praxis"} {
		p := filepath.Join(home, rel)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// lastFilter returns the last filter of the given type in a chain.
func lastFilter(t *testing.T, chain FilterChain, typ string) Filter {
	t.Helper()
	for i := len(chain.Filters) - 1; i >= 0; i-- {
		if chain.Filters[i].Type == typ {
			return chain.Filters[i]
		}
	}
	t.Fatalf("no %q filter in chain %q", typ, chain.Name)
	return Filter{}
}

func containsSubstring(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.Contains(h, needle) {
			return true
		}
	}
	return false
}
