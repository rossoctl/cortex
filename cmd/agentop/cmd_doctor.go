package main

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/rossoctl/cortex/cmd/agentop/checklist"
	"github.com/rossoctl/cortex/core/config"
)

const doctorUsage = `agentop doctor — check this machine's Cortex install, changing nothing

Usage:
  agentop doctor

Runs setup's checks without applying any, then checks of its own, such as the
TLS bridge CA and, on macOS with Claude Code routed, whether the login keychain
holds it. Each line is ✓ fine, ! advice, or ✗ a problem with the command that
fixes it.

Exit status: 0 nothing to fix, 1 something to fix, 2 usage.
`

// doctorNow is the clock the CA's expiry is read against. A var so tests can move it.
var doctorNow = time.Now

// caExpiryWarning is how near its NotAfter the CA reads as expiring: clients
// already running keep the old CA until they restart, so the warning comes early.
const caExpiryWarning = 30 * 24 * time.Hour

// runDoctor plans every setup step in repair mode and shows each plan as a check,
// applying none of them. Routing Claude Code is checked only when it is routed.
func runDoctor(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("doctor", stderr)
	fs.Usage = func() {} // printed below, on stdout for -h, as setup's is
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprint(stdout, doctorUsage)
			return 0
		}
		_, _ = fmt.Fprint(stderr, doctorUsage) // under Parse's own line
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "agentop: unexpected argument %q\n%s", fs.Arg(0), doctorUsage)
		return 2
	}
	env, err := newSetupEnv(setupOptions{})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "agentop: %v\n", err)
		return 1
	}
	// Checked, and fixed, as set up: a background proxy as one, and Claude Code's
	// routing only where Cortex routed it. A file enable's record names other than
	// ~/.claude/settings.json, a project's, is checked on its own: setup's step
	// writes only the default file, so its plan would read that file as unrouted,
	// and its fix would route Claude Code globally.
	env.opts.noService = backgroundOnly(env)
	elsewhere := recordedSettings(env)
	if elsewhere != "" && samePath(elsewhere, filepath.Join(env.home, settingsRel)) {
		elsewhere = ""
	}
	env.opts.claudeCode = elsewhere == "" && claudeCodeRouted(env)
	fix := "agentop setup"
	if env.opts.claudeCode {
		fix += " --claude-code" // so the re-run checks the routing too
	}
	if env.opts.noService {
		fix += " --no-service" // so the re-run does not put a service in its place
	}
	ui := checklist.New(stdout, isTerminal(stdout))
	defer ui.Close()
	ui.Header("rosso cortex · doctor", version+" · "+env.goos+"/"+runtime.GOARCH)
	ui.Blank()

	failed := false
	// In order, not through planSteps: a later plan reads what an earlier one set
	// on env, and planSteps sets problems apart from the plans.
	for _, s := range buildSetupSteps(env) {
		p, prob := s.plan(env)
		label := p.label
		if label == "" {
			label = s.name()
		}
		p, prob = doctorRefine(env, s, p, prob, fix)
		if doctorCheck(ui, label, p, prob, fix) {
			failed = true
		}
		if _, ok := s.(serviceStep); !ok {
			continue
		}
		switch {
		case elsewhere != "":
			if checkRoutedElsewhere(env, ui, elsewhere, fix) {
				failed = true
			}
		case !env.opts.claudeCode:
			hint := "agentop setup --claude-code"
			if env.opts.noService {
				hint += " --no-service"
			}
			ui.Already("routed", "Claude Code is not routed; "+hint+" routes it")
		}
	}
	if checkCA(env, ui, doctorNow()) {
		failed = true
	}
	checkGoTools(env, ui)
	checkPython(env, ui)

	ui.Blank()
	if !failed {
		ui.Plain("Nothing to fix.")
		return 0
	}
	ui.Plain("Run the fix lines above, then agentop doctor again.")
	return 1
}

