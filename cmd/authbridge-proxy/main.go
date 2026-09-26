// Package main is the proxy-sidecar authbridge binary: HTTP forward
// proxy + reverse proxy, no Envoy / gRPC dependencies. It links only the
// plugins its build tags name — nothing is compiled in by default. Every
// plugin has its own plugins_<name>.go file gated by
// `//go:build include_plugin_<name>`, so a binary contains exactly the
// set it was built with. main.go imports no plugin package directly.
//
// Tag sets are generated per profile by scripts/profile-tags:
// `local` for the desktop artifact (the three parsers + tool-prune),
// `full` for the Kubernetes image. A build with no tags registers no
// plugins and will reject any config that names one.
//
// Mode is hardcoded to proxy-sidecar; YAML configs that specify a
// different mode are rejected at boot. For envoy-sidecar mode, use
// cmd/authbridge-envoy.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rossoctl/cortex/core/auth"
	"github.com/rossoctl/cortex/core/bootstrap"
	"github.com/rossoctl/cortex/core/clientstate"
	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/cost/ledger"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/memstore"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"
	"github.com/rossoctl/cortex/core/reloader"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/sessionapi"
	"github.com/rossoctl/cortex/core/spiffe"
	"github.com/rossoctl/cortex/core/tlsbridge"
	authtls "github.com/rossoctl/cortex/core/tlsconfig"

	// Only HTTP listeners are compiled in: no extproc
	// (no gRPC, no envoy types).
	"github.com/rossoctl/cortex/core/listener/forwardproxy"
	"github.com/rossoctl/cortex/core/listener/reverseproxy"
	"github.com/rossoctl/cortex/core/listener/skiphost"
	"github.com/rossoctl/cortex/core/listener/transparentproxy"
	// Plugins are wired via per-plugin plugins_<name>.go files, each gated
	// by `//go:build include_plugin_<name>`. main.go imports no plugin
	// package directly, so a binary links exactly the set its profile names
	// (see scripts/profile-tags).
)

// version is the authbridge-proxy build version, overridden at release time
// via -ldflags "-X main.version=<tag>". Defaults to "dev" for local builds.
var version = "dev"

// localMode is set by --local. It suppresses listeners that only make sense with
// iptables enforce-redirect: the demo uses cooperative HTTPS_PROXY, so nothing
// is ever REDIRECTed to the transparent listener and opening it would just be
// an idle port. The forward-role preset defaults transparent_proxy_addr to
// :8082, and config can't unset it (the preset refills an empty value), so this
// gate is the only way to keep the demo to the listeners it actually uses.
var localMode bool

// localStatePath is abctl's state file for this --local install, resolved while
// --local sets up and consumed later, once the bridge CA has been loaded, to warn
// about a client pointed at a different CA. Empty when not in --local: outside it
// there is no abctl-managed client to compare against.
var localStatePath string

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

