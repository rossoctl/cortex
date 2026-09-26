package main

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/rossoctl/cortex/authlib/clientstate"
	"github.com/rossoctl/cortex/authlib/config"
	"github.com/rossoctl/cortex/authlib/tlsbridge"
)

// Everything Cortex writes for a user lives under ~/.cortex, so a laptop ends up
// with exactly one directory holding config, CA and keys rather than a CA
// scattered into whichever directory each command happened to run from.
const (
	cortexDirName = ".cortex"
	// localConfigName is the one config a local install has. There is deliberately
	// no second "cost-optimised" config: this one already carries both the parsers
	// and tool-prune, and writeBuiltinConfig preserves edits, so filling in
	// tool-prune's remove list is all the difference ever amounted to. Two configs
	// meant two CAs, two sets of paths, and two pages of instructions that read
	// identically.
	localConfigName = "config.yaml"
	// caDirName holds the bridge CA. Separate from the config so one directory
	// listing distinguishes "your settings" from "generated key material".
	caDirName = "ca"
	// localDirFallback is the directory --local used before the CA moved under
	// $HOME. It is no longer written to; it survives so main.go can spot a stale
	// one left in a working directory and warn that the client's trust anchor
	// needs updating.
	localDirFallback = "cortex-ca"
)

// defaultCortexDir returns ~/.cortex, or an error if there is no resolvable home
// directory.
//
// This used to be cwd-relative unconditionally, on the reasoning that no
// absolute path should be baked into the binary. Resolving $HOME at runtime
// satisfies that while keeping the private key in one predictable place — and
// the cwd default had a real cost: it dropped a CA and private key into
// whatever directory the proxy was started from, including checkouts.
//
// It returns an error rather than falling back to a relative path, because that
// fallback silently reintroduced exactly that bug: no $HOME meant the key landed
// in the working directory again, with nothing said about it. UserHomeDir only
// fails when $HOME is unset — a bare `env -i`, some systemd units, a scratch
// container — so failing loudly costs nothing anyone hits by accident, and
// --ca-dir remains available for those cases.
func defaultCortexDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("cannot determine your home directory (is $HOME set?); "+
			"pass --ca-dir to choose where the CA is written: %w", err)
	}
	return filepath.Join(home, cortexDirName), nil
}

// bridgeCACommonName is the subject every generated bridge CA carries
// (genSelfSignedCA). It is what makes one of our CAs recognisable as ours, and
// what makes two of them indistinguishable from each other.
const bridgeCACommonName = "authbridge-tls-bridge-ca"

// clientCAFromState returns the CA file the abctl-managed client is configured with
// right now, or "" when that cannot be established. The record's name and shape, and
// the reason `prior` is the wrong field to read, live in authlib/clientstate — shared
// with abctl, which writes the file.
func clientCAFromState(statePath string) string {
	st, err := clientstate.Load(statePath)
	if err != nil || st == nil {
		return ""
	}
	return st.CurrentCA()
}

// staleClientCAWarning reports a client pointed at a DIFFERENT bridge CA than the
// one now in force, returning slog args or nil when there is nothing to say.
//
// This is the sandbox / redirected-$HOME failure from issue #1033. defaultCortexDir
// resolves ca_dir from $HOME, so each $HOME gets its own generated CA — and since
// they all live at ~/.cortex/ca and all carry CN=authbridge-tls-bridge-ca, neither
// a log line nor a directory listing distinguishes them. A client still holding the
// other one rejects every forged leaf, and because Node reads its CA file once at
// process start, nothing recovers until that client restarts. The user sees only
// their agent's generic "self-signed certificate" error, which points at a
// corporate proxy rather than at this.
//
// Two properties make this safe to print on every boot, and both are load-bearing:
//
//   - It reads the client's CURRENT CA (clientCAFromState), not abctl's record of
//     what it displaced. The displaced value is a different CA by construction on
//     every enable, so comparing it warns about clients that are already correct.
//   - It compares the CERTIFICATES, not their paths. The configured file is often
//     not one of ours at all — behind a corporate proxy, routinely a system root
//     bundle — and a directory comparison both fired on that and would have advised
//     pointing --ca-dir at a system directory.
//
// Together they make the warning true when it fires and silent otherwise: it goes
// quiet the moment the client's configured file IS the CA in force (identical bytes
// → identical fingerprint), whatever paths either side is spelled with.
func staleClientCAWarning(currentCADir, clientCAPath string, currentCAPEM []byte) []any {
	if clientCAPath == "" || len(currentCAPEM) == 0 {
		return nil
	}
	client := parseBridgeCA(clientCAPath)
	current := parseBridgeCAPEM(currentCAPEM)
	// Only speak when both sides are certificates we recognise as our own. Anything
	// else — a corporate root, a bundle, an unreadable file, a CA we did not mint —
	// is not evidence of this failure.
	if client == nil || current == nil {
		return nil
	}
	if bytes.Equal(client.Raw, current.Raw) {
		return nil // the client already holds the CA in force
	}
	return []any{
		"client_ca", clientCAPath,
		"client_ca_fingerprint", tlsbridge.FingerprintSHA256(client),
		"now_using", currentCADir,
		"ca_fingerprint", tlsbridge.FingerprintSHA256(current),
		"why", "each $HOME gets its own generated CA and they share one name (CN=" + bridgeCACommonName + "), " +
			"so a client holding the other one rejects every forged leaf",
		// Both directions are offered because either is a legitimate resolution, and
		// which one is right depends on intent: point the client at this CA, or run
		// this proxy against the CA the client already trusts. Naming the client's
		// own configured directory keeps the advice consistent with what the client
		// is actually reading.
		"fix", "point the client's NODE_EXTRA_CA_CERTS at " + caTrustPath(currentCADir) +
			" and restart it, or run the proxy with --ca-dir " + filepath.Dir(clientCAPath),
	}
}