// doctorCheck draws one step's plan as a check and reports whether it failed. A
// change setup would make is a failure: the machine is not as setup leaves it.
// Every ✗ has a fix: the problem's own, or else fix.
func doctorCheck(ui *checklist.UI, label string, p stepPlan, prob *problem, fix string) (failed bool) {
	switch {
	case prob != nil:
		ui.Fail(label, prob.reason)
		fixes := prob.fix
		if len(fixes) == 0 {
			fixes = []string{fix}
		}
		for _, f := range fixes {
			ui.Remedy("fix: ", f)
		}
		return true
	case p.hidden:
		return false
	case p.done && p.advice != nil:
		ui.Advise(label, p.advice.reason, p.advice.fix...)
		return false
	case p.done:
		ui.Done(label, p.doneMsg, 0)
		return false
	}
	ui.Fail(label, wouldChange(p))
	ui.Remedy("fix: ", fix)
	return true
}

// wouldChange is a plan with changes as doctor's ✗ reads it: what setup would do.
func wouldChange(p stepPlan) string {
	would := "setup would " + p.verb + " " + p.what
	if p.where != "" {
		would += " · " + p.where
	}
	return would
}

// claudeCodeRouted reports whether Cortex routed Claude Code: enable's state
// record is there, or settings.json's HTTPS_PROXY is the proxy enable would write
// from the current config. Enable writes the record and every disable deletes it,
// including one that left a key behind because its value changed since enable set
// it — the record's existence is read here as "Cortex routes this", so it must not
// outlive the values it describes, or doctor reports a routing that is gone and
// offers a fix that puts it back. applyClaudeCodeDisable says why keeping it for a
// left key's sake would buy nothing. Any managed key being set is not enough: a
// corporate HTTPS_PROXY is the user's own, and disable, with no record, would
// remove it. A settings file that cannot be read, or a config that gives no proxy,
// leaves only the record to go by. It is the default file's answer: doctor asks it
// only when the record names no other file.
func claudeCodeRouted(env *setupEnv) bool {
	return fileExists(filepath.Join(env.home, stateRel)) || cortexProxyIn(env, filepath.Join(env.home, settingsRel))
}

// recordedSettings is the settings file enable's record names, which --settings
// may have made a project's: the file doctor checks and uninstall unroutes. It
// is "" when the record names none: there is no record, it cannot be read, or the
// path in it is relative, as enable wrote it from where it ran, so it is a guess.
func recordedSettings(env *setupEnv) string {
	st, err := readState(filepath.Join(env.home, stateRel))
	if err != nil || st == nil || !filepath.IsAbs(st.Settings) {
		return ""
	}
	return st.Settings
}

// cortexProxyIn reports whether the settings file's HTTPS_PROXY is the proxy
// enable would write from the current config. A file that cannot be read, or a
// config that gives no proxy, is not.
func cortexProxyIn(env *setupEnv, settings string) bool {
	doc, err := readSettings(settings)
	if err != nil {
		return false
	}
	cur, ok := envStrings(doc)[envProxy]
	if !ok {
		return false
	}
	want, err := wanted(env.configPath())
	return err == nil && cur == want[envProxy]
}

// bridgeCADir is the CA directory of the config's enabled TLS bridge, or "" when
// there is none: no config, one that will not load, or a bridge that is off and so
// mints no CA. Each of those is reported elsewhere, if at all.
func bridgeCADir(env *setupEnv) string {
	cfg, err := config.Load(env.configPath())
	if err != nil || !bridgeEnabled(cfg) {
		return ""
	}
	return cfg.TLSBridge.CADir
}

