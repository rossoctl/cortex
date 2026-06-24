// Package main is the proxy-sidecar authbridge binary: HTTP forward
// proxy + reverse proxy, no Envoy / gRPC dependencies. By default it
// compiles in every registered plugin; individual plugins can be
// dropped at build time via `-tags exclude_plugin_<name>` (each lives
// in its own plugins_<name>.go file). jwt-validation and token-exchange
// are always compiled in.
//
// The `authbridge-lite` image is this same binary built with the
// parser/opa/token-broker plugins excluded, leaving only jwt-validation
// + token-exchange — it is a build variant, not a separate binary.
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
	"strings"
	"syscall"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/config"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/observe"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/reloader"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/session"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/sessionapi"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/shared"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/spiffe"
	authtls "github.com/kagenti/kagenti-extensions/authbridge/authlib/tls"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/tlsbridge"

	// Only HTTP listeners are compiled in: no extproc/extauthz
	// (no gRPC, no envoy types).
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/listener/forwardproxy"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/listener/reverseproxy"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/listener/skiphost"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/listener/transparentproxy"

	// Always-compiled plugins (the "lite" footprint). Every other plugin
	// is wired via its own plugins_<name>.go file gated by
	// `//go:build !exclude_plugin_<name>`, so it can be dropped at build
	// time (e.g. the authbridge-lite image excludes all but these two).
	_ "github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/jwtvalidation"
	// Named (not blank) so pluginUsesSPIFFEIdentity can reference the shared
	// SpiffeIdentity constant instead of duplicating the "spiffe" literal.
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/tokenexchange"
)

var logLevel = new(slog.LevelVar)

func initLogging() {
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		logLevel.Set(slog.LevelDebug)
	case "warn":
		logLevel.Set(slog.LevelWarn)
	case "error":
		logLevel.Set(slog.LevelError)
	default:
		logLevel.Set(slog.LevelInfo)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})))
}

func startSignalToggle() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGUSR1)
	go func() {
		for range sigCh {
			if logLevel.Level() == slog.LevelDebug {
				logLevel.Set(slog.LevelInfo)
				slog.Info("log level toggled to INFO (send SIGUSR1 to switch back to DEBUG)")
			} else {
				logLevel.Set(slog.LevelDebug)
				slog.Info("log level toggled to DEBUG (send SIGUSR1 to switch back to INFO)")
			}
		}
	}()
}

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
	return probe.Identity.Type == tokenexchange.SpiffeIdentity
}

