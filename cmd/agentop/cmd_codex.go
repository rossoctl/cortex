package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// `agentop configure codex` routes Codex through Cortex by writing its dotenv file,
// $CODEX_HOME/.env (default ~/.codex/.env) — confirmed on the real Codex CLI to be
// loaded on its own at process start, not something agentop sources into a shell (see
// codexUsage). Codex has no settings JSON and no background service, so this is a
// fourth persistence mechanism alongside claude-code's settings file, bob's rc-file
// shell function and opencode's service environment.

const (
	// codexEnvRel is Codex's own dotenv file, read by Codex itself at startup,
	// relative to the home directory. Only the FALLBACK location — see
	// defaultCodexEnvPath, which honours CODEX_HOME first.
	codexEnvRel = ".codex/.env"
	// codexHomeEnv is Codex's own override for where its state lives, read by
	// find_codex_home. Set in CI, by a wrapper, or by anyone keeping more than
	// one Codex profile.
	codexHomeEnv = "CODEX_HOME"
	// codexStateRel records what the managed keys held before enable, so disable
	// can put them back — the same purpose stateRel serves for claude-code. Not
	// clientstate.RelPath: that file is claude-code's, and cortex reads it back to
	// check claude-code's CA. Nothing reads Codex's CA back out of this one today.
	codexStateRel = ".cortex/codex-state.json"
)

// WHY THE CA GOES IN SSL_CERT_FILE AND NOT CODEX_CA_CERTIFICATE, which is the
// name Codex's own HTTP client prefers.
//
// Codex does read CODEX_CA_CERTIFICATE (codex_http_client::custom_ca checks it
// before SSL_CERT_FILE, and both before native roots) — but only from the real
// process environment. Its dotenv loader throws the key away before it ever
// gets there: load_dotenv -> set_filtered drops every key whose uppercased name
// starts with "CODEX_", deliberately, so that a .env cannot reconfigure Codex
// itself ("Security: Do not allow `.env` files to create or modify any
// variables with names starting with `CODEX_`", codex-rs/arg0/src/lib.rs at
// rust-v0.160.1). Writing it here would set a CA override Codex never sees.
// SSL_CERT_FILE is not CODEX_-prefixed, so it survives the filter, and it is
// the next name custom_ca checks.
//
// MEASURED, because the failure is silent and looks like success. With the
// inherited CA variables stripped from the environment, a .env carrying
// CODEX_CA_CERTIFICATE behaved exactly like a .env carrying no CA variable at
// all: Codex rejected the bridge's forged certificate, Cortex logged
// "tls-bridge passthrough ... reason=client-hung-up" and tunnelled the bytes
// opaquely. Codex still answered the prompt — so "did it reply?" cannot
// distinguish a working CA from a broken one. The test that can is whether the
// bridge TERMINATED, which the absence of those passthrough lines shows; with
// SSL_CERT_FILE in the same file there were none.
//
// NOT "replaces the trust store", which an earlier version of this comment
// claimed by analogy with bundleKeys' OpenSSL-shaped semantics: custom_ca ADDS
// the file's certificates on top of the native roots
// (codex-rs/http-client/src/custom_ca.rs). The bundle is still the right value
// — bridge CA plus platform roots — it just is not load-bearing for reaching
// hosts the bridge does not terminate.

// codexKeys is exactly what enable writes and disable removes: the four proxy
// spellings plus the CA override (SSL_CERT_FILE — see the comment above for why
// not Codex's own CODEX_CA_CERTIFICATE). All four spellings are set rather than
// just one — not only the same inherited caution execProxyVars documents for
// other tools, but confirmed independently for Codex itself: setting each one
// alone (the other three absent) and running a bare `codex exec` showed Cortex's
// own proxy log pick up Codex's MCP handshake every time, for all four. Codex
// checks for (and uses) whichever spelling is present rather than committing to
// one, so there is no single "right" one to pick. All-or-nothing: codexWanted
// refuses outright when the bridge is off, so there is no partial "proxy only, no
// CA" state to track here.
var codexKeys = []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", envSSLCert}

