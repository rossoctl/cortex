package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// `agentop configure opencode` routes OpenCode through Cortex for good. The process you
// run is the wrong place for that: OpenCode's background service sends every session's
// traffic, outlives the client that started it, and keeps an environment of its own in
// service.json, which it takes over the one it inherited (see opencode.go). So enable
// writes the variables there, and only through the opencode CLI.

const (
	// opencodeStateRel is enable's record of what the nine variables held before it ran.
	// enable only ever replaces Cortex's own values, and records those as absent, so the
	// record holds nothing for disable to put back unless a value is written into it by
	// hand. Not clientstate.RelPath: that file is Claude Code's, and cortex reads it back
	// to check Claude Code's CA.
	opencodeStateRel = ".cortex/opencode-state.json"
	// opencodeStateSettings is the record's Settings field. Claude Code's record names
	// the settings file it describes; OpenCode's environment is reached through its
	// CLI rather than a file agentop writes, so the record names that instead.
	opencodeStateSettings = "opencode service env"
)

// openCodeKeys are the variables enable sets: the nine `agentop exec` gives a child, for
// the reasons execProxyVars and bundleKeys give. Not CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC,
// which only Claude Code reads. The order is the one enable lists them in and disable
// changes them in, the proxy first; enable sets them CA first (openCodeApplyOrder).
var openCodeKeys = append([]string{envProxy, "HTTP_PROXY", "https_proxy", "http_proxy", envCACerts}, bundleKeys...)

// openCodeCanonicalKey is the name wantedFromConfig and isCortexValue know k by. Both
// know the proxy only as HTTPS_PROXY, the one spelling Claude Code needs.
func openCodeCanonicalKey(k string) string {
	switch k {
	case "HTTP_PROXY", "https_proxy", "http_proxy":
		return envProxy
	}
	return k
}

// openCodeIsOurs reports whether v, the value of k, is Cortex's: the value enable sets,
// for a proxy key any spelling of the same listener (sameProxy: localhost, 127.0.0.1
// and ::1 are one host), or one isCortexValue recognises. isCortexValue is asked under
// the canonical name, so a lowercase proxy an earlier enable wrote for an older Cortex
// address counts as Cortex's.
func openCodeIsOurs(k, v string, want map[string]string) bool {
	if w, ok := want[k]; ok && v == w {
		return true
	}
	if openCodeCanonicalKey(k) == envProxy && sameProxy(v, want[k]) {
		return true
	}
	return isCortexValue(openCodeCanonicalKey(k), v)
}

// opencodeConfirm prompts before enable or disable changes the service environment.
// Its own var, for the reasons claudeCodeConfirm gives: tests replace it, and stubbing
// it must not disarm another command's prompt.
var opencodeConfirm = confirm

const openCodeUsage = `agentop configure opencode — route OpenCode through Cortex via its background service

Usage:
  agentop configure opencode enable | disable [--yes] [--config PATH] [--opencode BIN]
  agentop configure opencode status [--config PATH] [--opencode BIN]

OpenCode's traffic does not leave from the opencode you run. One background
service per user, "opencode serve --service", sends every session's requests;
the first client starts it and later ones reuse it. The service keeps an
environment of its own, which it takes over the one it inherited, so that is
where the routing goes. Every change is made with the opencode CLI
("opencode service set env" and "opencode service unset env").

enable sets the nine variables "agentop exec" sets, reading the addresses from
~/.cortex/config.yaml so they match the proxy:

  HTTPS_PROXY  HTTP_PROXY  https_proxy  http_proxy      the forward proxy URL
  NODE_EXTRA_CA_CERTS                                   ca.crt
  SSL_CERT_FILE  GIT_SSL_CAINFO                         bundle.crt: the bridge CA
  REQUESTS_CA_BUNDLE  CURL_CA_BUNDLE                    plus the platform roots

"agentop exec --help" says why each gets which file. enable refuses to overwrite a
value someone else set and leaves every other variable alone; the only values it
replaces are Cortex's own, such as a proxy at an older Cortex address. Its first
run records the nine in ~/.cortex/opencode-state.json, counting Cortex's values as
absent, so disable removes them rather than putting an old Cortex address back.
disable changes only Cortex's values: one set some other way, even after enable,
is left alone and named. status shows the nine and the running service, and
changes nothing.

Changing the service environment through OpenCode's CLI stops a running
service, and an open OpenCode starts it again straight away, before the change
lands. So when the service was running, enable and disable restart it once
after their change, which interrupts every OpenCode session using it; an open
OpenCode reconnects to it. When there is something to change and the service is
running, or may be, they say so first and ask (--yes skips the question, not the
warning). A service that was not running starts with the new environment the
next time you run OpenCode.

Note: once its service has started with these, OpenCode needs Cortex running —
its requests go to the proxy address. "agentop configure opencode disable" is the
off switch.

Exit status: 0 applied or already correct, 3 declined (or no terminal to ask
on), 1 something went wrong, 2 a usage error.

Flags:
  --yes            do not prompt for confirmation (enable and disable)
  --config PATH    Cortex config to read addresses from (default ~/.cortex/config.yaml)
  --opencode BIN   the opencode CLI (default: opencode on PATH, else ~/.opencode/bin/opencode)
`

