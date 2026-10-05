package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/rossoctl/cortex/core/clientstate"
	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/tlsbridge"
)

// The variables Claude Code needs to route through Cortex. Claude Code
// reads env vars from its own settings file, which is not merely more convenient
// than exporting them in a shell — it is more correct. The supervisor is one
// process shared by every terminal and inherits the environment of whichever
// shell cold-started it, so a shell export reaches background agents only by
// luck. Settings reach every session on the machine.
// exitDeclined is returned when the user said no, or there was no terminal to
// ask on. Separate from 1 so a caller can tell a refusal — which is a normal
// outcome — from an operational failure it must not report as success.
const exitDeclined = 3

const (
	envProxy   = "HTTPS_PROXY"
	envCACerts = "NODE_EXTRA_CA_CERTS"
	// The CA variables below take a bundle, not ca.crt. Unlike Node's, each of
	// these REPLACES the process's trust store with the file named, so pointing
	// them at the bare CA leaves the tool trusting one private CA and nothing
	// else — every unproxied TLS call then fails. Go is explicit about it:
	// crypto/x509 root_unix.go does `files = []string{f}` when SSL_CERT_FILE is
	// set.
	//
	// HTTPS_PROXY reaches these tools whether or not they were the target — a
	// Go or Python program spawned by Claude Code inherits the proxy and must be
	// able to verify the bridge's forged leaf.
	//
	// SSL_CERT_FILE DOES NOTHING ON macOS. root_unix.go, which honours it, is
	// built for `linux || freebsd || …` and excludes darwin; darwin's
	// loadSystemRoots returns a `systemPool: true` sentinel that reads no files
	// at all, and verify.go then routes any program that has not set RootCAs
	// straight to systemVerify — Security.framework, keychain only. So on macOS
	// a Go tool cannot be pointed at a CA file by environment; the CA has to go
	// into the keychain instead (see darwinGoNote). It is still written here
	// because it is correct and necessary everywhere else, including CI.
	//
	// The other three are unaffected: git, curl and Python read their bundles
	// through OpenSSL/LibreSSL, which honours these variables on macOS too.
	envSSLCert    = "SSL_CERT_FILE"      // Go: gh, agentop, any Go CLI — Linux only, see above
	envGitCA      = "GIT_SSL_CAINFO"     // git (incl. the fetches `go mod` makes)
	envRequestsCA = "REQUESTS_CA_BUNDLE" // Python requests: keycloak_sync, setup scripts
	envCurlCA     = "CURL_CA_BUNDLE"     // curl
	envNoTelem    = "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"
	settingsRel   = ".claude/settings.json"
	cortexCfgRel  = ".cortex/config.yaml"
	// stateRel records what each managed key looked like BEFORE enable, so disable
	// can put it back. Without it, disable deleted every managed key it found —
	// including one the user had set themselves, which is indistinguishable by
	// value (their CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 is byte-identical to
	// ours). Kept outside ~/.claude so this command's bookkeeping never appears in
	// a file Claude Code owns.
	stateRel = ".cortex/" + clientstate.RelPath
)

// managedState is the ownership record. A nil entry means the key was absent
// before enable, so disable deletes it; a non-nil entry is the value to restore.
// managedState is core/clientstate.State: the shape is shared with
// cortex, which reads this file back to check the client is still pointed
// at the CA in force. Aliased rather than redeclared so a field rename cannot leave
// the reader silently returning nothing.
type managedState = clientstate.State

// readState distinguishes "no record" from "record unreadable".
//
// Collapsing them was a silent hole: disable treats a missing record as
// "enabled by an older agentop" and falls back to deleting every managed key, so a
// truncated or hand-mangled state file re-opened exactly the data loss the record
// exists to prevent — a corrupt record looked identical to no record. A nil
// state with a nil error means genuinely absent; a non-nil error means the record
// was there and could not be trusted, which the caller must say out loud.
func readState(path string) (*managedState, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var st managedState
	if uerr := json.Unmarshal(b, &st); uerr != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", path, uerr)
	}
	if st.Prior == nil {
		return nil, fmt.Errorf("%s has no prior-value record", path)
	}
	return &st, nil
}