// checkCA checks the bridge CA Cortex mints as it starts, in the dir
// bridgeCANotBefore reads: both files there, ca.crt a certificate, and not near
// its expiry. An expiring CA is advice, as a restart mints a new one.
func checkCA(env *setupEnv, ui *checklist.UI, now time.Time) (failed bool) {
	dir := bridgeCADir(env)
	if dir == "" {
		return false
	}
	remint, restart := restartCommands(env) // Cortex mints a missing CA as it starts
	for _, name := range []string{"ca.crt", "bundle.crt"} {
		if !fileExists(filepath.Join(dir, name)) {
			ui.Fail("CA", "no "+name+" in "+env.tilde(dir))
			ui.Remedy("fix: ", remint)
			return true
		}
	}
	caFile := filepath.Join(dir, "ca.crt")
	crt := readCertificate(caFile)
	if crt == nil {
		ui.Fail("CA", env.tilde(caFile)+" is not a certificate")
		ui.Remedy("fix: ", remint)
		return true
	}
	expires := crt.NotAfter.Local().Format(time.DateOnly)
	if now.After(crt.NotAfter) {
		ui.Fail("CA", env.tilde(caFile)+" expired "+expires)
		ui.Remedy("fix: ", restart)
		ui.Faint("    Cortex mints a new CA as it restarts; restart the agents that use it afterwards") // at the fix's indent
		return true
	}
	if crt.NotAfter.Sub(now) <= caExpiryWarning {
		ui.Advise("CA", env.tilde(caFile)+" expires "+expires, restart)
		ui.Faint("    Cortex mints a new CA as it restarts; restart the agents that use it afterwards") // at the fix's indent
		return false
	}
	ui.Done("CA", env.tilde(caFile)+" · expires "+expires, 0)
	return false
}

// restartCommands are the commands that restart Cortex as it runs here: setup's,
// which mints a CA that is missing, and the plain restart. A background proxy has
// no service to restart, and setup without --no-service would put one in its place.
func restartCommands(env *setupEnv) (setup, restart string) {
	if env.opts.noService {
		return "agentop setup --no-service --restart", "agentop setup --no-service --restart"
	}
	return "agentop setup --restart", "agentop service restart"
}

// readCertificate is the first PEM certificate in the file at path, or nil.
func readCertificate(path string) *x509.Certificate {
	b, err := os.ReadFile(path) //nolint:gosec // the bridge CA the config names
	if err != nil {
		return nil
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil
	}
	crt, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return nil
	}
	return crt
}

// checkGoTools is darwinGoNote's advice as a check, on macOS with Claude Code
// routed: Go tools trust only the keychain, so the bridge CA has to be there for
// one to talk to a bridged host. Advice only, as most hosts a Go tool talks to are
// tunnelled unread.
func checkGoTools(env *setupEnv, ui *checklist.UI) {
	if env.goos != "darwin" || !claudeCodeRouted(env) {
		return
	}
	dir := bridgeCADir(env)
	if dir == "" {
		return // no CA to trust
	}
	caFile := filepath.Join(dir, "ca.crt")
	crt := readCertificate(caFile)
	if crt == nil {
		return // checkCA has said so
	}
	keychain := filepath.Join(env.home, "Library", "Keychains", "login.keychain-db")
	if keychainHolds(keychain, crt) {
		ui.Done("Go tools", "the CA is in the login keychain", 0)
		return
	}
	ui.Advise("Go tools", "go and gh trust only the keychain on macOS — this matters only if you bridge a host they talk to",
		"security add-trusted-cert -k "+env.tilde(keychain)+" -p ssl "+env.tilde(caFile))
}

// keychainHolds reports whether the keychain holds crt itself. Every CA Cortex
// mints has the same name, so after a re-mint the old one still matches by name;
// security's SHA-256 of each match is compared with crt's instead.
func keychainHolds(keychain string, crt *x509.Certificate) bool {
	out, err := exec.Command("security", "find-certificate", "-a", "-Z", "-c", bobCACommonName, keychain).Output() //nolint:gosec // a fixed command on our own keychain path
	if err != nil {
		return false
	}
	sum := sha256.Sum256(crt.Raw)
	want := hex.EncodeToString(sum[:])
	for _, l := range strings.Split(string(out), "\n") {
		if h, ok := strings.CutPrefix(strings.TrimSpace(l), "SHA-256 hash:"); ok && strings.EqualFold(strings.TrimSpace(h), want) {
			return true
		}
	}
	return false
}