// codexCanonicalKey is the name isCortexValue knows k by. isCortexValue's switch
// is keyed by VALUE SHAPE, not by key spelling, so a lowercase proxy spelling and
// the CA path both borrow the shape-check for the key whose constant already
// covers that shape — envProxy's "is this a Cortex proxy URL" and envCACerts'
// "is this a path under the Cortex CA dir", respectively. envSSLCert is in
// isCortexValue's own CA arm already, so this mapping is belt-and-braces for it
// and load-bearing only for the three proxy spellings envProxy does not name.
func codexCanonicalKey(k string) string {
	switch k {
	case "HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy":
		return envProxy
	case envSSLCert:
		return envCACerts
	}
	return k
}

// defaultCodexEnvPath is the dotenv file Codex will actually read: $CODEX_HOME/.env
// when CODEX_HOME is set, else ~/.codex/.env.
//
// Codex resolves its own home the same way (find_codex_home, which load_dotenv
// uses), so ignoring CODEX_HOME meant enable wrote a file Codex never loads —
// and, worse, status then read that same unused file and reported `enabled`.
// Both halves agreed with each other and disagreed with Codex, which is the
// failure mode a user cannot debug from the output.
//
// Only the HOME FALLBACK is joined with codexEnvRel: CODEX_HOME already names
// the .codex directory itself, so the file under it is a bare ".env" rather
// than another ".codex/.env" below it.
func defaultCodexEnvPath(home string) string {
	if ch := strings.TrimSpace(os.Getenv(codexHomeEnv)); ch != "" {
		return filepath.Join(ch, ".env")
	}
	return filepath.Join(home, codexEnvRel)
}

// codexIsOurs reports whether v, the value of k, is Cortex's: the value enable
// would set, or one isCortexValue recognises under the canonical name — so a
// value an earlier enable wrote for an older Cortex address still counts as
// Cortex's rather than being frozen into Prior as if it were the user's own.
func codexIsOurs(k, v string, want map[string]string) bool {
	if w, ok := want[k]; ok && v == w {
		return true
	}
	return isCortexValue(codexCanonicalKey(k), v)
}

// codexConfirm prompts before enable or disable writes the dotenv file. Its own
// var, for the reason claudeCodeConfirm gives: a test stubbing one command's
// prompt must not silently disarm another's.
var codexConfirm = confirm

const codexUsage = `agentop configure codex — route Codex through Cortex via its dotenv file

Usage:
  agentop configure codex enable  [--yes] [--env PATH] [--config PATH]
  agentop configure codex disable [--yes] [--env PATH]
  agentop configure codex status  [--env PATH] [--config PATH]

Codex loads $CODEX_HOME/.env (default ~/.codex/.env) itself at process start —
confirmed against the real Codex CLI (writing a dead proxy address there and
running bare "codex exec" broke its requests with no agentop wrapper
involved), not something agentop sources into a shell. enable writes four
proxy-variable spellings and a CA override into that file, reading the
addresses from ~/.cortex/config.yaml so they always match the running proxy:

  HTTP_PROXY  HTTPS_PROXY  http_proxy  https_proxy      the forward proxy URL
  SSL_CERT_FILE                                         bundle.crt: the bridge
                                                         CA plus the platform
                                                         roots

The CA goes in SSL_CERT_FILE rather than Codex's own CODEX_CA_CERTIFICATE,
which its HTTP client would otherwise prefer: Codex's dotenv loader discards
every key starting with "CODEX_" on purpose, so a CA written under that name
is one Codex never sees. SSL_CERT_FILE survives the filter and is the next
name Codex checks. If another tool already has its own SSL_CERT_FILE in this
file, enable refuses rather than overwrite it.

Only those five keys are added; everything else in the file, including blank
lines and comments, is left exactly as it was. enable scans the WHOLE file
for an existing definition of each key, not just a block it wrote itself, and
refuses rather than pick a side if a key is defined more than once. Codex
parses this file with dotenvy, whose precedence is last-wins, so appending
would quietly beat an earlier definition — including a corporate proxy
someone set deliberately. The first run copies the original file to .env.bak
and never overwrites that copy. disable removes only the keys it added.

Codex's background app-server, which makes the model calls for every codex
window, reads the file only when it starts, and so does its auto-updater. So
when the app-server is running, enable and disable stop the updater and restart
the app-server once after their change (codex app-server daemon restart, which
starts a fresh updater): turns already running get up to its shutdown grace, a
minute by default, to finish, and open Codex windows reconnect. When it is
running they say so first and ask (--yes skips the question, not the warning).
status shows whether the running app-server uses Cortex.

Note: while enabled, Codex needs Cortex running — its requests go to the
proxy address. "agentop configure codex disable" is the off switch.

Exit status: 0 applied or already correct, 3 declined (or no terminal to ask
on), 1 something went wrong, 2 a usage error.

Flags:
  --yes          do not prompt for confirmation (enable and disable)
  --env PATH     Codex's dotenv file (default ~/.codex/.env)
  --config PATH  Cortex config to read addresses from (default ~/.cortex/config.yaml)
`