func runOpenCode(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, openCodeUsage)
		return 2
	}
	action := args[0]
	// Help before the verb check, and the verb before anything touches the machine:
	// the same order as runBob, for its reasons.
	switch action {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, openCodeUsage)
		return 0
	case "enable", "disable", "status":
	default:
		fmt.Fprintf(stderr, "agentop: unknown opencode action %q (enable, disable, status)\n", action)
		return 2
	}

	fs := flag.NewFlagSet("configure opencode "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cortexCfgPath := fs.String("config", "", "Cortex config file")
	bin := fs.String("opencode", "", "the opencode CLI")
	// Not for status, which changes nothing: accepting --yes there would be a flag
	// that parses and does nothing.
	var yesFlag *bool
	if action != "status" {
		yesFlag = fs.Bool("yes", false, "do not prompt for confirmation")
	}
	fs.Usage = func() {}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, openCodeUsage)
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "agentop: opencode %s takes no arguments (got %q)\n", action, fs.Arg(0))
		return 2
	}
	yes := yesFlag != nil && *yesFlag

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		fmt.Fprintf(stderr, "agentop: cannot determine your home directory: %v\n", err)
		return 1
	}
	if *cortexCfgPath == "" {
		*cortexCfgPath = filepath.Join(home, cortexCfgRel)
	}
	if *bin == "" {
		found, ferr := findOpenCode()
		if ferr != nil {
			fmt.Fprintf(stderr, "agentop: %v; name it with --opencode PATH\n", ferr)
			return 1
		}
		*bin = found
	}
	statePath := filepath.Join(home, opencodeStateRel)

	// What every opencode CLI call is judged against (openCodeCLIWant): Cortex's values from
	// this config, or nothing when it cannot be read, so only their shape decides.
	openCodeCLIWant = nil
	if w, _, werr := wantedFromConfig(*cortexCfgPath); werr == nil {
		openCodeCLIWant = openCodeValues(w)
	}
	defer func() { openCodeCLIWant = nil }()

	switch action {
	case "enable":
		return openCodeEnable(*bin, *cortexCfgPath, statePath, yes, stdout, stderr)
	case "disable":
		return openCodeDisable(*bin, *cortexCfgPath, statePath, yes, stdout, stderr)
	default:
		return openCodeStatus(*bin, *cortexCfgPath, stdout, stderr)
	}
}

// openCodeValues spreads wantedFromConfig's values over the nine keys: its proxy URL
// into all four proxy spellings, the rest by name. A key with no value there, the CA
// keys of a config without a ca_dir, is left out.
func openCodeValues(want map[string]string) map[string]string {
	out := make(map[string]string, len(openCodeKeys))
	for _, k := range openCodeKeys {
		if v := want[openCodeCanonicalKey(k)]; v != "" {
			out[k] = v
		}
	}
	return out
}

// openCodeWanted is what enable sets, after the two refusals claude-code enable makes,
// for the reasons planClaudeCodeEnable gives: a bridge that is off, or no CA to trust.
func openCodeWanted(cortexCfgPath string) (map[string]string, error) {
	want, cfg, err := wantedFromConfig(cortexCfgPath)
	if err != nil {
		return nil, err
	}
	if !bridgeEnabled(cfg) {
		return nil, errBridgeDisabled(cortexCfgPath)
	}
	if _, ok := want[envCACerts]; !ok {
		return nil, fmt.Errorf("%s has no tls_bridge.ca_dir, so OpenCode has no CA to trust;\n"+
			"  requests would fail certificate verification. Enable the TLS bridge first.", cortexCfgPath)
	}
	return openCodeValues(want), nil
}

