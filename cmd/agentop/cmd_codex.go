package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// `agentop configure codex` routes Codex through Cortex by writing its dotenv file,
// ~/.codex/.env — confirmed on the real Codex CLI to be loaded on its own at process
// start, not something agentop sources into a shell (see codexUsage). Codex has no
// settings JSON and no background service, so this is a third persistence mechanism
// alongside claude-code's settings file and opencode's service environment.

const (
	// codexEnvRel is Codex's own dotenv file, read by Codex itself at startup.
	codexEnvRel = ".codex/.env"
	// codexStateRel records what the managed keys held before enable, so disable
	// can put them back — the same purpose stateRel serves for claude-code. Not
	// clientstate.RelPath: that file is claude-code's, and cortex reads it back to
	// check claude-code's CA. Nothing reads Codex's CA back out of this one today.
	codexStateRel = ".cortex/codex-state.json"
	// envCodexCA is Codex's own dedicated CA override, confirmed via `strings` on
	// the real binary: codex_http_client::custom_ca checks this before falling
	// back to the generic SSL_CERT_FILE, and before system roots when neither is
	// set. Like SSL_CERT_FILE and the other bundleKeys vars, it REPLACES the trust
	// store rather than extending it, so it gets the bundle (bridge CA plus
	// platform roots), not the bare ca.crt envCACerts names. Preferring Codex's
	// own name over the generic SSL_CERT_FILE avoids colliding with a value some
	// unrelated tool has for the generic one in the same file.
	envCodexCA = "CODEX_CA_CERTIFICATE"
)

// codexKeys is exactly what enable writes and disable removes: the four proxy
// spellings (same reasoning as execProxyVars — different libraries check different
// casings, and Codex does not document which one its own HTTP client reads) plus
// Codex's own CA override. All-or-nothing: codexWanted refuses outright when the
// bridge is off, so there is no partial "proxy only, no CA" state to track here.
var codexKeys = []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", envCodexCA}

// codexCanonicalKey is the name isCortexValue knows k by. isCortexValue's switch
// is keyed by VALUE SHAPE, not by key spelling, so a lowercase proxy spelling and
// Codex's own CA var name both borrow the shape-check for the key whose constant
// already covers that shape — envProxy's "is this a Cortex proxy URL" and
// envCACerts' "is this a path under the Cortex CA dir", respectively.
func codexCanonicalKey(k string) string {
	switch k {
	case "HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy":
		return envProxy
	case envCodexCA:
		return envCACerts
	}
	return k
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

Codex loads ~/.codex/.env itself at process start — confirmed against the real
Codex CLI (writing a dead proxy address there and running bare "codex exec"
broke its requests with no agentop wrapper involved), not something agentop
sources into a shell. enable writes four proxy-variable spellings and Codex's
own CA override into that file, reading the addresses from
~/.cortex/config.yaml so they always match the running proxy:

  HTTP_PROXY  HTTPS_PROXY  http_proxy  https_proxy      the forward proxy URL
  CODEX_CA_CERTIFICATE                                  bundle.crt: the bridge
                                                         CA plus the platform
                                                         roots

CODEX_CA_CERTIFICATE is Codex's own dedicated CA override, checked (per the
real binary) before the more generic SSL_CERT_FILE, which Codex also honours
as a fallback. Using the dedicated name avoids colliding with a value some
other tool has for the generic one in the same file.

Only those five keys are added; everything else in the file, including blank
lines and comments, is left exactly as it was. enable scans the WHOLE file
for an existing definition of each key, not just a block it wrote itself, and
refuses rather than guess which line wins if a key is defined more than once
— dotenv-loader precedence for a duplicate key is undocumented. The first run
copies the original file to .env.bak and never overwrites that copy. disable
removes only the keys it added, restoring any prior value it recorded.

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
		*envPath = filepath.Join(home, codexEnvRel)
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
		// vars resolve to — not the bare ca.crt envCACerts names. See envCodexCA.
		envCodexCA: want[envSSLCert],
	}, nil
}

// codexEnvLineRe matches a plain, unexported dotenv assignment: the shape Codex's
// own .env loading reads. An "export KEY=VALUE" or a commented "#KEY=VALUE" line
// does not match, and is therefore never treated as a live definition — matching
// dotenv's own semantics rather than a shell's.
var codexEnvLineRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)

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

// appendComment adds a bare comment line at the end — used once per enable,
// immediately before the first newly-appended key, so a reader of the file
// later understands why these lines are there. Purely cosmetic: nothing here
// parses it back, so it is never load-bearing for enable/disable's own logic.
func (f *codexEnvFile) appendComment(text string) {
	f.lines = append(f.lines, "# "+text)
}

// write backs the file up on its first write only, then replaces it atomically
// — mirroring writeSettings exactly, including why: Codex reads this file at
// its own process start, so a half-written file is a risk worth never taking,
// not only a cosmetic one.
func (f *codexEnvFile) write(path string) error {
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
	b := []byte(strings.Join(f.lines, "\n") + "\n")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
			pl.f.appendComment("Added by agentop configure codex enable")
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
	if len(pl.changes) == 0 {
		fmt.Fprintf(stdout, "Already enabled: %s routes Codex through Cortex.\n", envPath)
		return 0
	}
	fmt.Fprintf(stdout, "Sets in %s:\n%s\n", envPath, strings.Join(pl.changes, "\n"))
	fmt.Fprintf(stdout, "Nothing else in the file changes; a copy is kept as %s.bak if it already existed.\n\n", envPath)
	if !yes && !codexConfirm(stdout) {
		fmt.Fprintln(stdout, "Not changed.")
		return exitDeclined
	}
	if err := applyCodexEnable(pl, statePath, stderr); err != nil {
		fmt.Fprintf(stderr, "agentop: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "Enabled — run `codex` as usual. agentop configure codex disable undoes this.")
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
	fmt.Fprintf(stdout, "This will remove from %s: %s\n\n", envPath, strings.Join(pl.present, ", "))
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
	return 0
}

// codexStatus prints the five keys and whether they route Codex through this
// Cortex, and changes nothing. Falls back to presence-only reporting — the
// same thing claude-code's status always gives, since it never reads the
// Cortex config at all — when the config cannot be read: without it, there
// is nothing to judge the values by.
func codexStatus(envPath, cortexCfgPath string, stdout io.Writer) int {
	f, err := parseCodexEnv(envPath)
	if err != nil {
		fmt.Fprintf(stdout, "not enabled (%v)\n", err)
		return 0
	}
	keys := make([]string, 0, len(codexKeys))
	keys = append(keys, codexKeys...)
	sort.Strings(keys)
	for _, k := range keys {
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
	if set == len(codexKeys) {
		fmt.Fprintf(stdout, "enabled in %s\n", envPath)
	} else {
		fmt.Fprintf(stdout, "not fully enabled in %s (%d of %d set)\n", envPath, set, len(codexKeys))
	}
	return 0
}