// warnCostLedgerNeedsSessions says out loud that the cost ledger will not run
// because session tracking is off. Safe to call unconditionally — it checks both
// settings itself, and stays silent when there is nothing to report.
//
// The ledger records by being registered as a Recorder on the session store, and the
// whole block that constructs it is nested inside `if cfg.Session.SessionEnabled()`.
// With sessions off it is therefore unreachable rather than merely idle — and it used
// to be unreachable in complete silence: no error, no warning, not one log line
// naming the ledger, so an operator whose cost history was empty had to read main.go
// to find out why.
//
// The explicit contradiction (`cost_ledger.enabled: true` with `session.enabled:
// false`) is refused at load by config.Validate, so it never reaches here. What
// reaches here is the DEFAULT-ON case: a local install has the ledger on without
// anyone writing it down, and turning sessions off there is a legitimate choice that
// still silently costs the cost history. Only this binary knows which default
// applies, which is why the line is emitted here rather than in the config package.
//
// Names both settings, because the fix is a decision between them and a message that
// named only one would send the reader to the wrong file.
// ledgerDefaultOn decides whether an unset cost_ledger.enabled means ON, and says why.
//
// ON WHEREVER THE LEDGER CAN ACTUALLY DELIVER, rather than wherever a particular flag was passed. The
// old default was true under --local and false otherwise, so it described which FLAG started
// the process rather than whether durable cost history was achievable — and since every
// service install runs --config, the documented default was false on every installed laptop.
//
// Two ways to be durable, either sufficient:
//
//	an explicit cost_ledger.dir   an operator named a path, which in Kubernetes means a
//	                              volume is mounted there. Nothing else in this process can
//	                              see a volume, so this is the signal.
//	a config inside ~/.cortex     the laptop case: this process was started from a local
//	                              install's own config, so ~/.cortex/cost sits beside it and
//	                              persists the same way. Covers both shapes — the installed
//	                              service passes --config ~/.cortex/config.yaml, and --local
//	                              writes that file and points --config at it (see the flag
//	                              parsing below).
//
// A RESOLVABLE $HOME IS NOT ONE OF THEM, though it was: os.UserHomeDir succeeds in almost every
// container (HOME=/root), so that rule turned the ledger ON in precisely the place the paragraph
// below says it must be OFF.
//
// NOR IS AN EXISTING ~/.cortex, which was the first fix and is still too weak — a leftover
// directory is not a decision, and a k8s image with one baked in would turn on disk writes at
// 30-day retention. NOR --local ALONE, which sounds right and would reintroduce the regression
// this rule replaced: --local and --config are mutually exclusive, and every service install
// runs --config, so an installed laptop has localMode false.
//
// The config's LOCATION is the one signal that is a decision rather than a side effect: somebody
// installed this proxy into their home directory and started it from there.
//
// And the way to be neither: no dir, no --local, no ~/.cortex. That is a container with no
// volume, where the only writable place is the image layer — wiped on every restart, so the
// ledger would pay its whole cost and keep nothing, and counted against ephemeral-storage, where
// exceeding the limit EVICTS the pod. Measured growth is 36 MB to 1.2 GB per 30 days depending on
// label cardinality, so that is not a hypothetical limit. Off, with the reason said out loud.
//
// The reason is returned rather than logged here so the caller can log it once, next to the
// other ledger lines, instead of this being a function with a side effect.
func ledgerDefaultOn(cfg *config.Config, configPath string) (bool, string) {
	if cfg.CostLedger.DirSet() {
		return true, "cost_ledger.dir names a durable location"
	}
	dir, err := defaultCortexDir()
	if err != nil {
		return false, "no cost_ledger.dir and no resolvable home directory, so the only writable " +
			"location is a container layer that is discarded on restart"
	}
	if under, abs := pathUnder(dir, configPath); under {
		return true, "started from " + abs + ", inside a local install's " + dir
	}
	return false, "no cost_ledger.dir and the config is not inside " + dir + ", so this is not a " +
		"local install and the only writable location may be a container layer discarded on restart"
}

// pathUnder reports whether p is inside dir, and returns p absolute for the log line.
//
// filepath.Rel rather than a string prefix, so /home/u/.cortex-old cannot pass for /home/u/.cortex
// and a relative --config is judged the same way an absolute one is. A path that cannot be made
// absolute is treated as outside: the question is whether this is demonstrably a local install, so
// anything unresolvable answers no.
func pathUnder(dir, p string) (bool, string) {
	if p == "" {
		return false, ""
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return false, p
	}
	rel, err := filepath.Rel(dir, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false, abs
	}
	return true, abs
}

// ledgerDefaultOnValue is ledgerDefaultOn without the reason, for call sites that only need
// the decision. Kept separate rather than making the reason optional, so no caller can pass a
// default that disagrees with the one the ledger was built from.
func ledgerDefaultOnValue(cfg *config.Config, configPath string) bool {
	on, _ := ledgerDefaultOn(cfg, configPath)
	return on
}

// closeLedgerOnFatal hands the ledger's open minute back before a fatal exit. Set once, when the
// ledger opens; nil whenever there is no ledger to flush.
//
// PACKAGE LEVEL BECAUSE THE FATAL SITES ARE, and a defer cannot do this job at all: log.Fatalf ends
// in os.Exit, which runs no deferred functions. startTransparentProxy fails from its own function
// and from a goroutine inside it, so a closure in main would not reach either.
var closeLedgerOnFatal func()

// fatalf is log.Fatalf plus that flush. Every startup failure after the ledger opens happens with a
// minute of cost in memory, and exiting through log.Fatalf directly threw it away — the same loss the
// shutdown comment at the bottom of main says an orderly stop does not have. Close is guarded by a
// sync.Once, so flushing here and again on the normal path is free, and safe from a goroutine.
func fatalf(format string, args ...any) {
	if closeLedgerOnFatal != nil {
		closeLedgerOnFatal()
	}
	log.Fatalf(format, args...)
}

func warnCostLedgerNeedsSessions(cfg *config.Config, defaultOn bool, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.Session.SessionEnabled() || !cfg.CostLedger.LedgerEnabled(defaultOn) {
		return
	}
	// NOT `"cost_ledger.enabled", true` — this line is reachable only when the key is ABSENT,
	// because an explicit true with sessions off is refused at load (config.Validate). Printing the
	// key as though the operator had set it sends them to grep a config they never wrote; what is
	// true is that the default resolved to on.
	logger.Warn("cost ledger will NOT run — session tracking is disabled",
		"cost_ledger.enabled", "unset, defaulted to on",
		"session.enabled", false,
		"reason", "the ledger records through the session store (registered as a Recorder on it), so with the store off nothing reaches it",
		"effect", "no durable cost history; window=today, window=month and window=7d have nothing to read",
		"fix", "set session.enabled: true, or cost_ledger.enabled: false to say the ledger is not wanted")
}