// openCodeServiceEnv is OpenCode's service environment as `opencode service get env`
// prints it: a JSON object of strings, {} when empty.
func openCodeServiceEnv(bin string) (map[string]string, error) {
	out, err := openCodeRun(bin, "service", "get", "env")
	if err != nil {
		return nil, err
	}
	bad := fmt.Errorf("opencode service get env printed %q, which is not a JSON object of strings", out)
	// Pointers, because json.Unmarshal leaves a string empty for null rather than
	// failing; a nil map with no error is a top-level null.
	var raw map[string]*string
	if err := json.Unmarshal([]byte(out), &raw); err != nil || raw == nil {
		return nil, bad
	}
	env := make(map[string]string, len(raw))
	for k, v := range raw {
		if v == nil {
			return nil, bad
		}
		env[k] = *v
	}
	return env, nil
}

// openCodePlan is what enable would change, worked out without changing anything.
type openCodePlan struct {
	bin     string
	env     map[string]string // the service environment before the change
	want    map[string]string // the nine values enable sets
	changes []string          // keys whose value differs, in openCodeKeys order; none = already enabled
}

// planOpenCodeEnable runs the checks that can refuse an enable. An error is the
// refusal, worded to follow "agentop: ".
func planOpenCodeEnable(bin, cortexCfgPath string) (openCodePlan, error) {
	want, err := openCodeWanted(cortexCfgPath)
	if err != nil {
		return openCodePlan{}, err
	}
	env, err := openCodeServiceEnv(bin)
	if err != nil {
		return openCodePlan{}, err
	}
	// Refuse a value someone else set, most likely a corporate proxy or CA, as
	// claude-code does.
	for _, k := range openCodeKeys {
		if cur, ok := env[k]; ok && !openCodeIsOurs(k, cur, want) {
			return openCodePlan{}, fmt.Errorf("%s is already set to %q in OpenCode's service environment.\n"+
				"  Refusing to overwrite a value you set. Remove it first: opencode service unset env %s", k, cur, k)
		}
	}
	pl := openCodePlan{bin: bin, env: env, want: want}
	for _, k := range openCodeKeys {
		if env[k] != want[k] {
			pl.changes = append(pl.changes, k)
		}
	}
	return pl, nil
}

// applyOpenCodeEnable records what the keys held, then sets each one that differs. A
// value it replaces is always Cortex's, the plan having refused any other, so every key
// is recorded as absent. Failing to record is a warning, as it is for claude-code, and
// the change still happens: disable removes Cortex's values without a record.
//
// The CA variables are set before the proxy (openCodeApplyOrder), so a failure part way
// never leaves the proxy set without the CA that lets OpenCode trust it. partial is
// whether a set failed after an earlier one succeeded.
func applyOpenCodeEnable(pl openCodePlan, statePath string, stderr io.Writer) (partial bool, err error) {
	st := managedState{Settings: opencodeStateSettings, Prior: map[string]*string{}}
	for _, k := range openCodeKeys {
		// A Cortex-shaped prior is recorded as absent: enable overwrote it without
		// asking because it is ours, and restoring it would leave OpenCode on Cortex
		// after disable.
		if v, ok := pl.env[k]; ok && !openCodeIsOurs(k, v, pl.want) {
			st.Prior[k] = &v
		} else {
			st.Prior[k] = nil
		}
	}
	if err := writeState(statePath, st); err != nil {
		fmt.Fprintf(stderr, "agentop: could not record OpenCode's prior values (%v).\n"+
			"  Enabling anyway: every value this replaces is Cortex's, and disable removes\n"+
			"  those without a record.\n", err)
	}
	order := openCodeApplyOrder(pl.changes)
	for i, k := range order {
		if _, err := openCodeRun(pl.bin, "service", "set", "env", k, pl.want[k]); err != nil {
			if i == 0 {
				return false, err
			}
			return true, fmt.Errorf("%w\n  Already set: %s. agentop configure opencode disable undoes them",
				err, strings.Join(order[:i], ", "))
		}
	}
	return false, nil
}

// openCodeApplyOrder is keys, in openCodeKeys order, with the CA variables moved ahead of
// the proxy ones.
func openCodeApplyOrder(keys []string) []string {
	var ca, proxy []string
	for _, k := range keys {
		if openCodeCanonicalKey(k) == envProxy {
			proxy = append(proxy, k)
		} else {
			ca = append(ca, k)
		}
	}
	return append(ca, proxy...)
}

