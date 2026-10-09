package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/rossoctl/cortex/cmd/agentop/checklist"
)

// setupOptions are agentop setup's flags — the interface install.sh hands off
// through, frozen here so PR 4's script needs none that this binary lacks.
type setupOptions struct {
	from           string
	claudeCode     bool
	yes            bool
	noService      bool
	installOnly    bool
	noModifyPath   bool
	restart        bool
	handoffBytes   int64
	handoffSeconds int
}

// fromInstaller reports whether install.sh invoked setup: only it sets the
// hidden handoff flags.
func (o setupOptions) fromInstaller() bool { return o.handoffBytes > 0 || o.handoffSeconds > 0 }

// setupEnv is one run's machine and what the plans found out about it.
type setupEnv struct {
	home, goos, shell, pathEnv string
	binDir, cortexDir, fromDir string
	opts                       setupOptions
	argv                       []string // the command this setup was run as, for rerun

	// Established while planning; later plans and the ending read them.
	freshInstall      bool     // no agentop in binDir before this run
	installedVersion  string   // what binDir/agentop reported before this run, from readVersions
	stagedVersion     string   // what fromDir/agentop reports, from readVersions; "" if it did not answer
	versionsRead      bool     //
	binaryChanges     []string // file names the binaries step replaces
	binOnPath         bool     // binDir is on this shell's PATH
	binOnPathNewTerms bool     // binDir is, or will be, on PATH in new terminals
	pathProfile       string   // the profile the PATH step edits
	configFresh       bool     // no config.yaml before this run
	configPinsPending bool     // the migration adds listener pins, which a running proxy takes only on a restart
	unsupervised      bool     // the proxy runs without launchd/systemd
	priorService      bool     // a supervised Cortex was serving before this run

	// Set while applying.
	restorePrior func() error // brings back the Cortex that served before, after a rollback
	priorDesc    string
	priorManual  string   // what to run if restorePrior fails, which it may set as it fails; "" is agentop service restart
	onSuccess    []func() // run once every step has applied

	configPinsChanged bool // the config step added listener pins, so the service must restart
	startedBackground bool // the service step started a background proxy, so the ending says how to stop it
}

// readVersions asks the installed agentop, and under --from the staged one, for
// its version, once a run: the header and the binaries plan both show them, and
// each ask runs a binary.
func (e *setupEnv) readVersions() {
	if e.versionsRead {
		return
	}
	e.versionsRead = true
	e.installedVersion = installedVersion(filepath.Join(e.binDir, "agentop"))
	if e.fromDir != "" {
		e.stagedVersion = installedVersion(filepath.Join(e.fromDir, "agentop"))
	}
}

// newVersion is the version this run installs: the staged agentop's when it
// answered, else this binary's own — an installed agentop running setup --from
// is the old version, not the one it installs.
func (e *setupEnv) newVersion() string {
	if e.stagedVersion != "" {
		return e.stagedVersion
	}
	return version
}

// rerun is the clause a remedy ends on, naming what to run again once the user has
// acted on it: the installer, whose stage setup deletes on its way out, or else the
// command this setup was run as. Spelled out, because that is often not what the user
// typed: `make dev-install` runs setup, and a bare "re-run" left them to guess which
// (#1282).
func (e *setupEnv) rerun() string {
	if e.opts.fromInstaller() {
		return "re-run the installer"
	}
	if len(e.argv) == 0 {
		return "re-run agentop setup"
	}
	words := make([]string, len(e.argv))
	for i, a := range e.argv {
		if filepath.IsAbs(a) {
			words[i] = e.shellPath(a)
		} else {
			words[i] = shellQuote(a)
		}
	}
	return "re-run: " + strings.Join(words, " ")
}

func (e *setupEnv) configPath() string  { return filepath.Join(e.cortexDir, "config.yaml") }
func (e *setupEnv) previousDir() string { return filepath.Join(e.cortexDir, "previous") }

