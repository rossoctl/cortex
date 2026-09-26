// Package main is the envoy-sidecar authbridge binary: an ext_proc
// gRPC server intended to run alongside Envoy in a sidecar (or as a
// shared service hooked into Envoy's external_processor filter).
//
// It links only the plugins its build tags name — nothing is compiled in
// by default. Every plugin has its own plugins_<name>.go file gated by
// `//go:build include_plugin_<name>`, and this binary's set is the
// `envoy` profile in scripts/profile-tags. main.go imports no
// plugin package directly.
//
// Mode is hardcoded to envoy-sidecar; YAML configs that specify a
// different mode are rejected at boot. For proxy-sidecar mode (HTTP
// forward/reverse proxies, no Envoy), use cmd/authbridge-proxy.
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
	"syscall"
	"time"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	"github.com/rossoctl/cortex/core/auth"
	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"
	"github.com/rossoctl/cortex/core/pricing"
	"github.com/rossoctl/cortex/core/reloader"
	"github.com/rossoctl/cortex/core/runtimeutil"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/sessionapi"
	"github.com/rossoctl/cortex/core/shared"
	"github.com/rossoctl/cortex/core/spiffe"

	// Only the ext_proc listener is compiled in (no HTTP proxies).
	"github.com/rossoctl/cortex/core/listener/extproc"
	"github.com/rossoctl/cortex/core/listener/skiphost"
	// Plugins. Auth gates first, then the protocol parsers that
	// supply session-event context for abctl.
)

// warnCostLedgerInert says out loud that a cost_ledger block in this binary's config
// does nothing. Safe to call unconditionally; silent when the block is absent.
//
// INERT BY DESIGN, not an oversight, and that is why this is a log line rather than
// wiring or a refusal:
//
//   - Wiring it would be wrong here. authbridge-envoy is an ext_proc sidecar, which
//     is a Kubernetes shape, and the ledger is deliberately OFF in Kubernetes (see
//     CostLedgerConfig): a pod's filesystem is ephemeral, one replica's day files are
//     invisible to the next, and the right sink for fleet-wide spend is a central
//     collector rather than N per-pod files nobody collects. Wiring it would
//     manufacture exactly the arrangement the proxy's own comment argues against.
//
//   - Refusing it at load would be wrong too. config.Validate is shared by every
//     binary, and the gap is per-BINARY rather than per-mode — authbridge-cpex runs
//     proxy-sidecar mode and has no ledger either — so a mode-keyed refusal would
//     both miss cpex and turn a stray inherited key into a crash-loop over an
//     observability nicety. main.go already makes that trade the other way for a
//     ledger it cannot open.
//
// What is NOT defensible is what it did before: load the key, validate it, and
// discard it without a word. The line names the key, says it is inert, and points at
// the binary that does honour it.
func warnCostLedgerInert(cfg *config.Config, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.CostLedger == nil {
		return
	}
	logger.Warn("cost_ledger is configured but INERT in authbridge-envoy — no cost history will be written",
		"reason", "this binary wires no cost ledger and no usage aggregator; the ext_proc sidecar is a Kubernetes shape, where per-pod day files are the wrong sink for spend (use a central collector)",
		"effect", "the whole cost_ledger block is ignored, including dir and retention_days",
		"fix", "remove the cost_ledger block here; for durable local cost history run authbridge-proxy --local, which does honour it")
}