// openCodeStoppedMidway is said after a change that failed part way, when the service was
// running before it: the writes that succeeded stop it, an open OpenCode may have started
// it again, and agentop does not restart it with its environment half changed or probe it.
const openCodeStoppedMidway = "  OpenCode's background service may be stopped; re-run this command to finish.\n"

func openCodeEnable(bin, cortexCfgPath, statePath string, yes bool, stdout, stderr io.Writer) int {
	pl, err := planOpenCodeEnable(bin, cortexCfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentop: %v\n", err)
		return 1
	}
	if len(pl.changes) == 0 {
		fmt.Fprintln(stdout, "Already enabled: OpenCode's service environment routes it through Cortex.")
		reportOpenCodeService(bin, pl.want[envProxy], true, false, stdout)
		return 0
	}
	// Where claude-code prints it, and for its reason: the go and gh the service runs
	// read SSL_CERT_FILE no more than Claude Code's do. See darwinGoNote.
	if runtime.GOOS == "darwin" {
		fmt.Fprint(stdout, darwinGoNote(pl.want[envCACerts]))
	}
	fmt.Fprintln(stdout, "Sets in OpenCode's background-service environment (opencode service set env):")
	for _, k := range pl.changes {
		fmt.Fprintf(stdout, "  %s=%s\n", k, pl.want[k])
	}
	fmt.Fprintln(stdout, "Nothing else in that environment changes; agentop configure opencode disable removes them again.")
	wasRunning := noteOpenCodeServiceRestarts(bin, stdout)
	fmt.Fprintln(stdout)
	if !yes && !opencodeConfirm(stdout) {
		fmt.Fprintln(stdout, "Not changed.")
		return exitDeclined
	}
	if partial, err := applyOpenCodeEnable(pl, statePath, stderr); err != nil {
		fmt.Fprintf(stderr, "agentop: %v\n", err)
		if partial && wasRunning {
			fmt.Fprint(stderr, openCodeStoppedMidway)
		}
		return 1
	}
	finishOpenCodeChange(bin, pl.want[envProxy], true, wasRunning, stdout, stderr)
	return 0
}

// openCodeDisablePlan is what disable would change, worked out without changing anything.
type openCodeDisablePlan struct {
	bin     string
	env     map[string]string
	st      *managedState // enable's record; nil when there is none, it is unreadable, or it is not OpenCode's
	stErr   error         // why the record could not be read
	present []string      // keys disable changes, in openCodeKeys order; none = nothing to do
	left    []string      // keys it leaves: their values are not Cortex's
}

// planOpenCodeDisable sorts the managed keys set in the service environment into those
// disable changes and those it leaves. want is what enable would set, empty when the
// Cortex config cannot be read. The record decides only what a changed key goes back
// to, never whether it is changed.
func planOpenCodeDisable(bin, statePath string, want map[string]string) (openCodeDisablePlan, error) {
	env, err := openCodeServiceEnv(bin)
	if err != nil {
		return openCodeDisablePlan{}, err
	}
	st, stErr := readState(statePath)
	if st != nil && st.Settings != opencodeStateSettings {
		st = nil
	}
	pl := openCodeDisablePlan{bin: bin, env: env, st: st, stErr: stErr}
	for _, k := range openCodeKeys {
		v, ok := env[k]
		if !ok {
			continue
		}
		// Only a value that is Cortex's, whatever the record says: one set some other
		// way, even after enable, is someone else's, and changing it would lose it.
		if openCodeIsOurs(k, v, want) {
			pl.present = append(pl.present, k)
		} else {
			pl.left = append(pl.left, k)
		}
	}
	return pl, nil
}