// tilde shows a path under HOME as ~/…, as every line setup prints does.
func (e *setupEnv) tilde(p string) string {
	rel, err := filepath.Rel(e.home, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	return filepath.Join("~", rel)
}

// shellPath is p as a fix line's command takes it: ~/… when the rest needs no
// quoting, so the shell expands the ~; else the whole path, quoted.
func (e *setupEnv) shellPath(p string) string {
	t := e.tilde(p)
	if rest, under := strings.CutPrefix(t, "~/"); (under && shellQuote(rest) == rest) || shellQuote(t) == t {
		return t
	}
	return shellQuote(p)
}

// tildeText is s with each path under HOME shown as ~/…, for the lines setup
// passes on from elsewhere: a log, or service install's own messages. HOME counts
// only as a whole path, so /Users/al does not shorten /Users/alice, nor the
// /Users/al inside /Users/al/Users/al. HOME is taken cleaned, as tilde takes it.
func (e *setupEnv) tildeText(s string) string {
	home := filepath.Clean(e.home)
	if e.home == "" || home == "/" {
		return s
	}
	var b strings.Builder
	done := 0 // s[:done] is written
	for at := 0; ; {
		i := strings.Index(s[at:], home)
		if i < 0 {
			break
		}
		i += at
		j := i + len(home)
		// The bytes either side, read from s itself: a match right after the last
		// one is not at the start of a path.
		if (i == 0 || !isPathByte(s[i-1])) && (j == len(s) || s[j] == '/' || !isPathByte(s[j])) {
			b.WriteString(s[done:i] + "~")
			done, at = j, j
		} else {
			at = i + 1
		}
	}
	b.WriteString(s[done:])
	return b.String()
}

// isPathByte reports whether c can be part of a path's name, so that HOME next to
// it is not a whole path. Bytes of 0x80 and up are: they spell a non-ASCII name.
func isPathByte(c byte) bool {
	return c == '/' || c == '.' || c == '_' || c == '-' || c >= 0x80 ||
		'0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

// stepPlan is what one step would do, worked out before anything changes.
type stepPlan struct {
	label   string   // the checklist label
	verb    string   // consent row: install, replace, add, write, update, run, route, remove
	what    string   //
	where   string   //
	done    bool     // nothing to change: a dim "·" line, or "!" when advice is set
	doneMsg string   //
	advice  *problem // a non-fatal note shown in place of the "·" line
	hidden  bool     // nothing to change and nothing worth a line; set done too, as allDone and consentItems read only done
}

// problem is why a step cannot go ahead, or what the user should do, with the remedy.
type problem struct {
	label  string
	reason string
	fix    []string
}

// undo reverses one applied step; fn is nil when there is nothing to reverse.
type undo struct {
	label  string
	fn     func() error
	manual string // what the user can run or do by hand if fn fails
}

// step is one unit of agentop setup. plan must not change anything on disk. apply
// returns the undo for whatever it changed — also when it fails part way, so the
// runner can reverse the part that happened. Its detail's first line goes on the
// ✓ line; each line after it is a row of its own below.
type step interface {
	name() string
	plan(env *setupEnv) (stepPlan, *problem)
	apply(env *setupEnv, act *checklist.Running) (detail string, u undo, err error)
}

// alreadyStep is a step with bookkeeping to finish even when its plan found
// nothing to change — a record to top up, say. applySteps calls already on the
// done path, where apply never runs, and renders what it returns as faint rows
// under the "·" line, exactly as it does the lines after a detail's first.
//
// Deliberately NOT part of step, and deliberately not reachable from plan: plan
// must not change anything on disk, and `agentop doctor` plans every step to
// report on it. Only setup applies.
type alreadyStep interface {
	already(env *setupEnv) string
}

// finishAlready lets a done step finish its own bookkeeping and draws whatever it
// says about it as faint rows, as applySteps draws the lines after a detail's
// first. It reports whether it drew any.
//
// Called from the two places a done step is rendered and its apply does not run:
// applySteps' done path, and runSetup's own all-done branch, which returns before
// applySteps is reached at all — and which is the whole of a re-run that finds
// nothing to change. Not from anywhere `agentop doctor` goes.
func finishAlready(env *setupEnv, ui *checklist.UI, s step) (drew bool) {
	a, ok := s.(alreadyStep)
	if !ok {
		return false
	}
	for _, l := range strings.Split(a.already(env), "\n") {
		if l != "" {
			ui.Faint("    " + l)
			drew = true
		}
	}
	return drew
}

type plannedStep struct {
	s step
	p stepPlan
}

// planSteps plans every step, carrying on past a problem so the user sees all of
// them at once.
func planSteps(env *setupEnv, steps []step) ([]plannedStep, []*problem) {
	var out []plannedStep
	var probs []*problem
	for _, s := range steps {
		p, prob := s.plan(env)
		if p.label == "" {
			p.label = s.name()
		}
		if prob != nil {
			if prob.label == "" {
				prob.label = p.label
			}
			probs = append(probs, prob)
			continue
		}
		out = append(out, plannedStep{s, p})
	}
	return out, probs
}

func allDone(ps []plannedStep) bool {
	for _, p := range ps {
		if !p.p.done {
			return false
		}
	}
	return true
}

func consentItems(ps []plannedStep) []checklist.Item {
	var items []checklist.Item
	for _, p := range ps {
		if !p.p.done {
			items = append(items, checklist.Item{Verb: p.p.verb, What: p.p.what, Where: p.p.where})
		}
	}
	return items
}

// applySteps applies each planned change in order. After a failure, or a signal
// between steps, it undoes what it applied in reverse and brings back the Cortex
// that was serving before. It reports whether every planned change applied. A
// signal is seen only once the running step finishes, so a Ctrl-C during a slow
// one, such as the service's health wait, shows nothing until then.
func applySteps(env *setupEnv, ui *checklist.UI, ps []plannedStep, interrupted <-chan os.Signal) bool {
	var undos []undo
	for _, p := range ps {
		switch {
		case p.p.hidden:
			continue
		case p.p.done:
			if p.p.advice != nil {
				ui.Advise(p.p.label, p.p.advice.reason, p.p.advice.fix...)
			} else {
				ui.Already(p.p.label, p.p.doneMsg)
			}
			// A done step still gets to finish its own bookkeeping, since its apply
			// does not run: this is the path every re-run takes for the steps it has
			// nothing to change about.
			finishAlready(env, ui, p.s)
			continue
		}
		act := ui.Start(p.p.label)
		detail, u, err := p.s.apply(env, act)
		if u.label == "" {
			u.label = p.p.label
		}
		if u.fn != nil {
			undos = append(undos, u)
		}
		if err != nil {
			var se stepError
			if errors.As(err, &se) {
				act.Fail(se.reason, se.detail...)
			} else {
				reason, rest := errorLines(err)
				act.Fail(reason, rest...)
			}
			rollback(env, ui, undos)
			return false
		}
		// Lines after the detail's first are what the step passed on, a warning say:
		// faint rows at Fail's detail indent of six (Faint adds two spaces).
		detail, notes, _ := strings.Cut(detail, "\n")
		act.Done(detail)
		for _, l := range strings.Split(notes, "\n") {
			if l != "" {
				ui.Faint("    " + l)
			}
		}
		select {
		case <-interrupted:
			ui.Blank()
			ui.Plain("Interrupted.")
			rollback(env, ui, undos)
			return false
		default:
		}
	}
	return true
}

// errorLines splits an error into its first non-empty line, the reason, and the
// non-empty lines after it, so a joined error prints as one marked line with its
// detail indented below.
func errorLines(err error) (string, []string) {
	var lines []string
	for _, l := range strings.Split(err.Error(), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return "", nil
	}
	return lines[0], lines[1:]
}

// rollback runs the undos newest first, then restarts the Cortex that was serving
// before — after the undos, because the binaries step's undo runs last and the old
// service needs its binary back first. Each undo that works gets a dim line; each
// that fails is listed with its error and, where known, the manual fix.
func rollback(env *setupEnv, ui *checklist.UI, undos []undo) {
	type failure struct {
		label, manual string
		err           error
	}
	var failed []failure
	for i := len(undos) - 1; i >= 0; i-- {
		if err := undos[i].fn(); err != nil {
			failed = append(failed, failure{undos[i].label, undos[i].manual, err})
		} else {
			ui.Note("undone", undos[i].label)
		}
	}
	if env.restorePrior != nil {
		if err := env.restorePrior(); err != nil {
			manual := env.priorManual
			if manual == "" {
				manual = "agentop service restart"
			}
			failed = append(failed, failure{"the previous Cortex", manual, err})
		} else {
			ui.Note("reverted", env.priorDesc)
		}
	}
	if len(failed) == 0 {
		if len(undos) == 0 && env.restorePrior == nil {
			// Nothing had been applied that needed reversing.
			ui.Note("rolled back", "nothing had been changed")
		} else {
			ui.Note("rolled back", "the changes above are undone")
		}
		return
	}
	for _, f := range failed {
		reason, rest := errorLines(f.err)
		ui.Note("could not roll back", f.label+": "+reason)
		// Faint adds two spaces: these sit at Fail's detail indent of six.
		for _, l := range rest {
			ui.Faint("    " + l)
		}
		if f.manual != "" {
			ui.Remedy("do it yourself: ", f.manual)
		}
	}
}
