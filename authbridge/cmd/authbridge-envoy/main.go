// Package main is the envoy-sidecar authbridge binary: an ext_proc
// gRPC server intended to run alongside Envoy in a sidecar (or as a
// shared service hooked into Envoy's external_processor filter), with
// the full plugin set compiled in (jwt-validation, token-exchange,
// a2a-parser, mcp-parser, inference-parser).
//
// Mode is hardcoded to envoy-sidecar; YAML configs that specify a
// different mode are rejected at boot. For proxy-sidecar mode (HTTP
// forward/reverse proxies, no Envoy), use cmd/authbridge-proxy. For a
// size-optimized proxy-sidecar build with parsers dropped, use
// cmd/authbridge-lite.
package main

import (
	"context"
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
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/reloader"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/session"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/sessionapi"

	// Only the ext_proc listener is compiled in (no ext_authz, no
	// HTTP proxies).
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/listener/extproc"

	// Plugins. Auth gates first, then the protocol parsers that
	// supply session-event context for abctl.
	_ "github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/a2aparser"
	_ "github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/ibac"
	_ "github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/inferenceparser"
	_ "github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/jwtvalidation"
	_ "github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/mcpparser"
	_ "github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/tokenexchange"
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

	buildPipelines := func() (*pipeline.Pipeline, *pipeline.Pipeline, *config.Config, error) {
		c, err := config.Load(*configPath)
		if err != nil {
			return nil, nil, nil, err
		}
		if c.Mode != "" && c.Mode != config.ModeEnvoySidecar {
			return nil, nil, nil, fmt.Errorf(
				"authbridge-envoy supports only mode=%q (got %q); use cmd/authbridge for other modes",
				config.ModeEnvoySidecar, c.Mode)
		}
		c.Mode = config.ModeEnvoySidecar
		config.ApplyPreset(c)
		if err := config.Validate(c); err != nil {
			return nil, nil, nil, err
		}
		in, err := plugins.Build(c.Pipeline.Inbound.Plugins)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("inbound: %w", err)
		}
		out, err := plugins.Build(c.Pipeline.Outbound.Plugins)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("outbound: %w", err)
		}
		return in, out, c, nil
	}

	inboundPipeline, outboundPipeline, cfg, err := buildPipelines()
	if err != nil {
		log.Fatalf("initial pipeline build: %v", err)
	}

	// Single-player mode is a process-wide feature gate consumed by the
	// auth package's inbound→outbound token bridge. Read from top-level
	// config once at startup; jwt-validation captures and token-exchange
	// reads the cache only when this is true.
	//
	// The startup log line below makes the active posture discoverable
	// on every restart — important because the default-on behavior
	// changes the security profile on upgrade for deployments that were
	// previously failing OBO-with-non-propagating-agents at the
	// no-token-policy boundary.
	spmEnabled := cfg.SingleUserModeEnabled()
	auth.SetSingleUserModeEnabled(spmEnabled)
	if spmEnabled {
		slog.Info("authbridge: single-user mode is ACTIVE — sidecar will bridge inbound→outbound user tokens for non-propagating agent runtimes. Safe ONLY for single-user-per-process workloads; multi-user concurrent agents must propagate the inbound Authorization header themselves and should set single_user_mode: false. See docs/plugin-reference.md.")
	} else {
		slog.Info("authbridge: single-user mode is DISABLED — agent must propagate the inbound Authorization header to outbound calls for OBO to work. See docs/plugin-reference.md.")
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

	var grpcServers []*grpc.Server
	grpcServers = append(grpcServers, startGRPCExtProc(inboundH, outboundH, sessions, cfg.Listener.ExtProcAddr))

	statsProvider := func() *auth.Stats {
		sources := plugins.CollectStats(inboundH.Load())
		sources = append(sources, plugins.CollectStats(outboundH.Load())...)
		return auth.MergeStats(sources...)
	}
	statSrv := startStatServer(cfg, rld.ConfigProvider(), statsProvider, rld.Handler())

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

	slog.Info("authbridge-envoy starting", "mode", cfg.Mode, "logLevel", logLevel.Level().String())

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

	for _, srv := range grpcServers {
		go func(s *grpc.Server) {
			<-shutdownCtx.Done()
			s.Stop()
		}(srv)
		srv.GracefulStop()
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
