// Command authbridge is a unified auth proxy supporting three deployment modes:
// envoy-sidecar (ext_proc), waypoint (ext_authz + forward proxy), and
// proxy-sidecar (reverse proxy + forward proxy).
package main

import (
	"context"
	"errors"
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

	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/config"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/observe"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins"
	// Bundled plugins register themselves via init() on import. These
	// two moved from flat files in plugins/ to their own subpackages
	// (they own private subtrees for validation / exchange / cache /
	// spiffe); we opt into them here. External plugins follow the
	// same pattern — drop one line in here (or a plugins_extra.go) to
	// include them in this binary's build.
	_ "github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/jwtvalidation"
	_ "github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/tokenexchange"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/reloader"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/session"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/sessionapi"
	"github.com/kagenti/kagenti-extensions/authbridge/cmd/authbridge/listener/extauthz"
	"github.com/kagenti/kagenti-extensions/authbridge/cmd/authbridge/listener/extproc"
	"github.com/kagenti/kagenti-extensions/authbridge/cmd/authbridge/listener/forwardproxy"
	"github.com/kagenti/kagenti-extensions/authbridge/cmd/authbridge/listener/reverseproxy"
)

// logLevel is the dynamic log level, togglable at runtime via SIGUSR1.
var logLevel = new(slog.LevelVar)

func initLogging() {
	// LOG_LEVEL env var sets the initial level: debug, info, warn, error.
	// Default: info. Override at runtime with SIGUSR1 (toggles debug/info).
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
	// SIGUSR1 toggles between info and debug at runtime, regardless of
	// the initial LOG_LEVEL (warn/error are treated as "not debug").
	// Usage: kubectl exec <pod> -c authbridge-proxy -- kill -USR1 1
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
	mode := flag.String("mode", "", "deployment mode: envoy-sidecar, waypoint, proxy-sidecar")
	configPath := flag.String("config", "", "path to config YAML file")
	flag.Parse()

	initLogging()
	startSignalToggle()

	if *configPath == "" {
		log.Fatal("--config is required")
	}

	// buildPipelines loads the config from *configPath, applies mode
	// override + presets, validates, and builds the plugin pipelines.
	// Runs once at startup and again on every reload — the reloader
	// holds this closure so both paths share exactly the same sequence.
	// Returns a descriptive error on any step's failure so /reload/status
	// surfaces an operator-readable message.
	buildPipelines := func() (*pipeline.Pipeline, *pipeline.Pipeline, *config.Config, error) {
		c, err := config.Load(*configPath)
		if err != nil {
			return nil, nil, nil, err
		}
		if *mode != "" {
			c.Mode = *mode // flag overrides YAML
		}
		config.ApplyPreset(c)
		if err := config.Validate(c); err != nil {
			return nil, nil, nil, err
		}
		in, err := plugins.BuildChain(plugins.ChainInbound, c.Pipeline.Inbound.Plugins)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("inbound: %w", err)
		}
		out, err := plugins.BuildChain(plugins.ChainOutbound, c.Pipeline.Outbound.Plugins)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("outbound: %w", err)
		}
		if c.Mode == config.ModeWaypoint && (in.NeedsBody() || out.NeedsBody()) {
			return nil, nil, nil, errors.New("waypoint mode does not support plugins that require body access (ext_authz limitation)")
		}
		return in, out, c, nil
	}

	inboundPipeline, outboundPipeline, cfg, err := buildPipelines()
	if err != nil {
		log.Fatalf("initial pipeline build: %v", err)
	}

	// Invoke Init on any plugin implementing pipeline.Initializer. Done
	// before listeners start so a plugin that fails to initialize takes
	// down the process cleanly rather than serving traffic in a broken
	// state. Init may do network I/O (register metrics, load a model,
	// fetch a remote config); we give it the process context so an
	// abort during init (Ctrl-C) cancels cooperatively.
	initCtx, initCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer initCancel()
	if err := inboundPipeline.Start(initCtx); err != nil {
		log.Fatalf("inbound pipeline Start: %v", err)
	}
	if err := outboundPipeline.Start(initCtx); err != nil {
		log.Fatalf("outbound pipeline Start: %v", err)
	}

	// Wrap pipelines in Holders. Listeners reference the Holder rather
	// than the *Pipeline directly so the reloader can swap the bound
	// pipeline under a running listener without pod restart.
	inboundH := pipeline.NewHolder(inboundPipeline)
	outboundH := pipeline.NewHolder(outboundPipeline)

	// Arm the config file watcher. ctx is cancelled on SIGTERM/SIGINT
	// so the reloader goroutine exits cleanly at shutdown.
	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()
	rld := reloader.New(*configPath, inboundH, outboundH, buildPipelines, cfg)
	if err := rld.Start(ctx); err != nil {
		log.Fatalf("reloader: %v", err)
	}

	// Build session store if enabled (nil when disabled — zero overhead).
	// Defaults to on; set session.enabled: false in runtime config to opt out.
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

	// Track servers for graceful shutdown
	var grpcServers []*grpc.Server
	var httpServers []*http.Server

	// Start listeners FIRST — before credential resolution
	switch cfg.Mode {
	case config.ModeEnvoySidecar:
		grpcServers = append(grpcServers, startGRPCExtProc(inboundH, outboundH, sessions, cfg.Listener.ExtProcAddr))

	case config.ModeWaypoint:
		grpcServers = append(grpcServers, startGRPCExtAuthz(inboundH, outboundH, cfg.Listener.ExtAuthzAddr))
		httpServers = append(httpServers, startHTTPServer("forward-proxy", forwardproxy.NewServer(outboundH, sessions).Handler(), cfg.Listener.ForwardProxyAddr))

	case config.ModeProxySidecar:
		rpSrv, err := reverseproxy.NewServer(inboundH, sessions, cfg.Listener.ReverseProxyBackend)
		if err != nil {
			log.Fatalf("creating reverse proxy: %v", err)
		}
		httpServers = append(httpServers, startHTTPServer("reverse-proxy", rpSrv.Handler(), cfg.Listener.ReverseProxyAddr))
		httpServers = append(httpServers, startHTTPServer("forward-proxy", forwardproxy.NewServer(outboundH, sessions).Handler(), cfg.Listener.ForwardProxyAddr))

	default:
		log.Fatalf("unhandled mode %q", cfg.Mode)
	}

	// /stats aggregates per-plugin counters at request time. Each
	// plugin that implements plugins.StatsSource contributes its own
	// *auth.Stats; the provider merges them into a single response
	// per HTTP request. Freshly-computed every call, so the numbers
	// reflect traffic up to the moment of the curl.
	// Freshly computed per /stats request. Load through the Holder so a
	// pipeline swap is reflected without restarting the stats handler.
	statsProvider := func() *auth.Stats {
		sources := plugins.CollectStats(inboundH.Load())
		sources = append(sources, plugins.CollectStats(outboundH.Load())...)
		return auth.MergeStats(sources...)
	}
	statSrv := startStatServer(cfg, rld.ConfigProvider(), statsProvider, rld.Handler())

	// Session events API (optional; only when session tracking is on).
	// The API has no authentication — bind only on in-cluster addresses and
	// never expose via ingress. Payloads contain raw user messages, LLM
	// completions, and tool results verbatim. Set session.enabled: false to
	// disable.
	var sessionAPISrv *sessionapi.Server
	if cfg.Listener.SessionAPIAddr != "" && sessions != nil {
		sessionAPISrv = sessionapi.New(
			cfg.Listener.SessionAPIAddr,
			sessions,
			sessionapi.WithPipelines(inboundH, outboundH),
		)
		go func() {
			slog.Warn("session API listening — UNAUTHENTICATED; contains raw user content; never expose via ingress",
				"addr", cfg.Listener.SessionAPIAddr)
			if err := sessionAPISrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("session API: %v", err)
			}
		}()
	}

	slog.Info("authbridge starting", "mode", cfg.Mode, "logLevel", logLevel.Level().String())

	// Health/readiness endpoints. /healthz is liveness (always OK);
	// /readyz ANDs pipeline.Ready() across inbound and outbound. A
	// plugin implementing pipeline.Readier — currently jwt-validation
	// and token-exchange — can return false while its deferred Init
	// goroutine is still waiting on credential files. The kubelet
	// holds traffic off the pod until all plugins are ready, so the
	// 503-from-OnRequest window at pod boot is closed.
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
			// Report the first not-ready plugin by name so operators
			// can diagnose from `kubectl describe pod` without
			// tailing container logs.
			// Holder-delegated — a hot-reloaded pipeline's readiness is
			// reflected in the next /readyz probe.
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

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	slog.Info("shutting down", "signal", sig)

	// Graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	for _, srv := range grpcServers {
		// GracefulStop blocks until all RPCs complete. If streams are long-lived
		// (e.g., ext_proc), fall back to hard Stop after the shutdown timeout.
		go func(s *grpc.Server) {
			<-shutdownCtx.Done()
			s.Stop()
		}(srv)
		srv.GracefulStop()
	}
	for _, srv := range httpServers {
		srv.Shutdown(shutdownCtx)
	}
	statSrv.Shutdown(shutdownCtx)
	if sessionAPISrv != nil {
		sessionAPISrv.Shutdown(shutdownCtx)
	}

	// Invoke plugin Shutdown hooks after listeners have stopped accepting
	// traffic so in-flight work is allowed to complete before plugins
	// flush state. Best-effort — individual errors are logged but do
	// not abort the sequence. Bounded by shutdownCtx's deadline.
	outboundPipeline.Stop(shutdownCtx)
	inboundPipeline.Stop(shutdownCtx)

	if sessions != nil {
		sessions.Close()
	}
}