// applyOpenCodeDisable changes the keys the plan found holding Cortex's values: each goes
// back to a value enable recorded, or is unset. Then it deletes the record. It returns
// the keys restored to a recorded value, and on an error whether it came after an
// earlier change succeeded (partial).
//
// The record is deleted only once every key is done, so a failure part way leaves it
// for the next disable, which then finishes the job.
func applyOpenCodeDisable(pl openCodeDisablePlan, statePath string, stderr io.Writer) (restored []string, partial bool, err error) {
	if pl.stErr != nil {
		// Proceed, as claude-code does: the user asked for this off. enable never
		// records a value of the user's, so only one written in by hand can be lost.
		fmt.Fprintf(stderr, "agentop: cannot read the record of what OpenCode's service environment held before enable (%v).\n"+
			"  Removing Cortex's values without it. enable records none of yours, so this\n"+
			"  loses nothing unless a value was written into the record by hand.\n\n", pl.stErr)
	}
	for i, k := range pl.present {
		var err error
		if prior := openCodePrior(pl.st, k); prior != nil {
			_, err = openCodeRun(pl.bin, "service", "set", "env", k, *prior)
			if err == nil {
				restored = append(restored, k)
			}
		} else {
			// Absent before enable, or no record of it: removing it is all that is left.
			_, err = openCodeRun(pl.bin, "service", "unset", "env", k)
		}
		if err != nil {
			return nil, i > 0, fmt.Errorf("%w\n  Run agentop configure opencode disable again to finish", err)
		}
	}
	_ = os.Remove(statePath)
	return restored, false, nil
}

// openCodePrior is the value st recorded for k before enable, or nil when k was absent
// or there is no record.
func openCodePrior(st *managedState, k string) *string {
	if st == nil {
		return nil
	}
	return st.Prior[k]
}

func openCodeDisable(bin, cortexCfgPath, statePath string, yes bool, stdout, stderr io.Writer) int {
	// The config is not required: disable has to work when Cortex is gone. Without it,
	// only isCortexValue says which values are Cortex's.
	want, cortexProxy := map[string]string{}, ""
	if w, _, werr := wantedFromConfig(cortexCfgPath); werr == nil {
		want, cortexProxy = openCodeValues(w), w[envProxy]
	}
	pl, err := planOpenCodeDisable(bin, statePath, want)
	if err != nil {
		fmt.Fprintf(stderr, "agentop: %v\n", err)
		return 1
	}
	if len(pl.present) == 0 && len(pl.left) == 0 {
		fmt.Fprintln(stdout, "Nothing to do: none of the Cortex variables are set in OpenCode's service environment.")
		return 0
	}
	if len(pl.present) > 0 {
		fmt.Fprintf(stdout, "This will remove from OpenCode's service environment: %s\n", strings.Join(pl.present, ", "))
	}
	for _, k := range pl.left {
		fmt.Fprintf(stdout, "  %s left as %q: it is not a value Cortex set. Remove it yourself: opencode service unset env %s\n",
			k, pl.env[k], k)
	}
	if len(pl.present) == 0 {
		return 0
	}
	wasRunning := noteOpenCodeServiceRestarts(bin, stdout)
	fmt.Fprintln(stdout)
	if !yes && !opencodeConfirm(stdout) {
		fmt.Fprintln(stdout, "Not changed.")
		return exitDeclined
	}
	restored, partial, err := applyOpenCodeDisable(pl, statePath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "agentop: %v\n", err)
		if partial && wasRunning {
			fmt.Fprint(stderr, openCodeStoppedMidway)
		}
		return 1
	}
	if len(restored) > 0 {
		fmt.Fprintf(stdout, "Restored to the value(s) you had before: %s\n", strings.Join(restored, ", "))
	}

	// The service is compared with Cortex's proxy. When the config cannot be read, the
	// proxy disable just took out stands in for it: that is what a service still running
	// with its old environment uses.
	if cortexProxy == "" {
		for _, k := range pl.present {
			if openCodeCanonicalKey(k) == envProxy {
				cortexProxy = pl.env[k]
				break
			}
		}
	}
	finishOpenCodeChange(bin, cortexProxy, false, wasRunning, stdout, stderr)
	return 0
}