// writeState records ownership on the FIRST enable only. A second enable must not
// overwrite it with our own values, or the original would be lost exactly when it
// is needed.
func writeState(path string, st managedState) error {
	// An unreadable existing record is not a reason to overwrite it: if it can be
	// repaired by hand it is still the only copy of what the user had.
	existing, err := readState(path)
	if err != nil {
		return fmt.Errorf("refusing to overwrite the existing record: %w", err)
	}
	if existing != nil && existing.Settings == st.Settings {
		return nil
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// managedKeys is exactly what enable writes and disable removes. Nothing else in
// the file is touched — notably not ANTHROPIC_BASE_URL or any auth token, which
// commonly live in the same env block.
var managedKeys = []string{
	envProxy, envCACerts, envSSLCert, envGitCA, envRequestsCA, envCurlCA, envNoTelem,
}

// bundleKeys are the managed keys that must point at the CA+roots bundle rather
// than at ca.crt. Kept as its own list so wanted() and the existence check
// cannot disagree about which variables carry which file — getting one of these
// pointed at ca.crt is the exact bug this grouping prevents.
var bundleKeys = []string{envSSLCert, envGitCA, envRequestsCA, envCurlCA}

// darwinGoNote is printed on macOS, where SSL_CERT_FILE is inert: Go resolves
// roots through Security.framework and reads no CA file, so no environment
// variable can make `go`, `gh` or any other Go tool trust the bridge. Only the
// keychain can. git, curl and Python are unaffected — they read their bundles
// through OpenSSL/LibreSSL, which honours the variables on macOS.
//
// Said at enable time rather than left to documentation because the failure it
// predicts is a bare "x509: certificate signed by unknown authority" from a tool
// the user has just been told is configured — the same "no error points at the
// cause" problem this command exists to remove.
//
// Said only when enable is about to change the settings, not on the "Already
// enabled" re-run that install.sh --claude-code makes on every upgrade: repeated
// there, a note about a case that needs nothing doing was most of the upgrade's
// output. Short for the same reason; docs/laptop-service.md ("Go tools on macOS
// need the keychain") carries the why.
func darwinGoNote(caPath string) string {
	return "Note: SSL_CERT_FILE is inert on macOS; Go tools (go, gh) use the keychain only.\n" +
		"  Nothing to do by default: Cortex tunnels GitHub, the Go module proxy and the\n" +
		"  package registries unread. Only if you bridge a host a Go tool talks to, run:\n" +
		"    security add-trusted-cert -k ~/Library/Keychains/login.keychain-db -p ssl \\\n" +
		"      " + caPath + "\n" +
		"  (undo: security delete-certificate -c authbridge-tls-bridge-ca \\\n" +
		"     ~/Library/Keychains/login.keychain-db)\n\n"
}

const claudeCodeUsage = `agentop configure claude-code — route Claude Code through Cortex without shell env vars

Usage:
  agentop configure claude-code enable  [--yes] [--settings PATH] [--config PATH]
  agentop configure claude-code disable [--yes] [--settings PATH]
  agentop configure claude-code status  [--settings PATH]

enable writes HTTPS_PROXY, CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC and a set of
CA variables into the "env" block of ~/.claude/settings.json, reading the
addresses from ~/.cortex/config.yaml so they always match the running proxy.
Afterwards, plain "claude" goes through Cortex.

The CA variables cover the tools Claude Code spawns, not just Claude Code
itself: anything it runs inherits HTTPS_PROXY and so must be able to verify the
bridge. NODE_EXTRA_CA_CERTS (Node) gets ca.crt because it EXTENDS the trust
store; SSL_CERT_FILE (go, gh), GIT_SSL_CAINFO (git), REQUESTS_CA_BUNDLE (Python)
and CURL_CA_BUNDLE (curl) get bundle.crt, because each REPLACES the trust store
and ca.crt alone would leave them trusting one private CA and nothing else.

macOS caveat: SSL_CERT_FILE has no effect there. Go resolves roots through the
keychain and reads no CA file, so no environment variable can make go or gh
trust the bridge; enable prints the "security add-trusted-cert" command that
does. git, curl and Python are unaffected on macOS.

Only those keys are added; every other setting, including any other env entry,
is left exactly as it was. The first run copies the original file to
settings.json.bak and never overwrites that copy, so the pristine version
survives later runs. disable removes only the keys it added, restoring any
prior value it recorded.

Note: while enabled, Claude Code needs Cortex running — its requests go to the
proxy address. "agentop configure claude-code disable" is the off switch.

Exit status: 0 applied or already correct, 3 declined (or no terminal to ask
on), 1 something went wrong.

Flags:
  --yes           do not prompt for confirmation
  --settings PATH Claude Code settings file (default ~/.claude/settings.json)
  --config PATH   Cortex config to read addresses from (default ~/.cortex/config.yaml)
`

func runClaudeCode(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, claudeCodeUsage)
		return 2
	}
	action := args[0]
	// `agentop claude-code --help` used to be read as an action name and fall through to
	// the unknown-action branch below, which sends someone looking for the command
	// list to the one place that refuses to print it. Same fix, and same reason, as
	// `agentop service --help`.
	//
	// Answered before the flag set is built rather than through fs.Usage: --help asks
	// for the whole command's usage, and a flag set named "claude-code --help" would
	// print only the flags of an action that does not exist.
	//
	// Explicit help goes to stdout with exit 0; a missing or bad action keeps going to
	// stderr with exit 2. That split is what makes `--help` pipeable.
	switch action {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, claudeCodeUsage)
		return 0
	}

	fs := flag.NewFlagSet("claude-code "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	yes := fs.Bool("yes", false, "do not prompt for confirmation")
	settingsPath := fs.String("settings", "", "Claude Code settings file")
	cortexCfg := fs.String("config", "", "Cortex config file")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		fmt.Fprintf(stderr, "agentop: cannot determine your home directory: %v\n", err)
		return 1
	}
	if *settingsPath == "" {
		*settingsPath = filepath.Join(home, settingsRel)
	}
	if *cortexCfg == "" {
		*cortexCfg = filepath.Join(home, cortexCfgRel)
	}
	statePath := filepath.Join(home, stateRel)

	switch action {
	case "enable":
		return claudeCodeEnable2(*settingsPath, *cortexCfg, statePath, *yes, stdout, stderr)
	case "disable":
		return claudeCodeDisable2(*settingsPath, statePath, *cortexCfg, *yes, stdout, stderr)
	case "status":
		return claudeCodeStatus(*settingsPath, stdout)
	default:
		fmt.Fprintf(stderr, "agentop: unknown claude-code action %q (enable, disable, status)\n", action)
		return 2
	}
}

