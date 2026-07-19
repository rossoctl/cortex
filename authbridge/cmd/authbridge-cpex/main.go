//go:build cpex

// Package main is the CPEX-enabled authbridge binary: identical to
// authbridge-proxy (HTTP forward + reverse proxies, full rossoctl
// plugin set) plus the `cpex` plugin which routes hooks through the
// CPEX (Context Plugin Execution) framework — including the APL DSL
// and any pre-built CPEX policy plugins (Cedar, PII scanner, audit
// logger, etc.).
//
// This binary requires `-tags cpex` and links libcpex_ffi via cgo.
// The build constraint at the top of this file ensures a no-tag
// build fails fast rather than silently producing an authbridge-proxy
// duplicate.
//
// For envoy-sidecar mode use authbridge-envoy; for a no-cgo, pure-Go
// build use authbridge-proxy. The body of main() below is duplicated
// from authbridge-proxy/main.go pending an authlib-side `Run()`
// extraction — see this binary's README for the extraction proposal.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rossoctl/rossocortex/authbridge/authlib/auth"
	"github.com/rossoctl/rossocortex/authbridge/authlib/config"
	"github.com/rossoctl/rossocortex/authbridge/authlib/observe"
	"github.com/rossoctl/rossocortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/rossocortex/authbridge/authlib/plugins"
	"github.com/rossoctl/rossocortex/authbridge/authlib/reloader"
	"github.com/rossoctl/rossocortex/authbridge/authlib/session"
	"github.com/rossoctl/rossocortex/authbridge/authlib/sessionapi"
	"github.com/rossoctl/rossocortex/authbridge/authlib/shared"
	"github.com/rossoctl/rossocortex/authbridge/authlib/spiffe"
	authtls "github.com/rossoctl/rossocortex/authbridge/authlib/tls"

	"github.com/rossoctl/rossocortex/authbridge/authlib/listener/forwardproxy"
	"github.com/rossoctl/rossocortex/authbridge/authlib/listener/reverseproxy"

	// Plugins — same set as authbridge-proxy, plus the cpex plugin
	// which lives behind //go:build cpex. The cpex import only fires
	// in this binary's build; pure-Go binaries (authbridge-proxy,
	// authbridge-envoy, authbridge-lite) don't import it.
	_ "github.com/rossoctl/rossocortex/authbridge/authlib/plugins/a2aparser"
	_ "github.com/rossoctl/rossocortex/authbridge/authlib/plugins/cpex"
	_ "github.com/rossoctl/rossocortex/authbridge/authlib/plugins/ibac"
	_ "github.com/rossoctl/rossocortex/authbridge/authlib/plugins/inferenceparser"
	_ "github.com/rossoctl/rossocortex/authbridge/authlib/plugins/jwtvalidation"
	_ "github.com/rossoctl/rossocortex/authbridge/authlib/plugins/mcpparser"
	_ "github.com/rossoctl/rossocortex/authbridge/authlib/plugins/sparc"
	_ "github.com/rossoctl/rossocortex/authbridge/authlib/plugins/tokenbroker"
	_ "github.com/rossoctl/rossocortex/authbridge/authlib/plugins/tokenexchange"
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

func main() {
	configPath := flag.String("config", "", "path to config YAML file")
	flag.Parse()

	initLogging()
	startSignalToggle()

	if *configPath == "" {
		log.Fatal("--config is required")
	}

	bootCfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("initial config load: %v", err)
	}
	var provider *spiffe.Provider
	if bootCfg.SPIFFE != nil {
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
	}

	buildPipelines := func() (*pipeline.Pipeline, *pipeline.Pipeline, *config.Config, error) {
		c, err := config.Load(*configPath)
		if err != nil {
			return nil, nil, nil, err
		}
		if c.Mode != "" && c.Mode != config.ModeProxySidecar {
			return nil, nil, nil, fmt.Errorf(
				"authbridge-cpex supports only mode=%q (got %q); use cmd/authbridge-envoy for envoy-sidecar mode",
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
		maxEvents := 100
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
		rpMTLS = &reverseproxy.MTLSOptions{Source: src, Strict: strict, Metrics: mtlsMetrics}
		if strict {
			fpMTLS = &forwardproxy.MTLSOptions{Source: src, Metrics: mtlsMetrics}
		}
		slog.Info("mTLS enabled", "mode", cfg.MTLS.ResolvedMode())
	} else {
		slog.Info("mTLS disabled (no mtls block in config)")
	}

	rpSrv, err := reverseproxy.NewServer(inboundH, sessions, cfg.Listener.ReverseProxyBackend, rpMTLS)
	if err != nil {
		log.Fatalf("creating reverse proxy: %v", err)
	}
	fpSrv, err := forwardproxy.NewServer(outboundH, sessions, fpMTLS)
	if err != nil {
		log.Fatalf("creating forward proxy: %v", err)
	}
	sharedStore := shared.New()
	defer sharedStore.Close()
	rpSrv.Shared = sharedStore
	fpSrv.Shared = sharedStore
	httpServers = append(httpServers, startReverseProxyServer("reverse-proxy", rpSrv, cfg.Listener.ReverseProxyAddr))
	httpServers = append(httpServers, startHTTPServer("forward-proxy", fpSrv.Handler(), cfg.Listener.ForwardProxyAddr))
	_ = mtlsMetrics

	statsProvider := func() *auth.Stats {
		sources := plugins.CollectStats(inboundH.Load())
		sources = append(sources, plugins.CollectStats(outboundH.Load())...)
		return auth.MergeStats(sources...)
	}
	statSrv := startStatServer(cfg, rld.ConfigProvider(), statsProvider, rld.Handler())

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

	slog.Info("authbridge-cpex starting", "mode", cfg.Mode, "logLevel", logLevel.Level().String())

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