func runCodex(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, codexUsage)
		return 2
	}
	action := args[0]
	// Help before the verb check, and the verb before anything touches the
	// machine: the same order runClaudeCode and runOpenCode use, for their
	// reasons.
	switch action {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, codexUsage)
		return 0
	case "enable", "disable", "status":
	default:
		fmt.Fprintf(stderr, "agentop: unknown codex action %q (enable, disable, status)\n", action)
		return 2
	}

	fs := flag.NewFlagSet("configure codex "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {}
	envPath := fs.String("env", "", "Codex's dotenv file")
	cortexCfgPath := fs.String("config", "", "Cortex config file")
	// Not for status, which changes nothing: accepting --yes there would be a
	// flag that parses and does nothing, same reasoning as runOpenCode's.
	var yesFlag *bool
	if action != "status" {
		yesFlag = fs.Bool("yes", false, "do not prompt for confirmation")
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, codexUsage)
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "agentop: codex %s takes no arguments (got %q)\n", action, fs.Arg(0))
		return 2
	}
	yes := yesFlag != nil && *yesFlag

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		fmt.Fprintf(stderr, "agentop: cannot determine your home directory: %v\n", err)
		return 1
	}
	if *envPath == "" {
		*envPath = defaultCodexEnvPath(home)
	}
	if *cortexCfgPath == "" {
		*cortexCfgPath = filepath.Join(home, cortexCfgRel)
	}
	statePath := filepath.Join(home, codexStateRel)

	switch action {
	case "enable":
		return codexEnable(*envPath, *cortexCfgPath, statePath, yes, stdout, stderr)
	case "disable":
		return codexDisable(*envPath, statePath, yes, stdout, stderr)
	default:
		return codexStatus(*envPath, *cortexCfgPath, stdout)
	}
}

// codexWanted is what enable sets, after the same two refusals claude-code's
// enable makes (see planClaudeCodeEnable): a bridge that is off, or no CA to
// trust. All-or-nothing, like openCodeWanted.
func codexWanted(cortexCfgPath string) (map[string]string, error) {
	want, cfg, err := wantedFromConfig(cortexCfgPath)
	if err != nil {
		return nil, err
	}
	if !bridgeEnabled(cfg) {
		return nil, errBridgeDisabled(cortexCfgPath)
	}
	if _, ok := want[envCACerts]; !ok {
		return nil, fmt.Errorf("%s has no tls_bridge.ca_dir, so Codex has no CA to trust;\n"+
			"  requests would fail certificate verification. Enable the TLS bridge first.", cortexCfgPath)
	}
	proxy := want[envProxy]
	return map[string]string{
		"HTTP_PROXY":  proxy,
		"HTTPS_PROXY": proxy,
		"http_proxy":  proxy,
		"https_proxy": proxy,
		// The bundle (bridge CA plus platform roots) — the same file bundleKeys'
		// vars resolve to — not the bare ca.crt envCACerts names. See the comment
		// at the top of this file for why SSL_CERT_FILE and not Codex's own name.
		envSSLCert: want[envSSLCert],
	}, nil
}