// wanted derives the three values from the Cortex config, so they cannot drift
// from the proxy that is actually running. Hardcoding 47600 here would silently
// point Claude Code at nothing the moment someone edited their config.
func wanted(cortexCfgPath string) (map[string]string, error) {
	out, _, err := wantedFromConfig(cortexCfgPath)
	return out, err
}

// bridgeEnabled reports whether cfg has an enabled TLS bridge.
//
// Empty Mode means disabled, per TLSBridgeConfig.Mode's own documentation, and a
// nil TLSBridge means the block is absent entirely.
func bridgeEnabled(cfg *config.Config) bool {
	return cfg != nil && cfg.TLSBridge != nil && cfg.TLSBridge.Mode == "enabled"
}

// errBridgeDisabled is the shared refusal for a config whose bridge is off.
//
// Shared, because `agentop exec` and `agentop claude-code enable` must agree about the
// bridge posture as well as the addresses. ca_dir is only *required* when
// mode is "enabled" (config.Validate), so `mode: disabled` with a ca_dir set is
// valid config that both commands used to accept — writing a CA for a bridge that
// terminates nothing, so every https request fails verification against the real
// upstream certificate. exec grew the check first; hoisting it here is what makes
// "the two cannot drift" true of the posture too, not only the proxy and CA paths.
// source names where the config came from — a file path for `claude-code enable`,
// a stats URL for `agentop exec` — so the message points at the thing the reader can
// actually go and change.
func errBridgeDisabled(source string) error {
	return fmt.Errorf("%s has no enabled TLS bridge (tls_bridge.mode must be \"enabled\");\n"+
		"  without it Cortex terminates no TLS, so there is nothing for a client to\n"+
		"  trust and every https request would fail verification. Enable the TLS bridge first",
		source)
}