// parseBridgeCA reads path and returns the certificate only if it is one of our
// generated bridge CAs. Any failure answers nil: this drives a diagnostic, so an
// unreadable, non-PEM, or foreign certificate must produce silence rather than a
// claim. A file holding several certificates (a bundle) is deliberately not
// searched — our generated ca.crt holds exactly one, so a bundle here means the
// client was pointed at something else.
func parseBridgeCA(path string) *x509.Certificate {
	b, err := os.ReadFile(path) //nolint:gosec // path comes from abctl's own record
	if err != nil {
		return nil
	}
	return parseBridgeCAPEM(b)
}

// parseBridgeCAPEM is parseBridgeCA on bytes already in hand.
func parseBridgeCAPEM(pemBytes []byte) *x509.Certificate {
	blk, _ := pem.Decode(pemBytes)
	if blk == nil {
		return nil
	}
	crt, err := x509.ParseCertificate(blk.Bytes)
	if err != nil || crt.Subject.CommonName != bridgeCACommonName {
		return nil
	}
	return crt
}

// builtinConfigYAML returns the built-in --local config with caDir interpolated: a
// forward-only proxy with the TLS bridge on (auto-generated CA in caDir) and
// the LLM / MCP / A2A parsers, so an agent's egress is decrypted and parsed.
// Kept in sync with the root README.
//
// Every listener is pinned to loopback on an uncommon port. This runs on a
// laptop, so (a) a wildcard bind would expose an open forward proxy, the stats
// endpoint, the health endpoint, and the unauthenticated session API (which
// carries decrypted bodies and any injected tokens) to the LAN, and (b) the
// usual 8081/909x ports collide with common dev tools. The preset only fills
// empty addresses, so these explicit values win — keep them in sync with the
// ports the installer probes and prints (install.sh). The
// enforce-redirect transparent listener isn't used here (no iptables); --local
// skips it, and it is pinned anyway so that starting this same file with
// --config cannot bind it on every interface.
//
// The YAML body is flush-left on purpose — a raw string literal preserves
// leading whitespace, so indenting these lines in source would corrupt the YAML.
func builtinConfigYAML(caDir string) string {
	return `# Built-in config for: authbridge-proxy --local
# Forward-only proxy + TLS bridge (auto-generated CA) + LLM/MCP/A2A parsers.
# The running proxy watches this file — edit it to hot-reload.
mode: proxy-sidecar
listener:
  roles: [forward]
  # Every listener binds 127.0.0.1, including any this file does not name. The
  # explicit ports below pick a non-colliding range; this line is what makes an
  # unnamed or newly added listener safe on a laptop, where a wildcard bind means
  # the Wi-Fi.
  bind_loopback_only: true
  forward_proxy_addr: 127.0.0.1:47600
  session_api_addr: 127.0.0.1:47601
  # Without this the preset defaults health to ":9091" — every interface, and a
  # port common enough to collide with an unrelated service.
  health_addr: 127.0.0.1:47604
  # --local skips the enforce-redirect transparent listener, but --config does
  # not, and the troubleshooting docs tell people to start this same file with
  # --config. Unpinned it would then bind ":8082" on every interface. Pinning it
  # makes the config safe however it is launched.
  transparent_proxy_addr: 127.0.0.1:47603
stats:
  address: 127.0.0.1:47602
tls_bridge:
  mode: enabled
  ca_dir: "` + caDir + `"
  generate_ca: true
  # passthrough_hosts is deliberately absent. Left unset, the bridge tunnels a
  # built-in list of developer-tooling hosts — GitHub, the Go module proxy, the
  # package registries (tlsbridge.DefaultPassthroughHosts) — so gh, go, pip and
  # npm keep working without being told to trust anything.
  #
  # That is not a compromise: no plugin can read that traffic anyway. The parsers
  # and tool-prune act on agent<->LLM and agent<->tool messages, so forging a leaf
  # for a module download buys nothing and breaks every Go tool, which on macOS
  # cannot be pointed at a CA file by environment at all.
  #
  # Setting the key here REPLACES that list rather than adding to it, and an
  # explicit empty list means "intercept everything". Never list an inference
  # endpoint: skipping one silently removes the parsing and the token savings,
  # with no error to notice it by.
# Cost accounting. Nothing here is required.
#
# Rates come from a table built into the binary -- vendor list, refreshed each
# release -- scaled by any gateway discount Cortex knows about. To see what is
# actually in effect, including where each figure came from:
#
#   abctl pricing --host <your-gateway>
#
# Gateways matching a shipped rule already have their discount applied and need
# nothing here. If yours is not one of them and it bills a fraction of list, say
# so once: one scalar tracks upstream repricing, where a copied rate card freezes
# today's numbers and goes stale with nothing to say it has.
#
# pricing:
#   endpoints:
#     - hosts: ["my-gateway.example.com"]
#       multiplier: 0.80          # a FRACTION of list, so 0.80 is a 20% discount
#
# For a gateway whose prices are genuinely negotiated per model rather than
# derived from list, give rates instead of a multiplier -- see
# docs/pricing.md.
#
# Cost history on disk. Per-minute totals only -- no prompts, no completions,
# no tool arguments -- under ~/.cortex/cost/YYYY-MM-DD.jsonl, kept 31 days
# (roughly 10 MB). The in-memory counters are a 6-hour ring and this proxy
# restarts several times a day, so without this "what did today cost" answers
# over whatever is left in that ring, and window=7d and window=month cannot be
# answered at all.
#
# WRITTEN OUT RATHER THAN LEFT TO THE DEFAULT, which is belt-and-braces now
# rather than the mechanism. The default used to be "on for --local, off
# otherwise", and since "abctl service install" runs this file with --config
# and never --local, every INSTALLED laptop had the ledger off while the docs
# promised it was on. The default is no longer keyed on the flag at all: it is
# on wherever the ledger can survive a restart, which means an explicit
# cost_ledger.dir or a config inside ~/.cortex -- this file's own location, and
# what both an installed service and --local pass. See ledgerDefaultOn. This
# line therefore states what would happen anyway, and keeps saying it if that
# rule ever changes.
#
# Set enabled: false to turn it off. Restart-only, not hot-reloaded: the ledger
# is opened once at startup, so an edit here is REFUSED by the reloader (the
# whole save with it) rather than accepted and ignored. "abctl service restart"
# applies it, and cuts attached Claude Code sessions.
cost_ledger:
  enabled: true
  # dir: /absolute/path        # default ~/.cortex/cost; a RELATIVE path is refused
  # retention_days: 31         # minimum 9; 31 is the default and what window=month needs
  #                             # -- a month-to-date total on the 31st of a 31-day month
  #                             # opens 31 day files. A shorter setting is disclosed: the
  #                             # band marks such a total as a floor and abctl cost
  #                             # prints a coverage line. window=7d can open nine.
# Session bucketing: which request headers are used to group traffic into
# sessions visible in abctl. Each listed header is tried in order; the first
# non-empty value wins.
#
# X-Claude-Code-Session-Id is set by Claude Code on every inference request.
# X-Session-Id is set by OpenCode, Pi (Inflection AI), and similar frameworks.
#
# An explicit empty list (id_headers: []) disables header-based bucketing.
session:
  id_headers:
    - "X-Claude-Code-Session-Id"
    - "X-Session-Id"
pipeline:
  outbound:
    plugins:
      - name: inference-parser
      - name: mcp-parser
      - name: a2a-parser
      # tool-prune drops unused tool definitions from the outbound manifest.
      # The empty remove list is the off switch: with nothing named it does
      # nothing at all. Fill it in and it takes effect immediately --
      #   abctl tools scan --write <this file>
      # -- and the config is hot-reloaded, so no restart.
      #
      # Watch the Metrics section of abctl's plugin pane for what it saved. If
      # you ever suspect the plugin of breaking a request, set
      # on_error: observe here: it then counts what it *would* remove while
      # leaving every byte on the wire untouched, which settles the question
      # without unconfiguring anything.
      #
      # Keep it last: it rewrites the request body, and body readers must
      # precede the mutator so they see the original bytes.
      - name: tool-prune
        on_error: enforce
        config:
          remove: []
`
}