// openCodeStatus prints the nine keys, whether they route OpenCode through this Cortex,
// and the running service. It changes nothing.
func openCodeStatus(bin, cortexCfgPath string, stdout, stderr io.Writer) int {
	env, err := openCodeServiceEnv(bin)
	if err != nil {
		fmt.Fprintf(stderr, "agentop: %v\n", err)
		return 1
	}
	for _, k := range slices.Sorted(slices.Values(openCodeKeys)) {
		if v, ok := env[k]; ok {
			fmt.Fprintf(stdout, "  %s=%s\n", k, v)
		} else {
			fmt.Fprintf(stdout, "  %s (unset)\n", k)
		}
	}
	want, _, err := wantedFromConfig(cortexCfgPath)
	if err != nil {
		// Without Cortex's addresses there is nothing to judge the values by, so the
		// keys and the service are reported as they are. Not a failure: status reports.
		fmt.Fprintf(stdout, "cannot compare with Cortex's config: %v\n", err)
		reportOpenCodeService(bin, "", false, true, stdout)
		return 0
	}
	vals := openCodeValues(want)
	// Set means set to Cortex's value. A variable left at an older Cortex address is
	// shown above but not counted: a service started with it would not reach this one.
	set := 0
	for _, k := range openCodeKeys {
		if w, ok := vals[k]; ok && env[k] == w {
			set++
		}
	}
	// The verdict is openCodeStateFrom's (configure_status.go), the same value the
	// `agentop configure status` row carries, so the two cannot disagree. The count is
	// still printed from here: the table's one-line detail says it in its own words.
	if openCodeStateFrom(env, vals).State == configureOn {
		fmt.Fprintln(stdout, "enabled")
	} else {
		fmt.Fprintf(stdout, "not fully enabled (%d of %d set)\n", set, len(openCodeKeys))
	}
	// The service line is judged by the proxy a restart would give the service, read as
	// Bun reads it, not by the verdict above: a missing CA variable does not take the
	// service off Cortex, and a service started under agentop exec is on Cortex
	// whatever its service environment says.
	list := make([]string, 0, len(env))
	for k, v := range env {
		list = append(list, k+"="+v)
	}
	reportOpenCodeService(bin, vals[envProxy], sameProxy(environProxy(list), vals[envProxy]), true, stdout)
	return 0
}

// noteOpenCodeServiceRestarts probes the service before enable or disable changes its
// environment, prints what the change does to it, and returns whether it was running.
// OpenCode's CLI stops a running service whenever `service set env` or `service unset
// env` changes its environment (seen on 2.0.21), and an open OpenCode starts it again
// before the change lands, so finishOpenCodeChange restarts a service this saw running.
//
// A running service is named by its pid, or not when no process could be found on its
// port. When the CLI cannot say whether it runs, the line says what happens if it does.
// A service that is not running gets no line.
func noteOpenCodeServiceRestarts(bin string, stdout io.Writer) bool {
	svc, err := probeOpenCodeService(bin)
	switch {
	case err != nil:
		// Not restarted afterwards, since nothing says it was running: only the CLI's stop.
		fmt.Fprint(stdout, "If OpenCode's background service is running, changing its environment stops it, which\n"+
			"  interrupts every OpenCode session using it.\n")
	case svc.Running && svc.PID != 0:
		fmt.Fprintf(stdout, "OpenCode's background service is running (pid %d). Changing its environment restarts it,\n"+
			"  which interrupts every OpenCode session using it; an open OpenCode reconnects to it.\n", svc.PID)
	case svc.Running:
		fmt.Fprint(stdout, "OpenCode's background service is running. Changing its environment restarts it, which\n"+
			"  interrupts every OpenCode session using it; an open OpenCode reconnects to it.\n")
	}
	return err == nil && svc.Running
}