func main() {
	configPath := flag.String("config", "", "path to config YAML file")
	flag.Parse()

	initLogging()
	startSignalToggle()

	if *configPath == "" {
		log.Fatal("--config is required")
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
		log.Fatalf("initial config load: %v", err)
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
		// CA is always the operator-mounted cert-manager Secret (tls.crt/tls.key
		// under ca_dir). EphemeralSource exists only for in-process tests.
		src, cerr := tlsbridge.NewFileSource(cfg.TLSBridge.CADir+"/tls.crt", cfg.TLSBridge.CADir+"/tls.key")
		if cerr != nil {
			log.Fatalf("tls-bridge CA init failed: %v", cerr)
		}
		var extra []byte
		if cfg.TLSBridge.UpstreamCABundle != "" {
			if extra, err = os.ReadFile(cfg.TLSBridge.UpstreamCABundle); err != nil {
				log.Fatalf("tls-bridge upstream_ca_bundle read failed: %v", err)
			}
		}
		up, uerr := tlsbridge.NewUpstreamClient(extra)
		if uerr != nil {
			log.Fatalf("tls-bridge upstream client failed: %v", uerr)
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

	// Proxy-sidecar: reverse proxy on the inbound path + forward proxy
	// on the outbound path.
	rpSrv, err := reverseproxy.NewServer(inboundH, sessions, cfg.Listener.ReverseProxyBackend, rpMTLS)
	if err != nil {
		log.Fatalf("creating reverse proxy: %v", err)
	}
	fpSrv, err := forwardproxy.NewServer(outboundH, sessions, fpMTLS)
	if err != nil {
		log.Fatalf("creating forward proxy: %v", err)
	}
	// SkipHosts: outbound destinations that bypass the pipeline AND
	// session recording entirely. See ListenerConfig.SkipHosts for the
	// motivating case (chatty observability sidecars evicting the
	// inbound A2A user intent from the session FIFO).
	skipHosts, err := skiphost.New(cfg.Listener.SkipHosts)
	if err != nil {
		log.Fatalf("listener.skip_hosts: %v", err)
	}
	fpSrv.SkipHosts = skipHosts
	fpSrv.TLSBridge = bridge
	sharedStore := shared.New()
	defer sharedStore.Close() // stop the TTL janitor on normal main return
	rpSrv.Shared = sharedStore
	fpSrv.Shared = sharedStore
	httpServers = append(httpServers, startReverseProxyServer("reverse-proxy", rpSrv, cfg.Listener.ReverseProxyAddr))
	httpServers = append(httpServers, startHTTPServer("forward-proxy", fpSrv.Handler(), cfg.Listener.ForwardProxyAddr))

	// Outbound transparent listener (enforce-redirect mode). It shares the
	// forward proxy's outbound pipeline via HandleTransparentConn, so explicit
	// HTTP_PROXY egress and iptables-REDIRECTed bypass egress are gated and
	// tunnelled identically. Closed explicitly on shutdown (not an *http.Server).
	transparentLn := startTransparentProxy(fpSrv, cfg.Listener.TransparentProxyAddr)

	_ = mtlsMetrics // TODO Phase 2: surface metrics through /stats

	statsProvider := func() *auth.Stats {
		sources := plugins.CollectStats(inboundH.Load())
		sources = append(sources, plugins.CollectStats(outboundH.Load())...)
		return auth.MergeStats(sources...)
	}
	statSrv := startStatServer(cfg, rld.ConfigProvider(), statsProvider, rld.Handler())

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

	slog.Info("authbridge-proxy starting", "mode", cfg.Mode, "logLevel", logLevel.Level().String())

	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
			if name := inboundH.NotReadyPlugin(); name != "" {
				http.Error(w, "inbound plugin not ready: "+name, http.StatusServiceUnavailable)
				return
			}
			if name := outboundH.NotReadyPlugin(); name != "" {
				http.Error(w, "outbound plugin not ready: "+name, http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
		})
		slog.Info("health server listening", "addr", ":9091")
		if err := http.ListenAndServe(":9091", mux); err != nil {
			slog.Warn("health server failed", "error", err)
		}
	}()

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
	if sessionAPISrv != nil {
		sessionAPISrv.Shutdown(shutdownCtx)
	}

	outboundPipeline.Stop(shutdownCtx)
	inboundPipeline.Stop(shutdownCtx)

	if sessions != nil {
		sessions.Close()
	}
}

func startHTTPServer(name string, handler http.Handler, addr string) *http.Server {
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		slog.Info("HTTP server listening", "name", name, "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("%s serve: %v", name, err)
		}
	}()
	return srv
}

// startReverseProxyServer mirrors startHTTPServer but uses the
// reverseproxy.Server's Listen() method so the byte-peek TLS-sniffing
// listener is wired in when mTLS is enabled. With mTLS off, Listen
// returns a plain net.Listen and behavior matches startHTTPServer.
//
// Logged "mtls" attribute makes the listener mode visible at startup;
// operators expecting a separate :8443 port for TLS get a clear hint
// that this is the same :8080 with byte-peek detection.
func startReverseProxyServer(name string, rp *reverseproxy.Server, addr string) *http.Server {
	srv := &http.Server{
		Addr:              addr,
		Handler:           rp.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	listener, err := rp.Listen(addr)
	if err != nil {
		log.Fatalf("%s listen: %v", name, err)
	}
	go func() {
		slog.Info("HTTP server listening", "name", name, "addr", addr, "mtls", rp.MTLSEnabled())
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Fatalf("%s serve: %v", name, err)
		}
	}()
	return srv
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

func startStatServer(cfg *config.Config, cfgProvider observe.ConfigProvider, statsProvider observe.StatsProvider, reloadStatus http.Handler) *observe.StatServer {
	srv := observe.NewStatServer(cfg.Stats.StatsAddress, cfgProvider, statsProvider,
		observe.WithReloadStatus(reloadStatus))
	go func() {
		slog.Info("stat server listening", "addr", cfg.Stats.StatsAddress)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("stat server: %v", err)
		}
	}()
	return srv
}
