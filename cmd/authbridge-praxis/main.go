// Package main is the Praxis authbridge binary: Praxis reverse proxy,
// no forward proxy.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/praxis"
	"github.com/rossoctl/cortex/core/runtimeutil"
	"github.com/rossoctl/cortex/core/spiffe"
	// Only HTTP listeners are compiled in: no extproc
	// (no gRPC, no envoy types).
	// Plugins are wired via per-plugin plugins_<name>.go files, each gated
	// by `//go:build include_plugin_<name>`. main.go imports no plugin
	// package directly. This binary defines no plugins_*.go files of its own,
	// so it registers no plugins (see scripts/profile-tags).
)

// version is the authbridge-proxy build version, overridden at release time
// via -ldflags "-X main.version=<tag>". Defaults to "dev" for local builds.
var version = "dev"

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

// defaultPraxisConfigPath is where the generated Praxis configuration is
// written. Matches the path the Praxis run command in the package docs uses:
//
//	cargo run -p praxis-proxy -- -c /tmp/praxis-config.yaml
const defaultPraxisConfigPath = "/tmp/praxis-config.yaml"

// defaultPraxisPolicyPath is where the generated Praxis policy document is
// written. It is a second file, referenced by the proxy config's `policy`
// filter via config_path, and it is what makes the generated proxy enforce
// inbound JWT validation.
const defaultPraxisPolicyPath = "/tmp/praxis-policy.yaml"

func main() {
	configPath := flag.String("config", "", "path to config YAML file")
	praxisOut := flag.String("praxis-config-out", defaultPraxisConfigPath,
		"path to write the generated Praxis configuration to")
	praxisPolicyOut := flag.String("praxis-policy-out", defaultPraxisPolicyPath,
		"path to write the generated Praxis policy document to (referenced by the "+
			"generated config's policy filter; requires Praxis built with --features policy-engine)")
	audienceFile := flag.String("audience-file", "",
		"read the expected inbound JWT audience from this file when the jwt-validation "+
			"plugin config names none literally (the in-cluster default is the operator-mounted "+
			"/shared/client-id.txt). The value is baked into the generated policy, so regenerate "+
			"it if the workload's client ID is rotated")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("authbridge-proxy", version)
		return
	}

	runtimeutil.InitLogging("authbridge-praxis")
	runtimeutil.StartSignalToggle()

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
		log.Fatalf("failed to load config %q: %v", *configPath, err)
	}
	slog.Debug("config loaded", "configPath", *configPath)

	// Fill mode-specific listener defaults and validate the mode/listener
	// combination before converting. The Praxis conversion reads resolved
	// listener addresses (reverse_proxy_addr, forward_proxy_addr, ...) rather
	// than re-deriving them, so the preset must run first — otherwise a config
	// that relies on defaults would convert to a listener-less Praxis document.
	config.ApplyPreset(bootCfg)
	if err := config.Validate(bootCfg); err != nil {
		log.Fatalf("invalid config %q: %v", *configPath, err)
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

	// Note that hot reload is not yet supported

	// Translate the AuthBridge config into a Praxis configuration and write it
	// out, so Praxis can be started against it:
	//
	//   cargo run -p praxis-proxy -- -c /tmp/praxis-config.yaml
	//
	// Conversion is lossy by necessity: a default-feature Praxis build has no
	// JWT-validation or RFC 8693 token-exchange filter, so AuthBridge's auth
	// plugins have no counterpart there. Convert reports those rather than
	// dropping them silently, and they are logged at WARN below — a generated
	// proxy that no longer enforces inbound auth is precisely the kind of thing
	// that must not be discoverable only by reading the output file.
	if err := writePraxisConfig(bootCfg, *praxisOut, *praxisPolicyOut, *audienceFile); err != nil {
		log.Fatalf("failed to write Praxis config: %v", err)
	}

	// This binary generates configuration; it does not proxy. Praxis itself is
	// the data plane, started separately against the files just written — in
	// the container image, by the entrypoint that runs this binary first.
	//
	// Exiting 0 is what makes that chaining possible: the entrypoint runs this
	// binary to completion and then execs Praxis only if it succeeded, so a
	// conversion failure (which returns non-zero via log.Fatalf above) stops
	// the container instead of starting a proxy against a stale or absent
	// config.
	slog.Info("config generation complete; this binary does not proxy",
		"config", *praxisOut, "policy", *praxisPolicyOut)
	slog.Info("run Praxis against the generated config",
		"cmd", "praxis -c "+*praxisOut,
		"note", "requires a Praxis built with --features policy-engine when a policy was written")
}

