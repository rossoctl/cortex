// Package main is the proxy-sidecar authbridge binary: HTTP forward
// proxy + reverse proxy, no Envoy / gRPC dependencies. By default it
// compiles in every registered plugin. Every plugin — including
// jwt-validation and token-exchange — has its own plugins_<name>.go
// file gated by `//go:build !exclude_plugin_<name>`, so any subset can
// be dropped at build time via `-tags exclude_plugin_<name>`. main.go
// imports no plugin package directly.
//
// The `authbridge-lite` image is this same binary built with everything
// except jwt-validation + token-exchange excluded — it is a build
// variant, not a separate binary.
//
// Mode is hardcoded to proxy-sidecar; YAML configs that specify a
// different mode are rejected at boot. For envoy-sidecar mode, use
// cmd/authbridge-envoy.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/auth"
	"github.com/rossoctl/cortex/authbridge/authlib/config"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins"
	"github.com/rossoctl/cortex/authbridge/authlib/reloader"
	"github.com/rossoctl/cortex/authbridge/authlib/runtimeutil"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
	"github.com/rossoctl/cortex/authbridge/authlib/sessionapi"
	"github.com/rossoctl/cortex/authbridge/authlib/shared"
	"github.com/rossoctl/cortex/authbridge/authlib/spiffe"
	authtls "github.com/rossoctl/cortex/authbridge/authlib/tls"
	"github.com/rossoctl/cortex/authbridge/authlib/tlsbridge"

	// Only HTTP listeners are compiled in: no extproc/extauthz
	// (no gRPC, no envoy types).
	"github.com/rossoctl/cortex/authbridge/authlib/listener/forwardproxy"
	"github.com/rossoctl/cortex/authbridge/authlib/listener/reverseproxy"
	"github.com/rossoctl/cortex/authbridge/authlib/listener/skiphost"
	"github.com/rossoctl/cortex/authbridge/authlib/listener/transparentproxy"
	// Plugins are wired via per-plugin plugins_<name>.go files, each gated
	// by `//go:build !exclude_plugin_<name>`. main.go imports no plugin
	// package directly, so every plugin can be dropped at build time. The
	// authbridge-lite image excludes all but jwt-validation + token-exchange.
)

// version is the authbridge-proxy build version, overridden at release time
// via -ldflags "-X main.version=<tag>". Defaults to "dev" for local builds.
var version = "dev"

// demoMode is set by --demo. It suppresses listeners that only make sense with
// iptables enforce-redirect: the demo uses cooperative HTTPS_PROXY, so nothing
// is ever REDIRECTed to the transparent listener and opening it would just be
// an idle port. The forward-role preset defaults transparent_proxy_addr to
// :8082, and config can't unset it (the preset refills an empty value), so this
// gate is the only way to keep the demo to the listeners it actually uses.
var demoMode bool

// spiffeProviderNeeded reports whether any configured feature actually consumes
// the SPIFFE Provider: top-level mTLS (needs the X509Source on both listeners)
// or a plugin whose identity is spiffe-based (needs the JWT-SVID source — today
// only token-exchange, gated on identity.type=spiffe). When nothing consumes
// it, the provider — and its blocking SPIRE Workload API dial in NewProvider —
// is skipped, so the binary boots even on clusters without SPIRE.
func spiffeProviderNeeded(c *config.Config) bool {
	if c.MTLS != nil {
		return true
	}
	for _, p := range c.Pipeline.Inbound.Plugins {
		if pluginUsesSPIFFEIdentity(p) {
			return true
		}
	}
	for _, p := range c.Pipeline.Outbound.Plugins {
		if pluginUsesSPIFFEIdentity(p) {
			return true
		}
	}
	return false
}

// spiffeIdentityType is the `identity.type` config value that selects the
// SPIFFE identity scheme. It is a shared config convention (token-exchange is
// the only consumer today); kept as a local constant so main.go stays
// decoupled from any specific plugin package — every plugin is build-tag
// excludable via plugins_<name>.go.
const spiffeIdentityType = "spiffe"

// pluginUsesSPIFFEIdentity reports whether a plugin's config selects the spiffe
// identity scheme (identity.type=spiffe) — the only plugin-level consumer of
// the Provider today (token-exchange). The `identity` block is a shared
// convention; a new SPIFFE-consuming plugin must either follow it or extend
// this predicate.
func pluginUsesSPIFFEIdentity(p config.PluginEntry) bool {
	if len(p.Config) == 0 {
		return false
	}
	var probe struct {
		Identity struct {
			Type string `json:"type"`
		} `json:"identity"`
	}
	if err := json.Unmarshal(p.Config, &probe); err != nil {
		// Unparseable here just means the plugin's own typed decode will fail
		// later with a precise error; don't force the provider on for it.
		return false
	}
	return probe.Identity.Type == spiffeIdentityType
}