// writeBuiltinConfig ensures the built-in --local config exists next to the CA (in
// caDir) and returns its path, so --local reuses the normal file-based load +
// hot-reload path. caDir is caller-resolved (cwd-relative by default, or
// --ca-dir); no absolute path is baked into the binary.
//
// An existing file is KEPT, not overwritten. The config's own header invites
// editing it, and `abctl tools scan --write` writes a prune list into it — and
// this function runs before any port is bound, so an unconditional write meant
// that even a --local start which then failed on a port clash silently destroyed
// those edits. Delete the file to regenerate the preset.
// writeBuiltinConfig ensures cortexDir/config.yaml exists, pointing at caDir for
// the CA, and returns its path.
//
// An existing file is never rewritten. That is what makes this the only config a
// local install needs: the prune list, the on_error policy and any hand edit all
// survive a restart, so there is nothing for a second "persistent" config to do.
func writeBuiltinConfig(cortexDir, caDir string) (string, error) {
	// 0700: caDir under here holds the CA's private key.
	if err := os.MkdirAll(cortexDir, 0o700); err != nil {
		return "", err
	}
	// MkdirAll leaves an existing directory's mode alone, so a ~/.cortex created
	// before this (or by another tool) would stay 0755 and the perms claim would
	// be true only for fresh installs. install.sh already chmods it; this makes a
	// bare `authbridge-proxy --local` match.
	if err := os.Chmod(cortexDir, 0o700); err != nil {
		return "", err
	}
	// Same reasoning one level down. tlsbridge creates caDir 0700, but MkdirAll does
	// not tighten an existing directory, so a caDir left at 0755 by an earlier build
	// stays 0755 while holding the CA's private key. The key file itself is 0600, so
	// this is defence in depth rather than the only guard — but it should not be the
	// parent's mode alone standing between a MITM CA key and every process on the
	// machine. Only when it already exists: letting tlsbridge create it keeps the
	// Kubernetes case (a mounted ca_dir, deliberately left alone) untouched.
	if fi, err := os.Stat(caDir); err == nil && fi.IsDir() {
		if err := os.Chmod(caDir, 0o700); err != nil {
			return "", err
		}
	}
	path := filepath.Join(cortexDir, localConfigName)
	if _, err := os.Stat(path); err == nil {
		slog.Info("local mode — keeping the existing config (edits and any prune list are preserved)",
			"path", path, "hint", "delete it to regenerate the built-in preset")
		return path, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	// 0600, not 0644: it names the CA's location and the whole listener layout, and
	// abctl's migration already rewrites it 0600 — a fresh install should not be the
	// looser of the two.
	if err := os.WriteFile(path, []byte(builtinConfigYAML(caDir)), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// costLedgerDirName is the ledger's directory under ~/.cortex, beside the config
// and the CA — one place a user looks for everything Cortex wrote.
const costLedgerDirName = "cost"

// costLedgerDir returns where the durable cost ledger writes its day files.
//
// cost_ledger.dir wins when set, so an operator can put the files on a different
// volume. Otherwise it is ~/.cortex/cost, derived at runtime rather than stored in
// the config: a $HOME-derived absolute path written into a file that gets copied
// between machines is a path that silently points at someone else's home.
//
// Returns an error rather than falling back to the working directory, for the reason
// defaultCortexDir does: a cwd fallback drops spend records into whatever directory
// the proxy happened to start from, including checkouts, with nothing said about it.
//
// Which is why a RELATIVE cost_ledger.dir is refused rather than resolved. It used to
// be returned verbatim, so `dir: cost` produced exactly the failure the paragraph
// above says this function refuses — resolved against the proxy's working directory,
// which is not a property of the config and differs between a launchd job, a
// container, and a shell in a checkout. The same setting would scatter day files
// across all three, and a query would open one of them and report the others' spend
// as absent. The comment was making a promise the code did not keep; the code keeps
// it now.
//
// Cleaned on the way out so `/var/lib/cortex/cost/` and `/var/lib//cortex/cost` name
// one directory rather than three, which matters because the writer's per-day file
// locking is keyed on the path.
func costLedgerDir(cfg *config.Config) (string, error) {
	if cfg != nil && cfg.CostLedger != nil && cfg.CostLedger.Dir != "" {
		dir := cfg.CostLedger.Dir
		if !filepath.IsAbs(dir) {
			return "", fmt.Errorf("cost_ledger.dir must be an absolute path, got %q: a relative path "+
				"resolves against the proxy's working directory, which is not a property of the config — "+
				"a launchd job, a container and a shell in a checkout would each write a different %q, "+
				"and a query would report the others' spend as absent", dir, dir)
		}
		return filepath.Clean(dir), nil
	}
	dir, err := defaultCortexDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, costLedgerDirName), nil
}
