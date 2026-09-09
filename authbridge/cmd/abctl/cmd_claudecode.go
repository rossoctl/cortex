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

	"github.com/rossoctl/cortex/authbridge/authlib/config"
	"github.com/rossoctl/cortex/authbridge/authlib/tlsbridge"
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
	envSSLCert    = "SSL_CERT_FILE"      // Go: gh, abctl, any Go CLI — Linux only, see above
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
	stateRel = ".cortex/claude-code-state.json"
)

// managedState is the ownership record. A nil entry means the key was absent
// before enable, so disable deletes it; a non-nil entry is the value to restore.
type managedState struct {
	Settings string             `json:"settings"`
	Prior    map[string]*string `json:"prior"`
}

// readState distinguishes "no record" from "record unreadable".
//
// Collapsing them was a silent hole: disable treats a missing record as
// "enabled by an older abctl" and falls back to deleting every managed key, so a
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
func darwinGoNote(caPath string) string {
	return "Note: on macOS, SSL_CERT_FILE is inert. Go reads roots from the keychain, not\n" +
		"  from any CA file, so that one variable does nothing here (git, curl and Python\n" +
		"  are unaffected — they honour theirs on every platform).\n\n" +
		"  Nothing to do in the common case: gh, go, pip and npm are not intercepted at\n" +
		"  all, because the bridge tunnels GitHub, the Go module proxy and the package\n" +
		"  registries by default. Their traffic holds nothing a parser can read.\n\n" +
		"  Only if you add a host to tls_bridge.passthrough_hosts' replacement list, or\n" +
		"  point a Go program at a bridged host, does that program need the CA — and on\n" +
		"  macOS only the keychain can give it one:\n\n" +
		"    security add-trusted-cert -k ~/Library/Keychains/login.keychain-db \\\n" +
		"      -p ssl " + caPath + "\n\n" +
		"  Undo with: security delete-certificate -c authbridge-tls-bridge-ca \\\n" +
		"    ~/Library/Keychains/login.keychain-db\n\n"
}