func main() {
	configPath := flag.String("config", "", "path to config YAML file")
	showVersion := flag.Bool("version", false, "print version and exit")
	demo := flag.Bool("demo", false,
		"run a built-in local demo (forward-only TLS bridge + protocol parsers) that decrypts and parses an agent's egress; no --config, cluster, Keycloak, or SPIRE needed")
	caDir := flag.String("ca-dir", "",
		"CA directory for --demo (auto-generated); defaults to ./"+demoCADirDefault)
	flag.Parse()

	if *showVersion {
		fmt.Println("authbridge-proxy", version)
		return
	}

	runtimeutil.InitLogging("authbridge-proxy")
	runtimeutil.StartSignalToggle()

	if *demo {
		demoMode = true
		if *configPath != "" {
			log.Fatal("--demo and --config are mutually exclusive")
		}
		dir := *caDir
		if dir == "" {
			dir = demoCADirDefault // relative to cwd — no absolute path baked in
		}
		abs, aerr := filepath.Abs(dir)
		if aerr != nil {
			log.Fatalf("--demo: resolving --ca-dir %q: %v", dir, aerr)
		}
		// Write the built-in config next to the CA and drive the normal
		// file-based load + hot-reload path — so editing the file reloads live.
		p, werr := writeDemoConfig(abs)
		if werr != nil {
			log.Fatalf("--demo: %v", werr)
		}
		*configPath = p
		slog.Info("demo mode — wrote built-in config next to the CA; edit it to hot-reload",
			"config", p, "ca_dir", abs)
	} else if *caDir != "" {
		log.Fatal("--ca-dir only applies with --demo")
	}

	if *configPath == "" {
		log.Fatal("--config is required (or use --demo for the local demo)")
	}

	// Build the SPIFFE Provider when the spiffe block is configured. The
	// Provider drives both mTLS (via X509Source) and token-exchange's
	// spiffe identity (via JWTSource). Construction blocks until the first
	// X.509-SVID arrives (cold-start gate); kubelet restarts on failure.
	//
	// We need cfg first to read the spiffe block, so do a one-shot Load
	// before buildPipelines runs (buildPipelines re-Loads internally for
	// hot-reload). The Provider is captured by buildPipelines via closure
	// so reload-time pipeline rebuilds inject the same Provider into
	// freshly constructed plugin instances.
	bootCfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("failed to load config %q: %v", *configPath, err)
	}
	// Build the SPIFFE Provider only when something actually consumes it —
	// top-level mTLS (X509Source for the listeners) or a plugin whose identity
	// is spiffe-based (JWT-SVID for token-exchange). The platform's base config
	// ships an empty `spiffe: {}` for every agent, and NewProvider blocks until
	// the SPIRE Workload API returns the first SVID; constructing it on mere
	// presence of the block would hang any agent on a cluster without SPIRE —
	// e.g. a proxy-sidecar agent that only runs the TLS bridge, which mints
	// leaves from a cert-manager CA and never touches an SVID. Need-driven
	// construction keeps such agents decoupled from SPIRE. See spiffeProviderNeeded.
	var provider *spiffe.Provider
	if bootCfg.SPIFFE != nil && spiffeProviderNeeded(bootCfg) {
		mirrorFiles := true
		if bootCfg.SPIFFE.MirrorFiles != nil {
			mirrorFiles = *bootCfg.SPIFFE.MirrorFiles
		}
		provider, err = spiffe.NewProvider(context.Background(), spiffe.ProviderConfig{
			SocketPath:  bootCfg.SPIFFE.Socket,
			MirrorFiles: mirrorFiles,
			MirrorDir:   bootCfg.SPIFFE.MirrorDir,
		})
		if err != nil {
			log.Fatalf("spiffe provider: %v", err)
		}
		defer provider.Close()
	} else if bootCfg.SPIFFE != nil {
		slog.Info("spiffe block present but unused (no mTLS, no spiffe-identity plugin) — " +
			"skipping SPIRE provider; no Workload API connection will be attempted")
	}

	// This binary is hardcoded to proxy-sidecar. Rejecting other modes
	// early gives operators a clear boot-time error instead of silently
	// misbehaving (e.g., YAML says envoy-sidecar but binary can't
	// serve ext_proc).
	buildPipelines := func() (*pipeline.Pipeline, *pipeline.Pipeline, *config.Config, error) {
		c, err := config.Load(*configPath)
		if err != nil {
			return nil, nil, nil, err
		}
		if c.Mode != "" && c.Mode != config.ModeProxySidecar {
			return nil, nil, nil, fmt.Errorf(
				"authbridge-proxy supports only mode=%q (got %q); use cmd/authbridge-envoy for envoy-sidecar mode",
				config.ModeProxySidecar, c.Mode)
		}
		c.Mode = config.ModeProxySidecar
		config.ApplyPreset(c)
		if err := config.Validate(c); err != nil {
			return nil, nil, nil, err
		}
		config.WarnEmptyPipelines(c, slog.Default())
		in, err := plugins.BuildWithSPIFFE(c.Pipeline.Inbound.Plugins, provider)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("inbound: %w", err)
		}
		out, err := plugins.BuildWithSPIFFE(c.Pipeline.Outbound.Plugins, provider)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("outbound: %w", err)
		}
		return in, out, c, nil
	}

	inboundPipeline, outboundPipeline, cfg, err := buildPipelines()
	if err != nil {
		log.Fatalf("initial pipeline build: %v", err)
	}

	initCtx, initCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer initCancel()
	if err := inboundPipeline.Start(initCtx); err != nil {
		log.Fatalf("inbound pipeline Start: %v", err)
	}
	if err := outboundPipeline.Start(initCtx); err != nil {
		log.Fatalf("outbound pipeline Start: %v", err)
	}

	inboundH := pipeline.NewHolder(inboundPipeline)
	outboundH := pipeline.NewHolder(outboundPipeline)

	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()
	rld := reloader.New(*configPath, inboundH, outboundH, buildPipelines, cfg)
	if err := rld.Start(ctx); err != nil {
		log.Fatalf("reloader: %v", err)
	}

	var sessions *session.Store
	if cfg.Session.SessionEnabled() {
		ttl := 30 * time.Minute
		if cfg.Session.TTL != "" {
			if d, err := time.ParseDuration(cfg.Session.TTL); err == nil {
				ttl = d
			} else {
				slog.Warn("invalid session.ttl, using default", "value", cfg.Session.TTL, "error", err)
			}
		}
		maxEvents := 500 // raised from 100: recording every message (incl. no-plugin-activity) ~doubles volume
		if cfg.Session.MaxEvents > 0 {
			maxEvents = cfg.Session.MaxEvents
		}
		maxSessions := 100
		if cfg.Session.MaxSessions > 0 {
			maxSessions = cfg.Session.MaxSessions
		}
		sessions = session.New(ttl, maxEvents, maxSessions)
		slog.Info("session tracking enabled", "ttl", ttl, "maxEvents", maxEvents, "maxSessions", maxSessions)
	} else {
		slog.Info("session tracking disabled")
	}

	var httpServers []*http.Server

	// mTLS: a single global mode applies symmetrically to both the
	// inbound (reverse-proxy) and outbound (forward-proxy) listeners.
	// When cfg.MTLS is nil, today's plaintext behavior is preserved
	// throughout. The X509Source is shared by both listeners so they
	// see the same SVID + trust bundle even across spiffe-helper
	// rotations.
	var (
		rpMTLS      *reverseproxy.MTLSOptions
		fpMTLS      *forwardproxy.MTLSOptions
		mtlsMetrics *authtls.Metrics
	)
	if cfg.MTLS != nil {
		if provider == nil {
			log.Fatal("mtls requires the spiffe block to be configured")
		}
		strict := cfg.MTLS.ResolvedMode() == config.MTLSModeStrict
		src := provider.X509Source()
		mtlsMetrics = authtls.NewMetrics()
		// Inbound (reverse proxy): permissive peeks-and-routes, strict
		// rejects non-TLS. Strict bool toggles between the two.
		rpMTLS = &reverseproxy.MTLSOptions{Source: src, Strict: strict, Metrics: mtlsMetrics}
		// Outbound (forward proxy): only attempt TLS in strict mode.
		// Permissive is plaintext outbound — matches envoy-sidecar's
		// permissive (Envoy has no native primitive for "try TLS, fall
		// back on handshake failure", and Istio's PeerAuthentication
		// permissive is inbound-only). A permissive caller can no
		// longer reach a strict peer regardless of mode; mixed-mode
		// deployments need both ends compatible. See authbridge/CLAUDE.md
		// "Top-level mtls: configuration".
		if strict {
			fpMTLS = &forwardproxy.MTLSOptions{Source: src, Metrics: mtlsMetrics}
		}
		slog.Info("mTLS enabled", "mode", cfg.MTLS.ResolvedMode())
	} else {
		slog.Info("mTLS disabled (no mtls block in config)")
	}

	// TLS bridge: when enabled, the forward proxy terminates agent outbound
	// TLS so the outbound pipeline sees decrypted HTTPS. Constructed
	// here and set on fpSrv below (mirroring fpSrv.SkipHosts / fpSrv.Shared).
	// A nil *Engine leaves today's blind-tunnel behavior intact.
	var bridge *tlsbridge.Engine
	if cfg.TLSBridge != nil && cfg.TLSBridge.Mode == "enabled" {
		// CA is normally the operator-mounted cert-manager Secret (tls.crt/tls.key
		// under ca_dir). For standalone/demo use, generate_ca mints and persists a
		// self-signed CA into ca_dir when those files are absent (default false, so
		// in-cluster a missing Secret still fails loudly).
		src, generated, cerr := tlsbridge.EnsureFileSource(cfg.TLSBridge.CADir, cfg.TLSBridge.GenerateCA)
		if cerr != nil {
			log.Fatalf("tls-bridge CA init failed: %v", cerr)
		}
		if generated {
			slog.Warn("tls-bridge: generated self-signed CA (generate_ca=true; standalone/demo)",
				"ca_dir", cfg.TLSBridge.CADir,
				"hint", "clients must trust it, e.g. NODE_EXTRA_CA_CERTS="+cfg.TLSBridge.CADir+"/ca.crt")
		}
		var extra []byte
		if cfg.TLSBridge.UpstreamCABundle != "" {
			if extra, err = os.ReadFile(cfg.TLSBridge.UpstreamCABundle); err != nil {
				log.Fatalf("tls-bridge upstream_ca_bundle read failed: %v", err)
			}
		}
		up, uerr := tlsbridge.NewUpstreamClient(extra, cfg.TLSBridge.UpstreamInsecure)
		if uerr != nil {
			log.Fatalf("tls-bridge upstream client failed: %v", uerr)
		}
		if cfg.TLSBridge.UpstreamInsecure {
			log.Printf("tls-bridge: upstream_insecure=true — re-origination does NOT verify the upstream TLS cert")
		}
		minter := tlsbridge.NewMinter(src, tlsbridge.MinterOpts{})
		var ports map[int]bool // nil => NewDecision defaults to {443, 8443}
		if len(cfg.TLSBridge.Ports) > 0 {
			ports = make(map[int]bool, len(cfg.TLSBridge.Ports))
			for _, p := range cfg.TLSBridge.Ports {
				ports[p] = true
			}
		}
		bridge = &tlsbridge.Engine{
			Decision: tlsbridge.NewDecision(tlsbridge.DecisionOpts{
				Ports: ports, SkipHosts: cfg.TLSBridge.PassthroughHosts,
			}),
			Term:     tlsbridge.NewTerminator(minter),
			Skip:     tlsbridge.NewSkipSet(),
			Upstream: up,
			CAPEM:    src.CACertPEM(),
		}
		slog.Info("tls-bridge enabled", "ca_dir", cfg.TLSBridge.CADir)
	}

	// Proxy-sidecar: reverse proxy (inbound) and/or forward proxy (outbound),
	// selected by listener.roles (empty => both). sharedStore is created up
	// front so whichever proxies run share one session store.
	roles := cfg.Listener.ActiveRoles()
	sharedStore := shared.New()
	defer sharedStore.Close() // stop the TTL janitor on normal main return

	if roles[config.RoleReverse] {
		rpSrv, rerr := reverseproxy.NewServer(inboundH, sessions, cfg.Listener.ReverseProxyBackend, rpMTLS)
		if rerr != nil {
			log.Fatalf("creating reverse proxy: %v", rerr)
		}
		rpSrv.Shared = sharedStore
		rpHTTP, rerr := runtimeutil.StartReverseProxyServer("reverse-proxy", rpSrv, cfg.Listener.ReverseProxyAddr)
		if rerr != nil {
			log.Fatalf("reverse-proxy listen: %v", rerr)
		}
		httpServers = append(httpServers, rpHTTP)
	}

	// The transparent (enforce-redirect) listener rides with the forward proxy;
	// declared here so shutdown can close it whether or not the forward role ran.
	var transparentLn *net.TCPListener
	if roles[config.RoleForward] {
		fpSrv, ferr := forwardproxy.NewServer(outboundH, sessions, fpMTLS)
		if ferr != nil {
			log.Fatalf("creating forward proxy: %v", ferr)
		}
		// SkipHosts: outbound destinations that bypass the pipeline AND
		// session recording entirely. See ListenerConfig.SkipHosts for the
		// motivating case (chatty observability sidecars evicting the
		// inbound A2A user intent from the session FIFO).
		skipHosts, serr := skiphost.New(cfg.Listener.SkipHosts)
		if serr != nil {
			log.Fatalf("listener.skip_hosts: %v", serr)
		}
		fpSrv.SkipHosts = skipHosts
		fpSrv.TLSBridge = bridge
		fpSrv.Shared = sharedStore
		fpHTTP, herr := runtimeutil.StartHTTPServer("forward-proxy", fpSrv.Handler(), cfg.Listener.ForwardProxyAddr)
		if herr != nil {
			log.Fatalf("forward-proxy listen: %v", herr)
		}
		httpServers = append(httpServers, fpHTTP)

		// Outbound transparent listener (enforce-redirect mode). It shares the
		// forward proxy's outbound pipeline via HandleTransparentConn, so explicit
		// HTTP_PROXY egress and iptables-REDIRECTed bypass egress are gated and
		// tunnelled identically. Closed explicitly on shutdown (not an *http.Server).
		// Skipped in --demo: no iptables there, so nothing is ever REDIRECTed to it.
		if !demoMode {
			transparentLn = startTransparentProxy(fpSrv, cfg.Listener.TransparentProxyAddr)
		}
	}

	_ = mtlsMetrics // TODO Phase 2: surface metrics through /stats

	statsProvider := func() *auth.Stats {
		sources := plugins.CollectStats(inboundH.Load())
		sources = append(sources, plugins.CollectStats(outboundH.Load())...)
		return auth.MergeStats(sources...)
	}
	statSrv, statErr := runtimeutil.StartStatServer(cfg, rld.ConfigProvider(), statsProvider, rld.Handler(), cfg.Stats.StatsAddress)
	if statErr != nil {
		log.Fatalf("stat server listen: %v", statErr)
	}

	// Warm the plugin catalog at boot so any factory that violates the
	// constructor contract surfaces here rather than on the first
	// /v1/plugins request.
	plugins.WarmCatalog()

	var sessionAPISrv *sessionapi.Server
	if cfg.Listener.SessionAPIAddr != "" && sessions != nil {
		sessionAPISrv = sessionapi.New(
			cfg.Listener.SessionAPIAddr,
			sessions,
			sessionapi.WithPipelines(inboundH, outboundH),
			sessionapi.WithCatalog(sessionapi.PluginsCatalog),
		)
		go func() {
			slog.Warn("session API listening — UNAUTHENTICATED; contains raw user content; never expose via ingress",
				"addr", cfg.Listener.SessionAPIAddr)
			if err := sessionAPISrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("session API: %v", err)
			}
		}()
	}

	slog.Info("authbridge-proxy starting", "version", version, "mode", cfg.Mode, "logLevel", runtimeutil.LogLevel().String())

	healthSrv, healthErr := runtimeutil.StartHealthServer(inboundH, outboundH, ":9091")
	if healthErr != nil {
		log.Fatalf("health server listen: %v", healthErr)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	slog.Info("shutting down", "signal", sig)

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	for _, srv := range httpServers {
		srv.Shutdown(shutdownCtx)
	}
	if transparentLn != nil {
		_ = transparentLn.Close()
	}
	statSrv.Shutdown(shutdownCtx)
	healthSrv.Shutdown(shutdownCtx)
	if sessionAPISrv != nil {
		sessionAPISrv.Shutdown(shutdownCtx)
	}

	outboundPipeline.Stop(shutdownCtx)
	inboundPipeline.Stop(shutdownCtx)

	if sessions != nil {
		sessions.Close()
	}
}

// startTransparentProxy binds the outbound transparent listener and serves it
// in a goroutine, dispatching each REDIRECTed connection through the forward
// proxy's outbound pipeline. Returns the listener (for shutdown), or nil when
// addr is empty (transparent capture disabled). Bind failures are fatal —
// enforce-redirect iptables would otherwise REDIRECT to a dead port and break
// all egress silently.
func startTransparentProxy(fp *forwardproxy.Server, addr string) *net.TCPListener {
	if addr == "" {
		return nil
	}
	la, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		log.Fatalf("resolve transparent-proxy addr %q: %v", addr, err)
	}
	ln, err := net.ListenTCP("tcp", la)
	if err != nil {
		log.Fatalf("transparent-proxy listen on %q: %v", addr, err)
	}
	srv := transparentproxy.NewServer(fp.HandleTransparentConn)
	go func() {
		slog.Info("transparent proxy listening", "addr", addr)
		if err := srv.Serve(ln); err != nil {
			log.Fatalf("transparent-proxy serve: %v", err)
		}
	}()
	return ln
}