func main() {
	configPath := flag.String("config", "", "path to config YAML file")
	showVersion := flag.Bool("version", false, "print version and exit")
	local := flag.Bool("local", false,
		"run with a built-in local config (forward-only TLS bridge + protocol parsers) that decrypts and parses an agent's egress; no --config, cluster, Keycloak, or SPIRE needed")
	// --demo is what this flag used to be called. Kept working so a command
	// already in someone's shell history or notes does not start failing, and
	// listed as a deprecated alias rather than hidden: an empty usage string
	// still prints the flag, just with a blank description that reads like a bug.
	demoDeprecated := flag.Bool("demo", false, "deprecated alias for -local")
	supervise := flag.Bool("supervise", false,
		"restart the proxy if it exits (launchd cannot be relied on for this; see supervise.go)")
	writeConfigOnly := flag.Bool("write-config", false,
		"with -local: create the built-in config (and its directory) if absent, then exit")
	caDir := flag.String("ca-dir", "",
		"CA directory for --local (auto-generated); defaults to ~/"+cortexDirName+"/"+caDirName)
	flag.Parse()
	if *supervise {
		// Before anything binds: this process starts a child that does the real work.
		if err := runSupervisor("supervise"); err != nil {
			log.Fatalf("supervise: %v", err)
		}
		return
	}

	if *showVersion {
		fmt.Println("authbridge-proxy", version)
		return
	}

	bootstrap.InitLogging("authbridge-proxy")
	bootstrap.StartSignalToggle()

	if *demoDeprecated && !*local {
		slog.Warn("--demo has been renamed to --local; it still works but will be removed",
			"use", "--local")
	}
	if *local || *demoDeprecated {
		localMode = true
		if *configPath != "" {
			log.Fatal("--local and --config are mutually exclusive")
		}
		cortexDir, derr := defaultCortexDir()
		if derr != nil {
			log.Fatalf("--local: %v", derr)
		}
		// The default moved here from ./cortex-ca. Someone who still has that
		// directory almost certainly has a client trusting the CA inside it,
		// and pointing at a stale CA fails silently — every request tunnels
		// through opaquely and no plugin sees a body. Name both paths.
		if st, serr := os.Stat(localDirFallback); serr == nil && st.IsDir() && *caDir == "" {
			slog.Warn("local mode — the CA now lives under $HOME; the ./"+localDirFallback+" here is no longer used",
				"now_using", filepath.Join(cortexDir, caDirName),
				"ignored", localDirFallback,
				"hint", "update the client's CA path (e.g. NODE_EXTRA_CA_CERTS), or pass --ca-dir ./"+localDirFallback+" to keep the old location")
		}
		// --ca-dir moves only the CA. The config stays at one known path, so a
		// client's trust anchor can be relocated without the config going
		// somewhere a later command can't find.
		dir := *caDir
		if dir == "" {
			dir = filepath.Join(cortexDir, caDirName)
		}
		absCA, aerr := filepath.Abs(dir)
		if aerr != nil {
			log.Fatalf("--local: resolving --ca-dir %q: %v", dir, aerr)
		}
		absCortex, cerr := filepath.Abs(cortexDir)
		if cerr != nil {
			log.Fatalf("--local: resolving %q: %v", cortexDir, cerr)
		}
		// The CA a client was configured against is compared once the CA in force has
		// actually been loaded — see the tls_bridge setup below. It cannot happen here:
		// the comparison is on certificates, and ours does not exist yet.
		localStatePath = filepath.Join(absCortex, clientstate.RelPath)
		// Drive the normal file-based load + hot-reload path, so editing the
		// config reloads live.
		p, werr := writeBuiltinConfig(absCortex, absCA)
		if werr != nil {
			log.Fatalf("--local: %v", werr)
		}
		*configPath = p
		// install.sh needs the config materialised without starting anything: it hands
		// the proxy to the OS supervisor, which then starts it. Writing the config was
		// previously a side effect of starting --local, so removing that start removed
		// the only thing that ever created the file — a fresh install then had nothing
		// for `abctl service install` to load. Exiting here keeps one source of truth
		// for the built-in config instead of teaching abctl to write it too.
		if *writeConfigOnly {
			// Silent on success. This runs from install.sh, which reports progress
			// itself; a structured INFO line with timestamps and key=value pairs in the
			// middle of that output reads like something went wrong. Failures still
			// surface — writeBuiltinConfig's error is fatal above.
			return
		}
		slog.Info("local mode — using the built-in config; edit it to hot-reload",
			"config", p, "ca_dir", absCA)
	} else if *caDir != "" {
		log.Fatal("--ca-dir only applies with --local")
	} else if *writeConfigOnly {
		log.Fatal("--write-config only applies with --local")
	}

	if *configPath == "" {
		log.Fatal("--config is required (or use --local for a built-in local config)")
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
	slog.Debug("config loaded", "configPath", *configPath)

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
	} else if bootCfg.SPIFFE != nil {
		slog.Info("spiffe block present but unused (no mTLS, no spiffe-identity plugin) — " +
			"skipping SPIRE provider; no Workload API connection will be attempted")
	} else {
		slog.Debug("Config does not use SPIFFE")
	}

	// The pricing registry is built ONCE, here, deliberately outside
	// buildPipelines, and starts EMPTY: buildPipelines loads the config and swaps
	// the real table in before anything reads it. The reloader re-invokes that closure on every config change,
	// but the usage aggregator that shares these rates is created further down and
	// outlives every rebuild — so a registry reconstructed per pipeline would leave
	// the aggregator holding a stale table forever, and /v1/usage would silently
	// disagree with the plugins about what a request cost. The table is swapped in
	// place instead; the pointer never changes. See pricing.Registry.
	pricingRegistry := pricing.NewRegistry(nil)

	// This binary is hardcoded to proxy-sidecar. Rejecting other modes
	// early gives operators a clear boot-time error instead of silently
	// misbehaving (e.g., YAML says envoy-sidecar but binary can't
	// serve ext_proc).
	// pendingPricing carries a rate table from a build to the commit that accepts it. See
	// buildPipelines and applyPricing.
	var pendingPricing *pricing.Table
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
		// Rates reload with the rest of the config. Swapped BEFORE the pipelines are
		// built so a plugin's Configure sees the new table, and swapped in place so
		// the usage aggregator — which holds this same registry from before the
		// reload — sees it too.
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
		// PREPARED, NOT APPLIED. Swapping here applied a rate table that the reload might still
		// REFUSE: build runs before the reloader's unreloadable-field check, so a save touching both
		// pricing.* and cost_ledger.* was rejected — pipelines untouched, /config still serving the old
		// rates — while live traffic was already priced from the rejected file. A failed pipeline Start
		// had the same shape. applyPricing below is called from the reloader's commit hook, and from
		// the initial build, so the table only ever takes effect on a config that was accepted.
		pendingPricing = tab
		return in, out, c, nil
	}

	// applyPricing puts a prepared table into effect. Called on the reload goroutine, which is
	// serialised, so the single pending slot needs no lock.
	applyPricing := func(c *config.Config) {
		if pendingPricing != nil {
			pricingRegistry.Swap(pendingPricing)
			pendingPricing = nil
		}
		if c != nil {
			c.Pricing.WarnIfUnpinned(slog.Default())
		}
	}

	inboundPipeline, outboundPipeline, cfg, err := buildPipelines()
	if err != nil {
		log.Fatalf("initial pipeline build: %v", err)
	}
	// The startup build has no reload to accept it, so it applies its own table. Without this the
	// process would run with an empty registry until the first successful reload.
	applyPricing(cfg)

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
	rld := reloader.New(*configPath, inboundH, outboundH, buildPipelines, cfg,
		reloader.WithOnCommit(applyPricing))
	if err := rld.Start(ctx); err != nil {
		log.Fatalf("reloader: %v", err)
	}

	var sessions *session.Store
	var usageAgg *usage.Aggregator
	var costLedger *ledger.Writer
	if cfg.Session.SessionEnabled() {
		// Store parameters come from config.SessionConfig.Limits, which is where the
		// defaults and the reasoning behind them live — one home for what used to be
		// this same block in three main packages.
		lim, err := cfg.Session.Limits()
		if err != nil {
			slog.Warn("invalid session.ttl, using default", "value", cfg.Session.TTL, "error", err)
		}
		sessions = session.New(lim.TTL, lim.MaxEvents, lim.MaxSessions)

		// Usage aggregation feeds GET /v1/usage. Registered as a store Recorder
		// so it sees every appended event, and deliberately independent of the
		// event store's own retention: session.max_events trims the per-session
		// event list, but a bucket counter must keep counting after the events
		// it counted have aged out, or a chart would appear to lose history an
		// operator could still see a minute ago.
		//
		// No Pricer is passed, so cost fields stay absent and the response
		// reports priced:false — see the TODO on usage.Pricer for where real
		// rates would come from.
		// Same session cap as the store, so the two agree on how many sessions
		// are worth remembering. The aggregator reclaims its coldest ring at the
		// cap rather than refusing new sessions, which matters because the store
		// evicts and expires sessions without telling it.
		// WithPricing closes the gap that made cost depend on which plugins were
		// configured: without it, only litellm-budget-track could produce a figure,
		// so a pipeline running just inference-parser reported every request
		// unpriced however many tokens it burned. The registry is the same
		// long-lived one the plugins hold, so a config reload moves both together.
		usageAgg = usage.New(usage.WithMaxSessions(lim.MaxSessions), usage.WithPricing(pricingRegistry))
		sessions.AddRecorder(usageAgg)

		// The durable cost ledger is a SECOND Recorder alongside the aggregator, not a
		// reader of it: the aggregator keeps independent marginals (by-model,
		// by-endpoint, by-provenance) rather than the joint distribution a ledger row
		// needs, so summing them would double-count. See core/cost/ledger.
		//
		// ON WHEREVER IT CAN DELIVER, which is not the same as "on for --local". The
		// default used to be localMode, so it described which flag started the binary
		// rather than whether durable history was achievable — and every service install
		// runs --config, which made the documented default false on every installed
		// laptop. ledgerDefaultOn asks the question that actually decides it: is there a
		// location that survives a restart? An explicit cost_ledger.dir (a mounted volume
		// in Kubernetes) qualifies, and so does being started from a config inside
		// ~/.cortex, which is what a local install is. A container with neither does not,
		// because its only writable place is discarded on restart and counted against
		// ephemeral-storage, where the limit evicts the pod rather than dropping a figure.
		// cost_ledger.enabled still overrides in either direction.
		defaultOn, whyDefault := ledgerDefaultOn(cfg, *configPath)
		if cfg.CostLedger.LedgerEnabled(defaultOn) {
			dir, derr := costLedgerDir(cfg)
			if derr != nil {
				// Not fatal. The ledger is observability, and refusing to start the proxy
				// because cost history has nowhere to live would trade a nicety for an outage.
				slog.Warn("cost ledger disabled — cannot determine where to write it",
					"error", derr, "effect", "cost history will not survive a restart")
			} else {
				retention := 0
				if cfg.CostLedger != nil {
					retention = cfg.CostLedger.RetentionDays
				}
				// THE SAME REGISTRY THE AGGREGATOR AND THE PLUGINS HOLD, for the reason its own
				// construction above states: the pointer never changes and the table is swapped
				// in place, so the ledger cannot end up reading a stale table while /v1/usage
				// reads a fresh one. It is used only to rebuild the per-tier split on rows
				// written before that split was persisted — no total is priced from it here.
				led, lerr := ledger.New(dir, ledger.WithRetentionDays(retention),
					ledger.WithPricing(pricingRegistry))
				if lerr != nil {
					slog.Warn("cost ledger disabled — could not open it",
						"dir", dir, "error", lerr, "effect", "cost history will not survive a restart")
				} else {
					costLedger = led
					closeLedgerOnFatal = func() {
						if cerr := costLedger.Close(); cerr != nil {
							slog.Warn("cost ledger: final flush failed during a fatal startup error",
								"error", cerr)
						}
					}
					sessions.AddRecorder(costLedger)
					slog.Info("cost ledger enabled — durable cost history for window=today, window=month and window=7d",
						// THE RESOLVED VALUE, not the configured one: `retention` is 0 on a
						// default install and the store turns that into its default, so logging
						// the request told an operator "retentionDays=0" for a ledger keeping 31.
						"dir", dir, "retentionDays", costLedger.RetentionDays(),
						// WHY it is on, because the default is now derived rather than
						// keyed on a flag: an operator reading this line can tell an
						// explicit choice from a resolved one without reading main.go.
						"default", whyDefault,
						"note", "closed minutes only, written off the request path; an unclean stop loses up to 60s of cost")
				}
			}
		} else {
			// Said out loud, at the same level as "session tracking disabled", because the
			// absence is what makes window=today degrade to the ring's 6 hours — and a
			// degraded answer with no log line behind it reads as a bug in abctl.
			slog.Info("cost ledger disabled — window=today, window=month and window=7d will be served from the 6h in-memory ring",
				// The DERIVED reason, not a guess about the deployment. It used to say
				// "not a local install", which was the old localMode default describing
				// itself — and it was wrong on the machine where it mattered most, since an
				// installed laptop service is not a local install by that definition either.
				"reason", whyDefault,
				"fix", "set cost_ledger.dir to a path on a mounted volume, or cost_ledger.enabled: true if this filesystem does persist")
		}

		// Through lim.LogAttrs, not a hand-rolled attribute list. #999 gave the session
		// store's limits one home, and the local "ttl=0s would read like a
		// misconfiguration" formatting this branch had here moved with them — so the
		// zero-value wording now lives beside the limits it describes instead of at this
		// call site.
		slog.Info("session tracking enabled", lim.LogAttrs()...)
	} else {
		slog.Info("session tracking disabled")
	}
	// Outside the branch on purpose: the ledger block above is nested inside
	// `if cfg.Session.SessionEnabled()`, so with sessions off it is not merely
	// disabled but unreachable, and this is the only place that knows which
	// deployment default applied. The helper decides for itself whether there is
	// anything to say, so this call is unconditional rather than branch-local —
	// a warning that only exists down one arm of an if is the shape that produced
	// the silence in the first place.
	warnCostLedgerNeedsSessions(cfg, ledgerDefaultOnValue(cfg, *configPath), slog.Default())

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
			fatalf("mtls requires the spiffe block to be configured")
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
		// deployments need both ends compatible. See CLAUDE.md
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
			fatalf("tls-bridge CA init failed: %v", cerr)
		}
		if generated {
			// "already running" is the half people miss. A client reads its CA file
			// ONCE, at process start, so every agent that was already up is holding
			// the previous CA — or none — and will reject the leaves this new one
			// signs. It does not fail loudly: the bridge falls back to tunnelling,
			// so the traffic still flows and every body-reading plugin goes blind
			// with nothing on the client side to notice.
			//
			// Reached on a first install, after ~/.cortex is deleted and recreated —
			// which the uninstall instructions tell people to do — and when the CA is
			// RENEWED near its 365-day expiry (EnsureFileSource). The renewal case is
			// the one nobody expects: a proxy that has been working for a year
			// suddenly needs every client restarted, on a boot where nothing else
			// changed. A plain upgrade preserves the CA and is unaffected.
			slog.Warn("tls-bridge: generated self-signed CA (generate_ca=true; standalone/demo)",
				"ca_dir", cfg.TLSBridge.CADir,
				"hint", "clients must trust it, e.g. NODE_EXTRA_CA_CERTS="+cfg.TLSBridge.CADir+"/ca.crt",
				"restart_clients", "agents already running trust a different CA (or none) and cannot be observed until restarted")
		}
		// Now that the CA in force is loaded, compare it against the one a client was
		// configured with. This has to happen here rather than during --local setup:
		// the comparison is on certificates, so ours must exist first. Skipped outside
		// --local, where there is no abctl-managed client to reason about.
		if localStatePath != "" {
			if args := staleClientCAWarning(cfg.TLSBridge.CADir, clientCAFromState(localStatePath), src.CACertPEM()); args != nil {
				slog.Warn("a client is configured against a different bridge CA than the one now in force", args...)
			}
		}
		// Assemble the CA + platform-roots bundle for tools whose CA setting
		// REPLACES their trust store rather than extending it (Go's SSL_CERT_FILE,
		// GIT_SSL_CAINFO, REQUESTS_CA_BUNDLE, CURL_CA_BUNDLE). Pointing those at
		// ca.crt alone leaves the process trusting this CA and nothing else, which
		// breaks every unproxied TLS connection it makes.
		//
		// This runs per boot and is a snapshot, not a subscription — see
		// EnsureTrustBundle's doc for what that costs, notably that a root the OS
		// distrusts after this point keeps being trusted until the next restart.
		//
		// Renewal gives one of those costs teeth it did not have before. When
		// findSystemRootsFrom cannot locate a host root store, EnsureTrustBundle
		// deliberately keeps the existing bundle rather than writing a CA-only one
		// (bundle.go:126-140) — a tradeoff reasoned about stale platform ROOTS. On a
		// renewal boot that same path also pins the stale bridge CA, and these four
		// variables REPLACE a client's trust store, so such a client would verify
		// against a CA the proxy no longer signs with. Narrow (the root store has to
		// be unfindable) and it still warns below, but it is no longer only about roots.
		//
		// Non-fatal in every case: the bridge works without the bundle, only a
		// client's ability to verify it is affected.
		bundlePath, berr := tlsbridge.EnsureTrustBundle(cfg.TLSBridge.CADir)
		switch {
		case berr == nil:
			slog.Info("tls-bridge: CA trust bundle ready", "path", bundlePath)
		case errors.Is(berr, tlsbridge.ErrCADirNotWritable):
			// The in-cluster norm: ca_dir is a read-only cert-manager Secret mount,
			// and a sidecar has no use for the bundle anyway — it exists so a
			// developer's git/curl/Python can verify a laptop bridge. Warning here
			// would name four laptop-only variables on every production boot.
			slog.Debug("tls-bridge: no CA trust bundle (ca_dir is read-only, normal for a "+
				"mounted Secret); in-cluster clients trust the CA through their own config",
				"ca_dir", cfg.TLSBridge.CADir)
		default:
			slog.Warn("tls-bridge: no CA trust bundle written; tools whose CA setting replaces the "+
				"trust store (SSL_CERT_FILE, GIT_SSL_CAINFO, REQUESTS_CA_BUNDLE, CURL_CA_BUNDLE) "+
				"have no safe file to point at",
				"ca_dir", cfg.TLSBridge.CADir, "error", berr)
		}
		var extra []byte
		if cfg.TLSBridge.UpstreamCABundle != "" {
			if extra, err = os.ReadFile(cfg.TLSBridge.UpstreamCABundle); err != nil {
				fatalf("tls-bridge upstream_ca_bundle read failed: %v", err)
			}
		}
		up, uerr := tlsbridge.NewUpstreamClient(extra, cfg.TLSBridge.UpstreamInsecure)
		if uerr != nil {
			fatalf("tls-bridge upstream client failed: %v", uerr)
		}
		if cfg.TLSBridge.UpstreamInsecure {
			slog.Warn("tls-bridge: re-origination does NOT verify the upstream TLS cert", "upstream_insecure", true)
		}
		minter := tlsbridge.NewMinter(src, tlsbridge.MinterOpts{})
		var ports map[int]bool // nil => NewDecision defaults to {443, 8443}
		if len(cfg.TLSBridge.Ports) > 0 {
			ports = make(map[int]bool, len(cfg.TLSBridge.Ports))
			for _, p := range cfg.TLSBridge.Ports {
				ports[p] = true
			}
		}
		// A bad passthrough pattern is fatal rather than ignored: silently not
		// matching presents as "the bridge broke my tool", with nothing tying the
		// symptom back to the typo.
		decision, derr := tlsbridge.NewDecision(tlsbridge.DecisionOpts{
			Ports: ports, SkipHosts: cfg.TLSBridge.PassthroughHosts,
		})
		if derr != nil {
			fatalf("tls-bridge: %v", derr)
		}
		bridge = &tlsbridge.Engine{
			Decision: decision,
			Term:     tlsbridge.NewTerminator(minter),
			Skip:     tlsbridge.NewSkipSet(),
			Upstream: up,
			CAPEM:    src.CACertPEM(),
			CAFile:   caTrustPath(cfg.TLSBridge.CADir),
		}
		slog.Info("tls-bridge enabled", "ca_dir", cfg.TLSBridge.CADir)
	}

	// Proxy-sidecar: reverse proxy (inbound) and/or forward proxy (outbound),
	// selected by listener.roles (empty => both). sharedStore is created up
	// front so whichever proxies run share one session store.
	roles := cfg.Listener.ActiveRoles()
	sharedStore := memstore.New()
	defer sharedStore.Close() // stop the TTL janitor on normal main return

	if roles[config.RoleReverse] {
		// Two inbound shapes, selected by listener.inbound_interception:
		//   transparent   — iptables PREROUTING REDIRECTs here; the forwarding
		//                   target is per-connection, from SO_ORIGINAL_DST.
		//   reverse-proxy — the default; one fixed reverse_proxy_backend, reached
		//                   because the operator stole the agent's port.
		if cfg.Listener.InboundTransparent() {
			rpSrv, rerr := reverseproxy.NewTransparentServer(inboundH, sessions, rpMTLS)
			if rerr != nil {
				fatalf("creating transparent inbound proxy: %v", rerr)
			}
			rpSrv.Shared = sharedStore
			// Skipped in --local: there is no iptables there, so nothing would ever
			// be REDIRECTed to the listener and every request would fail closed.
			if localMode {
				slog.Warn("demo mode: transparent inbound listener not started (no iptables to REDIRECT to it)")
			} else {
				rpHTTP, rerr := bootstrap.StartTransparentInboundServer("transparent-inbound", rpSrv, cfg.Listener.TransparentInboundAddr)
				if rerr != nil {
					fatalf("transparent-inbound listen: %v", rerr)
				}
				httpServers = append(httpServers, rpHTTP)
			}
		} else {
			rpSrv, rerr := reverseproxy.NewServer(inboundH, sessions, cfg.Listener.ReverseProxyBackend, rpMTLS)
			if rerr != nil {
				fatalf("creating reverse proxy: %v", rerr)
			}
			rpSrv.Shared = sharedStore
			rpHTTP, rerr := bootstrap.StartReverseProxyServer("reverse-proxy", rpSrv, cfg.Listener.ReverseProxyAddr)
			if rerr != nil {
				fatalf("reverse-proxy listen: %v", rerr)
			}
			httpServers = append(httpServers, rpHTTP)
		}
	}

	// The transparent (enforce-redirect) listener rides with the forward proxy;
	// declared here so shutdown can close it whether or not the forward role ran.
	var transparentLn *net.TCPListener
	if roles[config.RoleForward] {
		fpSrv, ferr := forwardproxy.NewServer(outboundH, sessions, fpMTLS)
		if ferr != nil {
			fatalf("creating forward proxy: %v", ferr)
		}
		// SkipHosts: outbound destinations that bypass the pipeline AND
		// session recording entirely. See ListenerConfig.SkipHosts for the
		// motivating case (chatty observability sidecars evicting the
		// inbound A2A user intent from the session FIFO).
		skipHosts, serr := skiphost.New(cfg.Listener.SkipHosts)
		if serr != nil {
			fatalf("listener.skip_hosts: %v", serr)
		}
		fpSrv.SkipHosts = skipHosts
		fpSrv.TLSBridge = bridge
		fpSrv.Shared = sharedStore
		// Per-session bucketing. Without it every coding-agent session on the
		// machine records into one shared bucket: two Claude Code windows
		// interleave and per-session cost cannot be computed at all. Defaults to
		// the supported agents' session headers (config.SessionIDHeaders);
		// session.id_headers: [] turns it off.
		fpSrv.SessionIDHeaders = cfg.Session.SessionIDHeaders()
		fpHTTP, herr := bootstrap.StartHTTPServer("forward-proxy", fpSrv.Handler(), cfg.Listener.ForwardProxyAddr)
		if herr != nil {
			fatalf("forward-proxy listen: %v", herr)
		}
		httpServers = append(httpServers, fpHTTP)

		// Outbound transparent listener (enforce-redirect mode). It shares the
		// forward proxy's outbound pipeline via HandleTransparentConn, so explicit
		// HTTP_PROXY egress and iptables-REDIRECTed bypass egress are gated and
		// tunnelled identically. Closed explicitly on shutdown (not an *http.Server).
		// Skipped in --local: no iptables there, so nothing is ever REDIRECTed to it.
		if !localMode {
			transparentLn = startTransparentProxy(fpSrv, cfg.Listener.TransparentProxyAddr)
		}
	}

	_ = mtlsMetrics // TODO Phase 2: surface metrics through /stats

	statsProvider := func() *auth.Stats {
		sources := plugins.CollectStats(inboundH.Load())
		sources = append(sources, plugins.CollectStats(outboundH.Load())...)
		return auth.MergeStats(sources...)
	}
	statSrv, statErr := bootstrap.StartStatServer(cfg, rld.ConfigProvider(), statsProvider, rld.Handler(), pricingRegistry.Handler(), cfg.Stats.StatsAddress)
	if statErr != nil {
		fatalf("stat server listen: %v", statErr)
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
			sessionapi.WithUsage(usageAgg),
			// nil when the ledger is off, which handleUsage reads as "serve the ring's
			// maximum window and say which window that was".
			sessionapi.WithCostLedger(costLedger),
		)
		go func() {
			slog.Warn("session API listening — UNAUTHENTICATED; contains raw user content; never expose via ingress",
				"addr", cfg.Listener.SessionAPIAddr)
			if err := sessionAPISrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				fatalf("session API: %v", err)
			}
		}()
	}

	slog.Info("authbridge-proxy starting", "version", version, "mode", cfg.Mode, "logLevel", bootstrap.LogLevel().String())

	healthSrv, healthErr := bootstrap.StartHealthServer(inboundH, outboundH, cfg.Listener.HealthAddr)
	if healthErr != nil {
		fatalf("health server listen: %v", healthErr)
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

	// Flushed AFTER both pipelines have stopped and before the store closes, so the
	// final minute includes every event a draining request still produced. Any earlier
	// and a request finishing during the pipeline drain would land in a minute already
	// written; any later and the store is gone.
	//
	// This is what turns "a restart loses up to 60 seconds of cost" into "an orderly
	// stop loses nothing" — the ledger holds only closed minutes on disk precisely so
	// that the minute still accumulating has exactly one owner, and Close is what hands
	// it over. Close also stops the ledger's writer goroutine, so it must come after
	// anything that can still record. A SIGKILL still loses the open minute, and
	// nothing can change that.
	if costLedger != nil {
		if err := costLedger.Close(); err != nil {
			slog.Warn("cost ledger: final flush failed; the last minute of cost is lost", "error", err)
		}
	}

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
		fatalf("resolve transparent-proxy addr %q: %v", addr, err)
	}
	ln, err := net.ListenTCP("tcp", la)
	if err != nil {
		fatalf("transparent-proxy listen on %q: %v", addr, err)
	}
	srv := transparentproxy.NewServer(fp.HandleTransparentConn)
	go func() {
		slog.Info("transparent proxy listening", "addr", addr)
		if err := srv.Serve(ln); err != nil {
			fatalf("transparent-proxy serve: %v", err)
		}
	}()
	return ln
}

// caTrustPath returns the absolute path of the CA clients must trust.
//
// Absolute because a client is configured with this path (NODE_EXTRA_CA_CERTS
// and friends) and a mismatched trust anchor fails silently — every request
// tunnels through opaquely and no plugin sees a body. --local now resolves under
// $HOME rather than the launch directory, which removes most of the ways that
// happened, but --ca-dir still accepts a relative path.
func caTrustPath(caDir string) string {
	p := filepath.Join(caDir, "ca.crt")
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}