// wantedFromConfig is wanted plus the loaded config, so a caller needing more than
// the three values does not parse the file twice.
func wantedFromConfig(cortexCfgPath string) (map[string]string, *config.Config, error) {
	cfg, err := config.Load(cortexCfgPath)
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", cortexCfgPath, err)
	}
	out, err := wantedFromLoaded(cfg, cortexCfgPath)
	return out, cfg, err
}

// wantedFromLoaded is the derivation itself, over a config that is already in hand.
//
// Split out so `agentop exec` can feed it the config it fetched from the RUNNING
// proxy while `claude-code enable` feeds it one read from disk. One derivation, two
// sources: the values the two commands produce for the same Cortex cannot drift,
// which is the property both rely on.
// source names where cfg came from — a file path from wantedFromConfig, a stats
// URL from execEnv — so a refusal points at the thing the reader can go and change.
// errBridgeDisabled already took this parameter for exactly that reason; this
// applies the same reasoning to the forward_proxy_addr refusal, which had lost the
// path when the derivation became memstore.
func wantedFromLoaded(cfg *config.Config, source string) (map[string]string, error) {
	addr := cfg.Listener.ForwardProxyAddr
	if addr == "" {
		return nil, fmt.Errorf("%s has no listener.forward_proxy_addr; there is no forward proxy to point at", source)
	}
	// A bind address is not a URL: ":8081" and "127.0.0.1:47600" both need a host
	// a client can actually dial.
	//
	// net.SplitHostPort, not strings.Cut: Cut splits at the FIRST colon, so
	// "[::1]:47600" gave host="[" and port=":1]:47600" and this wrote a malformed
	// http://[:1]:47600 into settings.json — a broken value rather than an error,
	// in the file whose misconfiguration is the silent failure everything else
	// here works to make loud. SplitHostPort understands the bracketed form and
	// errors on genuinely bad input.
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("listener.forward_proxy_addr %q is not host:port: %w", addr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	out := map[string]string{
		// JoinHostPort, not concatenation: an IPv6 literal must keep its brackets
		// to be a valid URL authority.
		envProxy:   "http://" + net.JoinHostPort(host, port),
		envNoTelem: "1",
	}
	// Nil-checked: tls_bridge is an omitempty pointer, so a config without the
	// block at all leaves it nil and dereferencing it segfaulted — `agentop
	// claude-code enable` crashed with a stack trace on a perfectly valid config
	// whose only fault was having no TLS bridge, which is exactly the case the
	// caller below is written to report cleanly.
	if cfg.TLSBridge != nil && cfg.TLSBridge.CADir != "" {
		ca, aerr := filepath.Abs(filepath.Join(cfg.TLSBridge.CADir, "ca.crt"))
		if aerr != nil {
			return nil, aerr
		}
		out[envCACerts] = ca
		// Everything else gets the bundle, never ca.crt — see bundleKeys.
		bundle, berr := filepath.Abs(filepath.Join(cfg.TLSBridge.CADir, tlsbridge.TrustBundleName))
		if berr != nil {
			return nil, berr
		}
		for _, k := range bundleKeys {
			out[k] = bundle
		}
	}
	return out, nil
}

// claudeCodePlan is what enable would change, worked out without touching anything.
//
// Split from the command so `agentop setup` can show the change on its own consent
// screen and apply it after one question, and so the checks that refuse — a value
// someone else set, a bridge that is off — run before anything else on the machine
// has changed.
type claudeCodePlan struct {
	settingsPath string
	doc          map[string]any    // the settings file as read; apply mutates it
	env          map[string]string // its env block before the change
	want         map[string]string // the managed values enable writes
	changes      []string          // "  KEY=VALUE" per value that differs; none = already enabled
}

// planClaudeCodeEnable runs the checks that can refuse an enable, in the order enable
// made them. An error is the refusal, worded to follow "agentop: ".
func planClaudeCodeEnable(settingsPath, cortexCfgPath string) (claudeCodePlan, error) {
	want, cfg, err := wantedFromConfig(cortexCfgPath)
	if err != nil {
		return claudeCodePlan{}, err
	}
	// Same bridge-posture gate `agentop exec` applies. Without it, `mode: disabled`
	// with a ca_dir set was written into settings.json and produced exactly the
	// silent break the ca_dir check below exists to prevent.
	if !bridgeEnabled(cfg) {
		return claudeCodePlan{}, errBridgeDisabled(cortexCfgPath)
	}
	if _, ok := want[envCACerts]; !ok {
		return claudeCodePlan{}, fmt.Errorf("%s has no tls_bridge.ca_dir, so Claude Code has no CA to trust;\n"+
			"  requests would fail certificate verification. Enable the TLS bridge first.", cortexCfgPath)
	}

	doc, err := readSettings(settingsPath)
	if err != nil {
		return claudeCodePlan{}, err
	}
	env := envStrings(doc)

	// Refuse to overwrite a value the user set to something else — most likely a
	// corporate proxy. Silently replacing it would break their network access and
	// give no clue why.
	for _, k := range managedKeys {
		if cur, ok := env[k]; ok && cur != want[k] && !isCortexValue(k, cur) {
			return claudeCodePlan{}, fmt.Errorf("%s is already set to %q in %s.\n"+
				"  Refusing to overwrite a value you set. Remove it first, or edit the file by hand.",
				k, cur, settingsPath)
		}
	}

	pl := claudeCodePlan{settingsPath: settingsPath, doc: doc, env: env, want: want}
	for _, k := range managedKeys {
		if env[k] != want[k] {
			pl.changes = append(pl.changes, fmt.Sprintf("  %s=%s", k, want[k]))
		}
	}
	return pl, nil
}

// printTrustFileNotes warns about CA files that do not exist yet. Command output
// only, kept out of plan and apply: the proxy writes these files on start (the
// bundle only where it finds a system root store), so a caller that applies before
// the proxy has started should check them after that start, not warn before it.
func printTrustFileNotes(want map[string]string, stdout io.Writer) {
	// Both trust files are reported only when a ca_dir was configured at all.
	// wanted() populates these keys under `if cfg.TLSBridge.CADir != ""`, so
	// without one the paths are "" and an unguarded Stat printed
	// "Note:  does not exist yet." with a blank path — a note about no file.
	if want[envCACerts] == "" {
		return
	}
	// The CA path is written whether or not the file exists, because enabling
	// before the first start is legitimate — the proxy generates it on boot. But
	// a NODE_EXTRA_CA_CERTS pointing at a missing file fails SILENTLY: requests
	// keep working, every one tunnels through opaquely, and nothing is parsed.
	// Say so now rather than let that be discovered later.
	if _, serr := os.Stat(want[envCACerts]); serr != nil {
		fmt.Fprintf(stdout, "Note: %s does not exist yet.\n"+
			"  Cortex creates it on first start. Until then Claude Code cannot verify the\n"+
			"  bridge and every request tunnels through unparsed — which looks like nothing\n"+
			"  is wrong. Start Cortex, then check with: agentop configure claude-code status\n\n",
			want[envCACerts])
	}
	// The bundle is checked separately: it is written by a LATER step than
	// ca.crt (the proxy assembles it from the CA plus the platform roots), and
	// on a host where no root store could be located it never appears at all. A
	// tool whose CA variable points at a missing file fails closed with a
	// certificate error rather than silently, so say which tools are affected.
	if _, serr := os.Stat(want[envSSLCert]); serr != nil {
		fmt.Fprintf(stdout, "Note: %s does not exist yet.\n"+
			"  Cortex assembles it on start from the CA plus this machine's root store;\n"+
			"  git, curl and Python read it. If it is still missing after a start, Cortex\n"+
			"  could not find a system root bundle — check the proxy log for \"trust bundle\".\n\n",
			want[envSSLCert])
	}
}

// applyClaudeCodeEnable records what the managed keys held, then writes the plan's
// values. Failing to record is a warning on stderr, as it always was: the write still
// happens, and disable then deletes the keys rather than restoring them.
func applyClaudeCodeEnable(pl claudeCodePlan, statePath string, stderr io.Writer) error {
	// Record what was there before, so disable restores rather than deletes.
	if statePath != "" {
		st := managedState{Settings: pl.settingsPath, Prior: map[string]*string{}}
		for _, k := range managedKeys {
			if v, ok := pl.env[k]; ok {
				vv := v
				st.Prior[k] = &vv
			} else {
				st.Prior[k] = nil
			}
		}
		if werr := writeState(statePath, st); werr != nil {
			fmt.Fprintf(stderr, "agentop: could not record prior settings (%v); disable will delete\n"+
				"  these keys rather than restore any you had set yourself\n", werr)
		}
	}

	raw := envRaw(pl.doc)
	for _, k := range managedKeys {
		raw[k] = pl.want[k]
	}
	return writeSettings(pl.settingsPath, pl.doc)
}

func claudeCodeEnable2(settingsPath, cortexCfgPath, statePath string, yes bool, stdout, stderr io.Writer) int {
	pl, err := planClaudeCodeEnable(settingsPath, cortexCfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentop: %v\n", err)
		return 1
	}
	printTrustFileNotes(pl.want, stdout)
	if len(pl.changes) == 0 {
		fmt.Fprintf(stdout, "Already enabled: %s routes Claude Code through Cortex.\n", settingsPath)
		return 0
	}
	// After the no-op return, unlike the two notes above: those report a file that is
	// missing now, this one a platform fact the user needs once — see darwinGoNote.
	if runtime.GOOS == "darwin" && pl.want[envCACerts] != "" {
		fmt.Fprint(stdout, darwinGoNote(pl.want[envCACerts]))
	}

	// Three short lines, not three paragraphs. This is a confirmation prompt, so it
	// needs to say what changes and that the file is backed up; the rest (how to undo
	// it, that `claude` needs no env vars afterwards) belongs in the closing summary,
	// where it was also being printed.
	fmt.Fprintf(stdout, "Adds to the \"env\" block of %s:\n%s\n",
		settingsPath, strings.Join(pl.changes, "\n"))
	fmt.Fprintf(stdout, "Nothing else in the file changes; a copy is kept as %s.bak\n\n", settingsPath)
	if !yes && !claudeCodeConfirm(stdout) {
		fmt.Fprintln(stdout, "Not changed.")
		return exitDeclined
	}

	if err := applyClaudeCodeEnable(pl, statePath, stderr); err != nil {
		fmt.Fprintf(stderr, "agentop: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "Enabled — run `claude` as usual.")
	return 0
}

// claudeCodeDisablePlan is what disable would remove, worked out without touching
// anything.
type claudeCodeDisablePlan struct {
	settingsPath string
	doc          map[string]any
	present      []string          // managed keys set in the file, in managedKeys order; none = nothing to do
	want         map[string]string // managed values enable writes; nil when the Cortex config is unknown
}

func planClaudeCodeDisable(settingsPath, cortexCfgPath string) (claudeCodeDisablePlan, error) {
	doc, err := readSettings(settingsPath)
	if err != nil {
		return claudeCodeDisablePlan{}, err
	}
	env := envStrings(doc)
	pl := claudeCodeDisablePlan{settingsPath: settingsPath, doc: doc}
	if cortexCfgPath != "" {
		if want, werr := wanted(cortexCfgPath); werr == nil {
			pl.want = want
		}
	}
	for _, k := range managedKeys {
		if _, ok := env[k]; ok {
			pl.present = append(pl.present, k)
		}
	}
	return pl, nil
}

// applyClaudeCodeDisable puts back what enable recorded, removes what it added, and
// deletes the record. It returns the keys restored to a value the user had set.
//
// With none of the keys present it touches nothing, so a caller need not check
// first. Going on would create a settings.json where there was none, rewrite one
// that holds none of the keys (and back it up), and delete the record.
func applyClaudeCodeDisable(pl claudeCodeDisablePlan, statePath string, stderr io.Writer) ([]string, error) {
	if len(pl.present) == 0 {
		return nil, nil
	}
	st, sterr := readState(statePath)
	if sterr != nil {
		// Proceed — the user asked for this off — but say what is about to be lost.
		// Silence here would repeat the bug the record was added to fix.
		fmt.Fprintf(stderr, "agentop: cannot read the record of what you had before enabling (%v).\n"+
			"  Falling back to removing these keys outright. If you had set any of them\n"+
			"  yourself before running enable, that value is not recoverable from here —\n"+
			"  check %s afterwards.\n\n", sterr, pl.settingsPath)
	}
	raw := envRaw(pl.doc)
	var restored []string
	for _, k := range pl.present {
		if st != nil && st.Settings == pl.settingsPath {
			if prior, recorded := st.Prior[k]; recorded {
				// If the current value is not what enable wrote, the user edited
				// it by hand after enable. Keep their edit instead of silently
				// restoring the pre-enable value (#1289).
				if cur, isStr := raw[k].(string); isStr && pl.want != nil {
					if wrote, ok := pl.want[k]; ok && cur != wrote {
						fmt.Fprintf(stderr, "agentop: keeping your edit to %s; not restoring the pre-enable value\n", k)
						continue
					}
				}
				if prior == nil {
					delete(raw, k)
				} else {
					// The user had this set before enable; put their value back.
					raw[k] = *prior
					restored = append(restored, k)
				}
				continue
			}
		}
		// No ownership record (enabled by an older agentop, or state lost): fall back
		// to removing it, which is what this always did.
		delete(raw, k)
	}
	// Drop an env block we just emptied rather than leaving "env": {} behind.
	if len(raw) == 0 {
		delete(pl.doc, "env")
	}
	if err := writeSettings(pl.settingsPath, pl.doc); err != nil {
		return nil, err
	}
	if statePath != "" {
		_ = os.Remove(statePath)
	}
	return restored, nil
}

func claudeCodeDisable2(settingsPath, statePath, cortexCfgPath string, yes bool, stdout, stderr io.Writer) int {
	pl, err := planClaudeCodeDisable(settingsPath, cortexCfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentop: %v\n", err)
		return 1
	}
	if len(pl.present) == 0 {
		fmt.Fprintf(stdout, "Nothing to do: none of the Cortex variables are set in %s.\n", settingsPath)
		return 0
	}
	fmt.Fprintf(stdout, "This will remove from %s: %s\n\n", settingsPath, strings.Join(pl.present, ", "))
	if !yes && !claudeCodeConfirm(stdout) {
		fmt.Fprintln(stdout, "Not changed.")
		return exitDeclined
	}
	restored, err := applyClaudeCodeDisable(pl, statePath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "agentop: %v\n", err)
		return 1
	}
	if len(restored) > 0 {
		fmt.Fprintf(stdout, "\nRestored to the value(s) you had before: %s\n", strings.Join(restored, ", "))
	}
	fmt.Fprintf(stdout, "\nDisabled. Claude Code no longer routes through Cortex.\n")
	return 0
}

func claudeCodeStatus(settingsPath string, stdout io.Writer) int {
	doc, err := readSettings(settingsPath)
	if err != nil {
		fmt.Fprintf(stdout, "not enabled (%v)\n", err)
		return 0
	}
	env := envStrings(doc)
	set := 0
	keys := make([]string, 0, len(managedKeys))
	keys = append(keys, managedKeys...)
	sort.Strings(keys)
	for _, k := range keys {
		if v, ok := env[k]; ok {
			fmt.Fprintf(stdout, "  %s=%s\n", k, v)
			set++
		} else {
			fmt.Fprintf(stdout, "  %s (unset)\n", k)
		}
	}
	if set == len(managedKeys) {
		fmt.Fprintf(stdout, "enabled in %s\n", settingsPath)
	} else {
		fmt.Fprintf(stdout, "not fully enabled in %s (%d of %d set)\n", settingsPath, set, len(managedKeys))
	}
	return 0
}

// isCortexValue reports whether an existing value looks like one we wrote, so a
// port change in the Cortex config updates cleanly instead of tripping the
// overwrite guard.
func isCortexValue(key, val string) bool {
	switch key {
	case envNoTelem:
		return val == "1"
	case envCACerts, envSSLCert, envGitCA, envRequestsCA, envCurlCA:
		return strings.Contains(val, ".cortex"+string(os.PathSeparator)) || strings.Contains(val, "cortex-ca")
	case envProxy:
		return strings.Contains(val, "localhost:476") || strings.Contains(val, "127.0.0.1:476")
	}
	return false
}

// readSettings decodes into a generic map so every key the file already has
// survives the round trip, including ones this version of agentop knows nothing
// about. A missing file is an empty document, not an error.
func readSettings(path string) (map[string]any, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return map[string]any{}, nil
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON (%w); fix or move it before enabling", path, err)
	}
	// A bare `null` is valid JSON that unmarshals to a nil map, and assigning into
	// one panics. Treat it as the empty document it means.
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

// envRaw returns the env block as stored, creating it if absent. Callers mutate
// this map in place rather than assigning a rebuilt one: a filtered copy dropped
// every non-string value on write, so `"env": {"DEBUG": true}` silently
// disappeared — contradicting this command's own promise that everything else is
// left exactly as it was.
func envRaw(doc map[string]any) map[string]any {
	if raw, ok := doc["env"].(map[string]any); ok {
		return raw
	}
	raw := map[string]any{}
	doc["env"] = raw
	return raw
}

// envStrings is a read-only view for comparison. Non-string values are absent
// here by design — they are values we neither read nor write — but they survive
// in the document because envRaw is what gets mutated.
func envStrings(doc map[string]any) map[string]string {
	out := map[string]string{}
	raw, ok := doc["env"].(map[string]any)
	if !ok {
		return out
	}
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// writeSettings backs the file up, then replaces it atomically. Claude Code
// watches this file and reloads it, so a half-written file would be read.
func writeSettings(path string, doc map[string]any) error {
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	// Write the backup ONCE and never overwrite it. Overwriting on every call
	// meant a second enable, or an enable/disable pair, replaced the pristine
	// pre-Cortex file with one we had already edited — losing the only copy of
	// settings the user actually wrote, on a file that commonly holds API tokens.
	// A stale-but-original backup is worth more here than a fresh one of our own
	// output.
	if cur, rerr := os.ReadFile(path); rerr == nil { //nolint:gosec // operator-supplied path
		bak := path + ".bak"
		if _, serr := os.Stat(bak); os.IsNotExist(serr) {
			if werr := os.WriteFile(bak, cur, 0o600); werr != nil {
				return fmt.Errorf("writing backup %s: %w", bak, werr)
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	// 0600: this file commonly holds API tokens in the same env block.
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// claudeCodeConfirm prompts before enable or disable writes the settings file. A var
// so tests can substitute it, for the reason bobConfirm gives: `go test` inherits the
// terminal it was launched from, so an unstubbed prompt blocks waiting on a human.
// Its own var, not shared with serviceConfirm, for bobConfirm's other reason: a test
// stubbing one command's prompt must not silently disarm another's.
var claudeCodeConfirm = confirm

// confirm reads a yes/no from the terminal.
//
// It opens /dev/tty rather than reading stdin because the documented entry point
// is `curl ... | sh`: there stdin is the script itself, so reading it would
// consume the script or hit EOF and silently decline. When there is no
// controlling terminal — CI, a container, a non-interactive shell — it says so
// and declines, which callers treat as "skipped" rather than failed.
func confirm(stdout io.Writer) bool {
	tty, err := os.Open("/dev/tty")
	if err != nil {
		fmt.Fprintln(stdout, "Not a terminal, so not prompting. Re-run with --yes to apply.")
		return false
	}
	defer tty.Close()
	return confirmFrom(tty, stdout)
}

// confirmFrom is the answer-parsing half, split out so it is testable: a test
// process has no controlling terminal to open, so confirm itself cannot be
// exercised directly.
//
// Anything that is not an explicit yes declines, EOF included. The prompt says
// [y/N] and the destructive direction here is writing to a file that holds API
// tokens, so silence must mean no.
func confirmFrom(r io.Reader, stdout io.Writer) bool {
	fmt.Fprint(stdout, "Apply? [y/N] ")
	var answer string
	if _, err := fmt.Fscanln(r, &answer); err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true
	}
	return false
}

// claudeCodeEnable and claudeCodeDisable keep the pre-ownership signatures for
// callers and tests that do not care about the state file. Passing an empty
// statePath disables ownership tracking, which is the historical behaviour:
// disable then deletes the managed keys rather than restoring any the user had.
func claudeCodeEnable(settingsPath, cortexCfgPath string, yes bool, stdout, stderr io.Writer) int {
	return claudeCodeEnable2(settingsPath, cortexCfgPath, "", yes, stdout, stderr)
}

func claudeCodeDisable(settingsPath string, yes bool, stdout, stderr io.Writer) int {
	return claudeCodeDisable2(settingsPath, "", "", yes, stdout, stderr)
}