const claudeCodeUsage = `abctl claude-code — route Claude Code through Cortex without shell env vars

Usage:
  abctl claude-code enable  [--yes] [--settings PATH] [--config PATH]
  abctl claude-code disable [--yes] [--settings PATH]
  abctl claude-code status  [--settings PATH]

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
proxy address. "abctl claude-code disable" is the off switch.

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
		fmt.Fprintf(stderr, "abctl: cannot determine your home directory: %v\n", err)
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
		return claudeCodeDisable2(*settingsPath, statePath, *yes, stdout, stderr)
	case "status":
		return claudeCodeStatus(*settingsPath, stdout)
	default:
		fmt.Fprintf(stderr, "abctl: unknown claude-code action %q (enable, disable, status)\n", action)
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
// Shared, because `abctl exec` and `abctl claude-code enable` must agree about the
// bridge posture as well as the addresses. ca_dir is only *required* when
// mode is "enabled" (config.Validate), so `mode: disabled` with a ca_dir set is
// valid config that both commands used to accept — writing a CA for a bridge that
// terminates nothing, so every https request fails verification against the real
// upstream certificate. exec grew the check first; hoisting it here is what makes
// "the two cannot drift" true of the posture too, not only the proxy and CA paths.
func errBridgeDisabled(cortexCfgPath string) error {
	return fmt.Errorf("%s has no enabled TLS bridge (tls_bridge.mode must be \"enabled\");\n"+
		"  without it Cortex terminates no TLS, so there is nothing for a client to\n"+
		"  trust and every https request would fail verification. Enable the TLS bridge first",
		cortexCfgPath)
}

// wantedFromConfig is wanted plus the loaded config, so a caller needing more than
// the three values does not parse the file twice.
func wantedFromConfig(cortexCfgPath string) (map[string]string, *config.Config, error) {
	cfg, err := config.Load(cortexCfgPath)
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", cortexCfgPath, err)
	}
	addr := cfg.Listener.ForwardProxyAddr
	if addr == "" {
		return nil, nil, fmt.Errorf("%s has no listener.forward_proxy_addr; Claude Code needs a forward proxy to point at", cortexCfgPath)
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
		return nil, nil, fmt.Errorf("listener.forward_proxy_addr %q is not host:port: %w", addr, err)
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
	// block at all leaves it nil and dereferencing it segfaulted — `abctl
	// claude-code enable` crashed with a stack trace on a perfectly valid config
	// whose only fault was having no TLS bridge, which is exactly the case the
	// caller below is written to report cleanly.
	if cfg.TLSBridge != nil && cfg.TLSBridge.CADir != "" {
		ca, aerr := filepath.Abs(filepath.Join(cfg.TLSBridge.CADir, "ca.crt"))
		if aerr != nil {
			return nil, nil, aerr
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
	return out, cfg, nil
}

func claudeCodeEnable2(settingsPath, cortexCfgPath, statePath string, yes bool, stdout, stderr io.Writer) int {
	want, cfg, err := wantedFromConfig(cortexCfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}
	// Same bridge-posture gate `abctl exec` applies. Without it, `mode: disabled`
	// with a ca_dir set was written into settings.json and produced exactly the
	// silent break the ca_dir check below exists to prevent.
	if !bridgeEnabled(cfg) {
		fmt.Fprintf(stderr, "abctl: %v\n", errBridgeDisabled(cortexCfgPath))
		return 1
	}
	if _, ok := want[envCACerts]; !ok {
		fmt.Fprintf(stderr, "abctl: %s has no tls_bridge.ca_dir, so Claude Code has no CA to trust;\n"+
			"  requests would fail certificate verification. Enable the TLS bridge first.\n", cortexCfgPath)
		return 1
	}

	doc, err := readSettings(settingsPath)
	if err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}
	env := envStrings(doc)

	// Refuse to overwrite a value the user set to something else — most likely a
	// corporate proxy. Silently replacing it would break their network access and
	// give no clue why.
	for _, k := range managedKeys {
		if cur, ok := env[k]; ok && cur != want[k] && !isCortexValue(k, cur) {
			fmt.Fprintf(stderr, "abctl: %s is already set to %q in %s.\n"+
				"  Refusing to overwrite a value you set. Remove it first, or edit the file by hand.\n",
				k, cur, settingsPath)
			return 1
		}
	}

	// Both trust files are reported only when a ca_dir was configured at all.
	// wanted() populates these keys under `if cfg.TLSBridge.CADir != ""`, so
	// without one the paths are "" and an unguarded Stat printed
	// "Note:  does not exist yet." with a blank path — a note about no file.
	if want[envCACerts] != "" {
		// The CA path is written whether or not the file exists, because enabling
		// before the first start is legitimate — the proxy generates it on boot. But
		// a NODE_EXTRA_CA_CERTS pointing at a missing file fails SILENTLY: requests
		// keep working, every one tunnels through opaquely, and nothing is parsed.
		// Say so now rather than let that be discovered later.
		if _, serr := os.Stat(want[envCACerts]); serr != nil {
			fmt.Fprintf(stdout, "Note: %s does not exist yet.\n"+
				"  Cortex creates it on first start. Until then Claude Code cannot verify the\n"+
				"  bridge and every request tunnels through unparsed — which looks like nothing\n"+
				"  is wrong. Start Cortex, then check with: abctl claude-code status\n\n",
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
		if runtime.GOOS == "darwin" {
			fmt.Fprint(stdout, darwinGoNote(want[envCACerts]))
		}
	}

	var changes []string
	for _, k := range managedKeys {
		if env[k] != want[k] {
			changes = append(changes, fmt.Sprintf("  %s=%s", k, want[k]))
		}
	}
	if len(changes) == 0 {
		fmt.Fprintf(stdout, "Already enabled: %s routes Claude Code through Cortex.\n", settingsPath)
		return 0
	}

	// Three short lines, not three paragraphs. This is a confirmation prompt, so it
	// needs to say what changes and that the file is backed up; the rest (how to undo
	// it, that `claude` needs no env vars afterwards) belongs in the closing summary,
	// where it was also being printed.
	fmt.Fprintf(stdout, "Adds to the \"env\" block of %s:\n%s\n",
		settingsPath, strings.Join(changes, "\n"))
	fmt.Fprintf(stdout, "Nothing else in the file changes; a copy is kept as %s.bak\n\n", settingsPath)
	if !yes && !confirm(stdout) {
		fmt.Fprintln(stdout, "Not changed.")
		return exitDeclined
	}

	// Record what was there before, so disable restores rather than deletes.
	if statePath != "" {
		st := managedState{Settings: settingsPath, Prior: map[string]*string{}}
		for _, k := range managedKeys {
			if v, ok := env[k]; ok {
				vv := v
				st.Prior[k] = &vv
			} else {
				st.Prior[k] = nil
			}
		}
		if werr := writeState(statePath, st); werr != nil {
			fmt.Fprintf(stderr, "abctl: could not record prior settings (%v); disable will delete\n"+
				"  these keys rather than restore any you had set yourself\n", werr)
		}
	}

	raw := envRaw(doc)
	for _, k := range managedKeys {
		raw[k] = want[k]
	}
	if err := writeSettings(settingsPath, doc); err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "Enabled — run `claude` as usual.")
	return 0
}

func claudeCodeDisable2(settingsPath, statePath string, yes bool, stdout, stderr io.Writer) int {
	doc, err := readSettings(settingsPath)
	if err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}
	env := envStrings(doc)
	var present []string
	for _, k := range managedKeys {
		if _, ok := env[k]; ok {
			present = append(present, k)
		}
	}
	if len(present) == 0 {
		fmt.Fprintf(stdout, "Nothing to do: none of the Cortex variables are set in %s.\n", settingsPath)
		return 0
	}
	fmt.Fprintf(stdout, "This will remove from %s: %s\n\n", settingsPath, strings.Join(present, ", "))
	if !yes && !confirm(stdout) {
		fmt.Fprintln(stdout, "Not changed.")
		return exitDeclined
	}
	st, sterr := readState(statePath)
	if sterr != nil {
		// Proceed — the user asked for this off — but say what is about to be lost.
		// Silence here would repeat the bug the record was added to fix.
		fmt.Fprintf(stderr, "abctl: cannot read the record of what you had before enabling (%v).\n"+
			"  Falling back to removing these keys outright. If you had set any of them\n"+
			"  yourself before running enable, that value is not recoverable from here —\n"+
			"  check %s afterwards.\n\n", sterr, settingsPath)
	}
	raw := envRaw(doc)
	var restored []string
	for _, k := range present {
		if st != nil && st.Settings == settingsPath {
			if prior, recorded := st.Prior[k]; recorded {
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
		// No ownership record (enabled by an older abctl, or state lost): fall back
		// to removing it, which is what this always did.
		delete(raw, k)
	}
	// Drop an env block we just emptied rather than leaving "env": {} behind.
	if len(raw) == 0 {
		delete(doc, "env")
	}
	if err := writeSettings(settingsPath, doc); err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}
	if len(restored) > 0 {
		fmt.Fprintf(stdout, "\nRestored to the value(s) you had before: %s\n", strings.Join(restored, ", "))
	}
	if statePath != "" {
		_ = os.Remove(statePath)
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
// survives the round trip, including ones this version of abctl knows nothing
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
	return claudeCodeDisable2(settingsPath, "", yes, stdout, stderr)
}