// finishOpenCodeChange prints the closing lines once enable (configured) or disable has
// changed the service environment.
//
// A service that was not running before the change is left alone: the closing line says
// the change applies from its next start, and the service is reported as
// reportOpenCodeService reports it, which says nothing about one that is not running.
//
// A service that was running before is restarted once, with `opencode service restart`,
// which starts it with its service environment. Then it is probed again, and one line
// says whether its proxy now matches the change: Cortex's after enable, not Cortex's
// after disable. A restart that fails is reported on stderr; the change itself
// succeeded, so the caller still exits 0.
func finishOpenCodeChange(bin, cortexProxy string, configured, wasRunning bool, stdout, stderr io.Writer) {
	done, nextStart := "Disabled.", "Disabled. OpenCode's service no longer routes through Cortex from its next start."
	if configured {
		done, nextStart = "Enabled.", "Enabled — OpenCode's service uses Cortex from its next start."
	}
	if !wasRunning {
		fmt.Fprintln(stdout, nextStart)
		reportOpenCodeService(bin, cortexProxy, configured, false, stdout)
		return
	}
	fmt.Fprintln(stdout, done)
	if _, err := openCodeRun(bin, "service", "restart"); err != nil {
		fmt.Fprintf(stderr, "agentop: could not restart OpenCode's background service (%v).\n"+
			"  It may be stopped, or running with its old environment. To apply the change now:\n"+
			"    opencode service restart   # interrupts every OpenCode session using it\n", err)
		return
	}
	svc, err := probeOpenCodeService(bin)
	switch {
	case err != nil:
		fmt.Fprintf(stdout, "OpenCode's background service was restarted; it could not be checked (%v).\n", err)
	case !svc.Running:
		fmt.Fprintln(stdout, "OpenCode's background service was restarted, but is not running now. "+
			"It starts with the new environment the next time you run OpenCode.")
	case svc.EnvErr != nil && svc.PID == 0:
		fmt.Fprintln(stdout, "OpenCode's background service was restarted; its environment could not be checked.")
	case svc.EnvErr != nil:
		fmt.Fprintf(stdout, "OpenCode's background service was restarted (pid %d); its environment could not be checked.\n", svc.PID)
	case cortexProxy == "":
		// disable with no Cortex config and no proxy of Cortex's taken out: nothing to
		// compare the restarted service with.
		fmt.Fprintf(stdout, "OpenCode's background service was restarted (pid %d).\n", svc.PID)
	case sameProxy(svc.Proxy, cortexProxy) == configured:
		if configured {
			fmt.Fprintf(stdout, "OpenCode's background service was restarted (pid %d) and is using Cortex.\n", svc.PID)
		} else {
			fmt.Fprintf(stdout, "OpenCode's background service was restarted (pid %d) and no longer uses Cortex.\n", svc.PID)
		}
	default:
		describeOpenCodeService(svc, nil, cortexProxy, configured, false, stdout)
	}
}

// reportOpenCodeService probes the running service and says how it compares with its
// service environment. It runs only `opencode service status`, which changes nothing.
func reportOpenCodeService(bin, cortexProxy string, configured, status bool, stdout io.Writer) {
	svc, err := probeOpenCodeService(bin)
	describeOpenCodeService(svc, err, cortexProxy, configured, status, stdout)
}

// describeOpenCodeService prints the line for a probe's result, svc and err.
//
// cortexProxy is Cortex's proxy URL, "" when it is not known. configured is whether the
// service environment routes OpenCode through it: true after enable, false after
// disable, and for status whatever the environment's proxy says. status asks for a line
// in every case; enable and disable say only what needs doing or what was confirmed.
func describeOpenCodeService(svc openCodeService, err error, cortexProxy string, configured, status bool, stdout io.Writer) {
	if err == nil && svc.Running {
		err = svc.EnvErr
	}
	switch {
	case err != nil && status:
		fmt.Fprintf(stdout, "Could not check OpenCode's background service (%v).\n", err)
		return
	case err != nil:
		fmt.Fprintf(stdout, "Could not check OpenCode's running service (%v); restart it to be sure: opencode service restart\n", err)
		return
	case !svc.Running:
		if status {
			fmt.Fprintln(stdout, "OpenCode's background service is not running.")
		}
		return
	case cortexProxy == "":
		if !status {
			return
		}
		if svc.Proxy == "" {
			fmt.Fprintf(stdout, "OpenCode's background service (pid %d) is running with no proxy.\n", svc.PID)
		} else {
			fmt.Fprintf(stdout, "OpenCode's background service (pid %d) is running with proxy %s.\n", svc.PID, svc.Proxy)
		}
		return
	}
	const oldEnv = "OpenCode's background service (pid %d) is running with its old environment.\n" +
		"  It picks this up when it restarts: opencode service restart   # interrupts every OpenCode session using it\n"
	running := sameProxy(svc.Proxy, cortexProxy)
	switch {
	case running && configured:
		fmt.Fprintf(stdout, "OpenCode's background service (pid %d) is using Cortex.\n", svc.PID)
	case configured:
		fmt.Fprintf(stdout, oldEnv, svc.PID)
	case running && status:
		// Started some other way, most likely under agentop exec. A restart would take
		// it off Cortex, which is the opposite of the note above.
		fmt.Fprintf(stdout, "OpenCode's background service (pid %d) is using Cortex, but its service environment does not route it there,\n"+
			"  so its next start will not use Cortex. To keep it on Cortex:\n"+
			"    agentop configure opencode enable   # restarts the service, interrupting every OpenCode session using it\n", svc.PID)
	case running:
		fmt.Fprintf(stdout, oldEnv, svc.PID)
	case status:
		fmt.Fprintf(stdout, "OpenCode's background service (pid %d) is not using Cortex.\n", svc.PID)
	}
}