// codexEnvLineRe matches a dotenv assignment as DOTENVY parses it, which is what
// Codex uses (dotenvy 0.15.7). Three shapes an earlier `^KEY=(.*)$` missed are
// live definitions to that parser, and every one of them is a way for the
// foreign-value and duplicate guards to be bypassed:
//
//	export HTTPS_PROXY=http://corp.example.com:3128   leading "export "
//	  HTTPS_PROXY=http://corp.example.com:3128        leading whitespace
//	HTTPS_PROXY = http://corp.example.com:3128        spaces around "="
//
// Unmatched, each one let enable exit 0 and APPEND a second definition of the
// same key. Codex's precedence is last-wins, so the appended Cortex line won and
// a deliberately-set corporate proxy was silently overridden — the exact outcome
// the guards exist to prevent, reached by a file the guards could not see.
//
// A commented "#KEY=VALUE" still does not match, which is correct: "#" is not in
// the leading character class, so a commented-out assignment is not a live one.
var codexEnvLineRe = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$`)

// codexEnvFile is ~/.codex/.env, parsed into lines so editing one key never
// disturbs any other line's bytes — comments, blank lines, and unrelated
// assignments all survive untouched, the same guarantee writeSettings' doc
// comment makes for Claude Code's settings file.
type codexEnvFile struct {
	lines []string        // raw lines, no trailing newline on any entry
	index map[string]int  // key -> its line's index, for a key defined exactly once
	dup   map[string]bool // key defined on more than one line: never resolved by picking one
}

// parseCodexEnv reads path into a codexEnvFile. A missing file parses as empty,
// not an error — enable then creates it fresh, matching clientstate.Load's
// "absent is normal" convention.
//
// A key defined on more than one line is recorded in dup rather than resolved:
// set/unset refuse rather than silently guess which definition a dotenv loader's
// own last-wins/first-wins precedence would actually use — see codexUsage.
func parseCodexEnv(path string) (*codexEnvFile, error) {
	f := &codexEnvFile{index: map[string]int{}, dup: map[string]bool{}}
	b, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if err != nil {
		if os.IsNotExist(err) {
			return f, nil
		}
		return nil, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		f.lines = append(f.lines, line)
		m := codexEnvLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key := m[1]
		if _, exists := f.index[key]; exists {
			f.dup[key] = true
			continue
		}
		f.index[key] = len(f.lines) - 1
	}
	// strings.Split on a file ending in "\n" (the common case) produces one
	// trailing "" entry for the newline itself, not one more blank line. Left
	// in, a repeat enable's "append a new line" path would accumulate a
	// growing run of blank lines at the end on every run. write always adds
	// its own trailing newline back, so the file's shape round-trips either
	// way.
	if n := len(f.lines); n > 0 && f.lines[n-1] == "" {
		f.lines = f.lines[:n-1]
	}
	return f, nil
}

// get returns key's value and whether a definition exists. Ignores dup —
// callers check ambiguous before trusting a single value for a duplicated key.
func (f *codexEnvFile) get(key string) (string, bool) {
	i, ok := f.index[key]
	if !ok {
		return "", false
	}
	m := codexEnvLineRe.FindStringSubmatch(f.lines[i])
	return m[2], true
}

// ambiguous names every key in keys defined more than once in the file.
func (f *codexEnvFile) ambiguous(keys []string) []string {
	var out []string
	for _, k := range keys {
		if f.dup[k] {
			out = append(out, k)
		}
	}
	return out
}

// set writes value for key: in place, byte-for-byte unchanged elsewhere, if a
// single definition already exists; appended as a new line otherwise.
func (f *codexEnvFile) set(key, value string) {
	if i, ok := f.index[key]; ok {
		f.lines[i] = key + "=" + value
		return
	}
	f.index[key] = len(f.lines)
	f.lines = append(f.lines, key+"="+value)
}

// unset removes key's line entirely. Absent is a no-op.
func (f *codexEnvFile) unset(key string) {
	i, ok := f.index[key]
	if !ok {
		return
	}
	f.lines = append(f.lines[:i], f.lines[i+1:]...)
	delete(f.index, key)
	for k, idx := range f.index {
		if idx > i {
			f.index[k] = idx - 1
		}
	}
}

// codexMarker is the comment enable leaves above the keys it appends, so a
// reader of the file later understands why they are there.
//
// ONE CONSTANT because disable removes it by exact match. Two copies of the
// literal would drift, and a drifted copy fails silently: disable would match
// nothing and leave the line behind, which is what it used to do when there was
// no removal at all. Three enable/disable cycles on a file holding one unrelated
// key left three of these, contradicting disable's promise to remove only what
// it added and leave everything else exactly as it was.
const codexMarker = "Added by agentop configure codex enable"

// appendComment adds a bare comment line at the end.
func (f *codexEnvFile) appendComment(text string) {
	f.lines = append(f.lines, "# "+text)
}

// removeComment deletes every line that is exactly the comment appendComment
// would have written for text. Exact match, so a line a user edited — even by
// adding a space — is theirs now and is left alone; this only ever reclaims
// agentop's own untouched marker. Indices are not tracked in f.index (comments
// are not assignments), so unlike unset there is nothing to renumber.
func (f *codexEnvFile) removeComment(text string) {
	want := "# " + text
	kept := f.lines[:0]
	removed := 0
	for _, line := range f.lines {
		if line == want {
			removed++
			continue
		}
		kept = append(kept, line)
	}
	if removed == 0 {
		return
	}
	f.lines = kept
	// Every assignment after a removed comment shifted up by the number of
	// comments removed before it. Rebuilding is simpler than tracking offsets,
	// and parse is the one definition of how a line maps to a key.
	f.reindex()
}

// reindex recomputes index/dup from lines, for an edit that moved them.
func (f *codexEnvFile) reindex() {
	f.index = map[string]int{}
	f.dup = map[string]bool{}
	for i, line := range f.lines {
		m := codexEnvLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key := m[1]
		if _, seen := f.index[key]; seen {
			f.dup[key] = true
			continue
		}
		f.index[key] = i
	}
}

// write backs the file up on its first write only, then replaces it atomically
// — mirroring writeSettings exactly, including why: Codex reads this file at
// its own process start, so a half-written file is a risk worth never taking,
// not only a cosmetic one.
func (f *codexEnvFile) write(path string) error {
	if cur, rerr := os.ReadFile(path); rerr == nil { //nolint:gosec // operator-supplied path
		if berr := backupOnce(path, cur); berr != nil {
			return berr
		}
	}
	// Atomic and symlink-resolving; see writeSettings for why the fixed ".tmp"
	// name had to go (#1565).
	return writeFileAtomic(path, []byte(strings.Join(f.lines, "\n")+"\n"), 0o600)
}

// codexPlan is what enable would change, worked out without touching anything.
type codexPlan struct {
	envPath string
	f       *codexEnvFile
	want    map[string]string
	changes []string // "  KEY=VALUE" per value that differs; none = already enabled
}

// planCodexEnable runs the checks that can refuse an enable, in the order
// enable makes them. An error is the refusal, worded to follow "agentop: ".
func planCodexEnable(envPath, cortexCfgPath string) (codexPlan, error) {
	want, err := codexWanted(cortexCfgPath)
	if err != nil {
		return codexPlan{}, err
	}
	f, err := parseCodexEnv(envPath)
	if err != nil {
		return codexPlan{}, err
	}
	if dup := f.ambiguous(codexKeys); len(dup) > 0 {
		return codexPlan{}, fmt.Errorf("%s defines %s more than once; refusing to guess which\n"+
			"  definition Codex's own dotenv loading would use. Remove the duplicate(s) by hand.",
			envPath, strings.Join(dup, ", "))
	}
	// Refuse to overwrite a value the user set to something else — most likely
	// a corporate proxy — same guard planClaudeCodeEnable makes.
	for _, k := range codexKeys {
		if cur, ok := f.get(k); ok && cur != want[k] && !isCortexValue(codexCanonicalKey(k), cur) {
			return codexPlan{}, fmt.Errorf("%s is already set to %q in %s.\n"+
				"  Refusing to overwrite a value you set. Remove it first, or edit the file by hand.",
				k, cur, envPath)
		}
	}
	pl := codexPlan{envPath: envPath, f: f, want: want}
	for _, k := range codexKeys {
		cur, _ := f.get(k)
		if cur != want[k] {
			pl.changes = append(pl.changes, fmt.Sprintf("  %s=%s", k, want[k]))
		}
	}
	return pl, nil
}

// applyCodexEnable records what the managed keys held, then writes the plan's
// values. Failing to record is a warning on stderr, as it is for claude-code:
// the write still happens, and disable then deletes the keys rather than
// restoring them.
func applyCodexEnable(pl codexPlan, statePath string, stderr io.Writer) error {
	st := managedState{Settings: pl.envPath, Prior: map[string]*string{}}
	for _, k := range codexKeys {
		// A Cortex-shaped prior is recorded as absent, the same filtered capture
		// applyOpenCodeEnable makes: it is ours already, from an earlier enable,
		// and restoring it on disable would leave Codex on some earlier Cortex
		// address rather than genuinely off.
		if v, ok := pl.f.get(k); ok && !codexIsOurs(k, v, pl.want) {
			st.Prior[k] = &v
		} else {
			st.Prior[k] = nil
		}
	}
	if werr := writeState(statePath, st); werr != nil {
		fmt.Fprintf(stderr, "agentop: could not record Codex's prior values (%v); disable will\n"+
			"  delete these keys rather than restore any you had set yourself\n", werr)
	}

	commented := false
	for _, k := range codexKeys {
		if _, exists := pl.f.index[k]; !exists && !commented {
			pl.f.appendComment(codexMarker)
			commented = true
		}
		pl.f.set(k, pl.want[k])
	}
	return pl.f.write(pl.envPath)
}

func codexEnable(envPath, cortexCfgPath, statePath string, yes bool, stdout, stderr io.Writer) int {
	pl, err := planCodexEnable(envPath, cortexCfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentop: %v\n", err)
		return 1
	}
	codexHome := filepath.Dir(envPath)
	if len(pl.changes) == 0 {
		fmt.Fprintf(stdout, "Already enabled: %s routes Codex through Cortex.\n", envPath)
		// Checked here too: a second enable is what someone runs when Codex does not seem
		// to be going through Cortex, and a daemon older than the file is the usual
		// reason. Reported, not restarted: there was nothing to apply.
		reportCodexDaemon(codexHome, pl.want["HTTPS_PROXY"], true, false, stdout)
		return 0
	}
	fmt.Fprintf(stdout, "Sets in %s:\n%s\n", envPath, strings.Join(pl.changes, "\n"))
	fmt.Fprintf(stdout, "Nothing else in the file changes; a copy is kept as %s.bak if it already existed.\n", envPath)
	was := noteCodexDaemonRestart(codexHome, stdout)
	fmt.Fprintln(stdout)
	if !yes && !codexConfirm(stdout) {
		fmt.Fprintln(stdout, "Not changed.")
		return exitDeclined
	}
	if err := applyCodexEnable(pl, statePath, stderr); err != nil {
		fmt.Fprintf(stderr, "agentop: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "Enabled — run `codex` as usual. agentop configure codex disable undoes this.")
	finishCodexChange(codexHome, pl.want["HTTPS_PROXY"], pl.want, true, was, stdout, stderr)
	return 0
}

// codexDisablePlan is what disable would remove, worked out without touching
// anything.
type codexDisablePlan struct {
	envPath string
	f       *codexEnvFile
	present []string // managed keys set in the file, in codexKeys order; none = nothing to do
}

func planCodexDisable(envPath string) (codexDisablePlan, error) {
	f, err := parseCodexEnv(envPath)
	if err != nil {
		return codexDisablePlan{}, err
	}
	if dup := f.ambiguous(codexKeys); len(dup) > 0 {
		return codexDisablePlan{}, fmt.Errorf("%s defines %s more than once; refusing to guess which\n"+
			"  definition to remove. Resolve the duplicate(s) by hand, then re-run disable.",
			envPath, strings.Join(dup, ", "))
	}
	pl := codexDisablePlan{envPath: envPath, f: f}
	for _, k := range codexKeys {
		if _, ok := f.get(k); ok {
			pl.present = append(pl.present, k)
		}
	}
	return pl, nil
}

// applyCodexDisable puts back what enable recorded, removes what it added, and
// deletes the record. It returns the keys restored to a value the user had
// set. Matches applyClaudeCodeDisable's restore logic exactly, including its
// known limitation: it restores Prior unconditionally, with no check on
// whether the value changed since enable (tracked for a future fix by #1289).
//
// IN PRACTICE THE RESTORE BRANCH IS UNREACHABLE TODAY, and it is worth knowing
// that before reading the warning below as a live case. planCodexEnable refuses
// outright when a managed key holds a value that is not codexIsOurs, which is
// the exact negation of the filter applyCodexEnable records Prior through — so
// every value that survives to be recorded is one Cortex owns, and is stored as
// nil. A non-nil Prior can therefore only reach here from a hand-written state
// file (which is how TestCodexDisable_RestoresRecordedPriorValue reaches it).
//
// The machinery is kept rather than dropped so that all three dotenv-adjacent
// integrations carry the same state shape: #1289 is one pass over claude-code,
// opencode and this, and a Codex-only design would have to be reconciled back
// into it. The docs say the record is always empty today rather than implying a
// restore a user could observe.
func applyCodexDisable(pl codexDisablePlan, statePath string, stderr io.Writer) ([]string, error) {
	if len(pl.present) == 0 {
		return nil, nil
	}
	st, sterr := readState(statePath)
	if sterr != nil {
		fmt.Fprintf(stderr, "agentop: cannot read the record of what you had before enabling (%v).\n"+
			"  Falling back to removing these keys outright. If you had set any of them\n"+
			"  yourself before running enable, that value is not recoverable from here —\n"+
			"  check %s afterwards.\n\n", sterr, pl.envPath)
	}
	var restored []string
	for _, k := range pl.present {
		if st != nil && st.Settings == pl.envPath {
			if prior, recorded := st.Prior[k]; recorded {
				if prior == nil {
					pl.f.unset(k)
				} else {
					pl.f.set(k, *prior)
					restored = append(restored, k)
				}
				continue
			}
		}
		// No ownership record (enabled by an older agentop, or state lost): fall
		// back to removing it, which is what this always did.
		pl.f.unset(k)
	}
	// Reclaim enable's own marker comment, so an enable/disable cycle is a
	// round trip rather than leaving a line behind each time. Unconditional:
	// it is only ever present if an enable wrote it, and removeComment matches
	// exactly, so a user-edited copy stays theirs.
	pl.f.removeComment(codexMarker)
	if err := pl.f.write(pl.envPath); err != nil {
		return nil, err
	}
	_ = os.Remove(statePath)
	return restored, nil
}

func codexDisable(envPath, statePath string, yes bool, stdout, stderr io.Writer) int {
	pl, err := planCodexDisable(envPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentop: %v\n", err)
		return 1
	}
	if len(pl.present) == 0 {
		fmt.Fprintf(stdout, "Nothing to do: none of the Cortex variables are set in %s.\n", envPath)
		return 0
	}
	fmt.Fprintf(stdout, "This will remove from %s: %s\n", envPath, strings.Join(pl.present, ", "))
	codexHome := filepath.Dir(envPath)
	was := noteCodexDaemonRestart(codexHome, stdout)
	fmt.Fprintln(stdout)
	// The proxy the restarted daemon is compared with, read before the file loses it.
	// Disable has to work when Cortex is gone, so this is what the file held, not
	// Cortex's config; want is nil for the same reason, and codexIsOurs falls back to
	// the value's shape.
	cortexProxy, _ := pl.f.get("HTTPS_PROXY")
	if cortexProxy == "" {
		cortexProxy, _ = pl.f.get("https_proxy")
	}
	if !yes && !codexConfirm(stdout) {
		fmt.Fprintln(stdout, "Not changed.")
		return exitDeclined
	}
	restored, err := applyCodexDisable(pl, statePath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "agentop: %v\n", err)
		return 1
	}
	if len(restored) > 0 {
		fmt.Fprintf(stdout, "\nRestored to the value(s) you had before: %s\n", strings.Join(restored, ", "))
	}
	fmt.Fprintf(stdout, "\nDisabled. Codex no longer routes through Cortex.\n")
	finishCodexChange(codexHome, cortexProxy, nil, false, was, stdout, stderr)
	return 0
}

// codexStatus prints the five keys and whether they route Codex through this
// Cortex, and changes nothing. Falls back to presence-only reporting — the
// same thing claude-code's status always gives, since it never reads the
// Cortex config at all — when the config cannot be read: without it, there
// is nothing to judge the values by.
// The verdict comes from codexStateFrom (configure_status.go), which is the same
// value `agentop configure status` renders as one table row — so the paragraph here
// and the row there cannot disagree. The per-key lines and the duplicate-definition
// warning stay local: they are what makes this command worth running over the table.
func codexStatus(envPath, cortexCfgPath string, stdout io.Writer) int {
	f, err := parseCodexEnv(envPath)
	if err != nil {
		fmt.Fprintf(stdout, "not enabled (%v)\n", err)
		return 0
	}
	for _, k := range sortedKeys(codexKeys) {
		if v, ok := f.get(k); ok {
			fmt.Fprintf(stdout, "  %s=%s\n", k, v)
		} else {
			fmt.Fprintf(stdout, "  %s (unset)\n", k)
		}
	}
	if dup := f.ambiguous(codexKeys); len(dup) > 0 {
		fmt.Fprintf(stdout, "warning: %s defines %s more than once; Codex's own dotenv loading\n"+
			"  decides which line wins, not the value shown above for it\n",
			envPath, strings.Join(dup, ", "))
	}
	want, err := codexWanted(cortexCfgPath)
	if err != nil {
		fmt.Fprintf(stdout, "cannot compare with Cortex's config: %v\n", err)
		return 0
	}
	set := 0
	for _, k := range codexKeys {
		if v, ok := f.get(k); ok && v == want[k] {
			set++
		}
	}
	// codexStateFrom is asked for the verdict rather than re-deriving it from the
	// count, so the two readings cannot drift. The count is still printed from here,
	// because the table's one-line detail cannot carry it alongside the duplicate
	// warning.
	//
	// THE DUPLICATE CHECK IS NOT IN codexStateFrom, and that is why this command's
	// output is unchanged for a file with duplicates whose read values all match: it
	// still closes with "enabled in …", exactly as it did before the extraction. Only
	// codexState — the table's entry point — turns a duplicate into configureUnknown,
	// spending its single word on the warning this command has already printed on a
	// line of its own above. See codexState's comment for that split.
	on := codexStateFrom(envPath, f, want).State == configureOn
	if on {
		fmt.Fprintf(stdout, "enabled in %s\n", envPath)
	} else {
		fmt.Fprintf(stdout, "not fully enabled in %s (%d of %d set)\n", envPath, set, len(codexKeys))
	}
	// The file is what the next daemon reads; the running one may differ, and it is the
	// one sending Codex's prompts.
	reportCodexDaemon(filepath.Dir(envPath), want["HTTPS_PROXY"], on, true, stdout)
	return 0
}