func main() {
	configPath := flag.String("config", "", "path to config YAML file")
	flag.Parse()

	runtimeutil.InitLogging("authbridge-envoy")
	runtimeutil.StartSignalToggle()

	if *configPath == "" {
		log.Fatal("--config is required and must point to a YAML file")
	}

	// Build the SPIFFE Provider when the spiffe block is configured.
	// envoy-sidecar mode terminates mTLS in Envoy itself (via the
	// file-based DownstreamTlsContext / UpstreamTlsContext referencing
	// /opt/svid*.pem in the rendered envoy-config) — this binary
	// doesn't see the TLS bytes directly, so X509Source() isn't read
	// here. The Provider is still needed because token-exchange's
	// spiffe identity path consumes a JWTSource via DI, and the file
	// mirror is what keeps /opt/svid.pem, /opt/svid_key.pem,
	// /opt/svid_bundle.pem, and /opt/jwt_svid.token fresh on disk for
	// Envoy and other consumers.
	//
	// We need cfg first to read the spiffe block, so do a one-shot
	// Load before buildPipelines runs (buildPipelines re-Loads
	// internally for hot-reload). The Provider is captured by
	// buildPipelines via closure so reload-time pipeline rebuilds
	// inject the same Provider into freshly constructed plugin
	// instances.
	bootCfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("failed to load config %q: %v", *configPath, err)
	}
	slog.Debug("config loaded", "configPath", *configPath)

	var provider *spiffe.Provider
	if bootCfg.SPIFFE != nil {
		mirrorFiles := true
		if bootCfg.SPIFFE.MirrorFiles != nil {
			mirrorFiles = *bootCfg.SPIFFE.MirrorFiles
		}
		slog.Debug("About to create SPIFFE Provider", "bootCfg.SPIFFE.Socket", bootCfg.SPIFFE.Socket)
		provider, err = spiffe.NewProvider(context.Background(), spiffe.ProviderConfig{
			SocketPath:  bootCfg.SPIFFE.Socket,
			MirrorFiles: mirrorFiles,
			MirrorDir:   bootCfg.SPIFFE.MirrorDir,
		})
		if err != nil {
			log.Fatalf("spiffe provider: %v", err)
		}
		defer provider.Close()
		slog.Debug("SPIFFE provider created", "bootCfg.SPIFFE.Socket", bootCfg.SPIFFE.Socket)
	} else {
		slog.Debug("Config does not use SPIFFE")
	}

	// Built once, outside buildPipelines, because the reloader re-invokes that
	// closure while anything sharing these rates outlives the rebuild. Without this
	// the binary injected no resolver at all: tool-prune is linked here by default
	// and used to ship its own rate table, so `$ saved` worked unconfigured — and
	// config.Validate builds the pricing table and discards it, so an operator's
	// `pricing:` block validated cleanly and was then never applied.
	//
	// Starts EMPTY. buildPipelines below loads the config and swaps the real table in
	// before anything reads this, so building one here too was duplicate work — and
	// in this binary it ran before the mode check and Validate, so a wrong-mode
	// config reported a pricing error instead of the clearer mode error.
	pricingRegistry := pricing.NewRegistry(nil)

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
		config.WarnEmptyPipelines(c, slog.Default())
		// Rates reload with the rest of the config, swapped in place so anything
		// holding this registry from before the reload sees the new table.
		tab, err := pricing.Build(c.Pricing)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("pricing: %w", err)
		}

		// The registry still holds the PREVIOUS table while the pipelines build, and
		// is swapped only once both have. Swapping first made a reload
		// non-transactional: a plugin-build failure makes the reloader reject the
		// change and keep the running pipelines, but the rates had already moved — so
		// live traffic priced from a config that was refused. Plugins only store the
		// resolver during Configure and never resolve through it, so building against
		// the old table is safe.
		deps := plugins.Deps{SPIFFE: provider, Pricing: pricingRegistry}
		in, err := plugins.BuildWithDeps(c.Pipeline.Inbound.Plugins, deps)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("inbound: %w", err)
		}
		out, err := plugins.BuildWithDeps(c.Pipeline.Outbound.Plugins, deps)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("outbound: %w", err)
		}
		pricingRegistry.Swap(tab)
		c.Pricing.WarnIfUnpinned(slog.Default())
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
		// Store parameters come from config.SessionConfig.Limits, which is where the
		// defaults and the reasoning behind them live — one home for what used to be
		// this same block in three main packages.
		lim, err := cfg.Session.Limits()
		if err != nil {
			slog.Warn("invalid session.ttl, using default", "value", cfg.Session.TTL, "error", err)
		}
		sessions = session.New(lim.TTL, lim.MaxEvents, lim.MaxSessions)
		slog.Info("session tracking enabled", lim.LogAttrs()...)
	} else {
		slog.Info("session tracking disabled")
	}
	// This binary builds a session store and stops there — no usage aggregator, no
	// cost ledger — so a cost_ledger block in its config is loaded, validated, and
	// thrown away. That is a deliberate absence (see the helper), but silence about
	// it is not.
	warnCostLedgerInert(cfg, slog.Default())

	store := shared.New()
	defer store.Close() // stop the TTL janitor on normal main return

	// SkipHosts: outbound destinations that bypass the pipeline AND
	// session recording entirely. See ListenerConfig.SkipHosts for the
	// motivating case (chatty observability sidecars evicting the
	// inbound A2A user intent from the session FIFO).
	skipHosts, err := skiphost.New(cfg.Listener.SkipHosts)
	if err != nil {
		log.Fatalf("listener.skip_hosts: %v", err)
	}

	var grpcServers []*grpc.Server
	grpcServers = append(grpcServers, startGRPCExtProc(inboundH, outboundH, sessions, store, skipHosts, cfg.Listener.ExtProcAddr))

	statsProvider := func() *auth.Stats {
		sources := plugins.CollectStats(inboundH.Load())
		sources = append(sources, plugins.CollectStats(outboundH.Load())...)
		return auth.MergeStats(sources...)
	}
	statSrv, statErr := runtimeutil.StartStatServer(cfg, rld.ConfigProvider(), statsProvider, rld.Handler(), pricingRegistry.Handler(), cfg.Stats.StatsAddress)
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

	slog.Info("authbridge-envoy starting", "mode", cfg.Mode, "logLevel", runtimeutil.LogLevel().String())

	healthSrv, healthErr := runtimeutil.StartHealthServer(inboundH, outboundH, cfg.Listener.HealthAddr)
	if healthErr != nil {
		log.Fatalf("health server listen: %v", healthErr)
	}

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

func startGRPCExtProc(inbound, outbound *pipeline.Holder, sessions *session.Store, store pipeline.SharedStore, skipHosts *skiphost.Matcher, addr string) *grpc.Server {
	srv := grpc.NewServer()
	extprocv3.RegisterExternalProcessorServer(srv, &extproc.Server{
		InboundPipeline:  inbound,
		OutboundPipeline: outbound,
		Sessions:         sessions,
		Shared:           store,
		SkipHosts:        skipHosts,
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

func registerHealth(srv *grpc.Server) {
	healthSrv := health.NewServer()
	healthpb.RegisterHealthServer(srv, healthSrv)
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
}
