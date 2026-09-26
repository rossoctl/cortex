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
// from authbridge-proxy/main.go pending an core-side `Run()`
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
	"syscall"
	"time"

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
	authtls "github.com/rossoctl/cortex/core/tls"

	"github.com/rossoctl/cortex/core/listener/forwardproxy"
	"github.com/rossoctl/cortex/core/listener/reverseproxy"
	// Plugins — same set as authbridge-proxy, plus the cpex plugin
	// which lives behind //go:build cpex. The cpex import only fires
	// in this binary's build; pure-Go binaries (authbridge-proxy,
	// authbridge-envoy, authbridge-lite) don't import it.
)

// warnCostLedgerInert says out loud that a cost_ledger block in this binary's config
// does nothing. Safe to call unconditionally; silent when the block is absent.
//
// FOURTH BINARY WITH THIS SHAPE, and the same answer as authbridge-envoy's for the same
// reasons — see warnCostLedgerInert there, which this mirrors deliberately rather than
// paraphrases. This binary builds a session store and stops: no usage aggregator, no
// ledger, and a session API constructed without WithUsage, so the whole block is loaded,
// validated, and thrown away.
//
// INERT IS THE RIGHT ANSWER; the silence was not:
//
//   - Wiring it would be wrong here. authbridge-cpex runs proxy-sidecar mode in
//     Kubernetes, where the ledger is deliberately off (see CostLedgerConfig): a pod's
//     filesystem is ephemeral, one replica's day files are invisible to the next, and the
//     right sink for fleet-wide spend is a central collector rather than N per-pod files
//     nobody collects.
//   - Refusing it at load would be wrong too. config.Validate is shared by every binary
//     and this gap is per-BINARY rather than per-mode, so a mode-keyed refusal would turn
//     a stray inherited key into a crash-loop over an observability nicety.
//
// What is not defensible is loading the key, validating it, and discarding it without a
// word. The line names the key as it is spelled in YAML, says INERT rather than
// "disabled" (which reads as the ordinary Kubernetes default), and points at the binary
// that does honour it.
func warnCostLedgerInert(cfg *config.Config, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.CostLedger == nil {
		return
	}
	logger.Warn("cost_ledger is configured but INERT in authbridge-cpex — no cost history will be written",
		"reason", "this binary wires no cost ledger and no usage aggregator; it runs in Kubernetes, where per-pod day files are the wrong sink for spend (use a central collector)",
		"effect", "the whole cost_ledger block is ignored, including dir and retention_days",
		"fix", "remove the cost_ledger block here; for durable local cost history run authbridge-proxy --local, which does honour it")
}

func main() {
	configPath := flag.String("config", "", "path to config YAML file")
	flag.Parse()

	runtimeutil.InitLogging("authbridge-cpex")
	runtimeutil.StartSignalToggle()

	if *configPath == "" {
		log.Fatal("--config is required and must point to a YAML file")
	}

	bootCfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("failed to load config %q: %v", *configPath, err)
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
	// Outside the branch on purpose, like authbridge-proxy's: this binary builds a session
	// store and stops there — no usage aggregator, no cost ledger — so a cost_ledger block
	// in its config is loaded, validated and thrown away. That absence is deliberate (see
	// the helper); the silence about it was not, and a warning that only exists down one
	// arm of an if is the shape that produces the next silence.
	warnCostLedgerInert(cfg, slog.Default())

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
	// Same per-session bucketing as authbridge-proxy: a client-supplied session
	// id beats the global ActiveSession(), so concurrent agent sessions stay
	// separable. Falls back to the previous behavior when absent.
	//
	// The default list is shared with the laptop binary, so it applies in-cluster
	// too — where the headers are client-asserted by whatever can reach this proxy
	// rather than by an agent the operator started. session.id_headers: [] is the
	// off switch wherever attribution is a trust boundary; see
	// session.IDFromHeaders.
	fpSrv.SessionIDHeaders = cfg.Session.SessionIDHeaders()
	rpHTTP, err := runtimeutil.StartReverseProxyServer("reverse-proxy", rpSrv, cfg.Listener.ReverseProxyAddr)
	if err != nil {
		log.Fatalf("reverse-proxy listen: %v", err)
	}
	httpServers = append(httpServers, rpHTTP)
	fpHTTP, err := runtimeutil.StartHTTPServer("forward-proxy", fpSrv.Handler(), cfg.Listener.ForwardProxyAddr)
	if err != nil {
		log.Fatalf("forward-proxy listen: %v", err)
	}
	httpServers = append(httpServers, fpHTTP)
	_ = mtlsMetrics

	statsProvider := func() *auth.Stats {
		sources := plugins.CollectStats(inboundH.Load())
		sources = append(sources, plugins.CollectStats(outboundH.Load())...)
		return auth.MergeStats(sources...)
	}
	statSrv, statErr := runtimeutil.StartStatServer(cfg, rld.ConfigProvider(), statsProvider, rld.Handler(), pricingRegistry.Handler(), cfg.Stats.StatsAddress)
	if statErr != nil {
		log.Fatalf("stat server listen: %v", statErr)
	}

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

	slog.Info("authbridge-cpex starting", "mode", cfg.Mode, "logLevel", runtimeutil.LogLevel().String())

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

	for _, srv := range httpServers {
		srv.Shutdown(shutdownCtx)
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