func startGRPCExtProc(inbound, outbound *pipeline.Holder, sessions *session.Store, addr string) *grpc.Server {
	srv := grpc.NewServer()
	extprocv3.RegisterExternalProcessorServer(srv, &extproc.Server{
		InboundPipeline:  inbound,
		OutboundPipeline: outbound,
		Sessions:         sessions,
	})
	registerHealth(srv)
	reflection.Register(srv)

	go func() {
		lis, err := net.Listen("tcp", addr)
		if err != nil {
			log.Fatalf("ext_proc listen %s: %v", addr, err)
		}
		slog.Info("ext_proc gRPC listening", "addr", addr)
		if err := srv.Serve(lis); err != nil {
			log.Fatalf("ext_proc serve: %v", err)
		}
	}()
	return srv
}

func startGRPCExtAuthz(inbound, outbound *pipeline.Holder, addr string) *grpc.Server {
	srv := grpc.NewServer()
	authv3.RegisterAuthorizationServer(srv, &extauthz.Server{
		InboundPipeline:  inbound,
		OutboundPipeline: outbound,
	})
	registerHealth(srv)
	reflection.Register(srv)

	go func() {
		lis, err := net.Listen("tcp", addr)
		if err != nil {
			log.Fatalf("ext_authz listen %s: %v", addr, err)
		}
		slog.Info("ext_authz gRPC listening", "addr", addr)
		if err := srv.Serve(lis); err != nil {
			log.Fatalf("ext_authz serve: %v", err)
		}
	}()
	return srv
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

func registerHealth(srv *grpc.Server) {
	healthSrv := health.NewServer()
	healthpb.RegisterHealthServer(srv, healthSrv)
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
}