// writePraxisConfig converts cfg to a Praxis configuration and writes it to
// outPath, along with the Praxis policy document at policyPath.
//
// The policy document is what lets the generated proxy actually enforce
// AuthBridge's inbound JWT validation: Praxis's `policy` filter reads it and
// denies requests without a valid token. It is written only when the AuthBridge
// pipeline declares something the policy engine can enforce — otherwise no file
// is created and the proxy config carries no `policy` filter, since a `policy`
// filter pointing at a nonexistent path fails Praxis startup.
//
// Note the generated `policy` filter requires Praxis to be built with the
// policy-engine cargo feature; a default-feature build rejects it as an unknown
// filter type.
func writePraxisConfig(cfg *config.Config, outPath, policyPath, audienceFile string) error {
	res, pol, err := praxis.ConvertWithPolicy(cfg, policyPath,
		&praxis.PolicyOptions{AudienceFile: audienceFile})
	if err != nil {
		return fmt.Errorf("converting config: %w", err)
	}

	// Write the policy first: the proxy config references it by path, and
	// Praxis fails to start if that path is missing. Writing the referrer
	// before the referent would leave a broken pair behind on a policy write
	// failure.
	if pol.Document != nil {
		policyData, err := praxis.RenderPolicyResult(pol, outPath)
		if err != nil {
			return fmt.Errorf("rendering policy: %w", err)
		}
		if err := writeFileAtomic(policyPath, policyData); err != nil {
			return fmt.Errorf("writing policy %q: %w", policyPath, err)
		}
		slog.Info("wrote Praxis policy",
			"path", policyPath,
			"enforces", pol.Enforced)
	} else {
		// Remove any policy from a previous run. Leaving it would strand a file
		// that nothing loads — the generated config carries no `policy` filter
		// in this branch — while the entrypoint's `[ -s "$PRAXIS_POLICY" ]`
		// check would find it and announce "policy engine will enforce it".
		// That is a false claim of enforcement, which is worse than no file.
		if err := os.Remove(policyPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing stale policy %q: %w", policyPath, err)
		} else if err == nil {
			slog.Warn("removed a stale Praxis policy from a previous run "+
				"(nothing in this config maps to a policy plugin)", "path", policyPath)
		}
		slog.Info("no Praxis policy written (nothing in the inbound pipeline maps to a policy plugin)",
			"path", policyPath)
	}

	data, err := praxis.RenderResult(res, outPath)
	if err != nil {
		return fmt.Errorf("rendering config: %w", err)
	}
	if err := writeFileAtomic(outPath, data); err != nil {
		return fmt.Errorf("writing %q: %w", outPath, err)
	}

	slog.Info("wrote Praxis config",
		"path", outPath,
		"listeners", len(res.Config.Listeners),
		"filterChains", len(res.Config.FilterChains),
		"unmappedPlugins", len(res.Unmapped))
	for _, u := range res.Unmapped {
		slog.Warn("AuthBridge plugin not represented in the generated Praxis config", "detail", u)
	}
	for _, w := range res.Warnings {
		slog.Warn("Praxis config translation note", "detail", w)
	}
	return nil
}

// writeFileAtomic writes data to path via a temporary file in the same
// directory, then renames it into place.
//
// A plain WriteFile truncates first, so an interrupted or partial write leaves a
// truncated document behind — and both of these files are consumed by another
// process. Praxis watches its config for changes and reloads on them, so it can
// observe a half-written file; on the policy side a truncated document is worse
// than a missing one, because it can parse into a policy that enforces less
// than intended. rename(2) within a directory is atomic, so a reader sees either
// the old file or the complete new one.
//
// 0o644: neither file carries secrets — they reference JWKS URLs, issuer and
// audience values, and credential/SVID paths, never the material itself — and
// Praxis may run as a different user than the generator.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("creating temp file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup on every failure path below; a successful rename
	// makes this a no-op.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temp file %q: %w", tmpName, err)
	}
	// fsync before rename: without it the rename can be durable while the
	// contents are not, which after a crash yields an empty file at the final
	// path — precisely the state the rename was meant to rule out.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing temp file %q: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file %q: %w", tmpName, err)
	}
	// CreateTemp makes the file 0o600; widen it before the rename so the final
	// file has its intended mode from the instant it becomes visible.
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("chmod temp file %q: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming %q to %q: %w", tmpName, path, err)
	}
	return nil
}
