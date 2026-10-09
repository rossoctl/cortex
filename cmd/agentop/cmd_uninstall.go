package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rossoctl/cortex/cmd/agentop/checklist"
)

const uninstallUsage = `agentop uninstall — remove Cortex from this machine, after one question

Usage:
  agentop uninstall [--yes] [--purge]

Unroutes Claude Code, OpenCode, IBM Bob and the bob shell function where Cortex
routed them; stops and removes the service, or the background proxy; removes the PATH
lines setup added, unless other tools in ~/.local/bin still need them; and
removes agentop, cortex and cortex-session-dump from ~/.local/bin. ~/.cortex
stays unless --purge. What to remove is read from disk, not from a record of
the install. A removal that fails is reported with its fix and the rest still
run, but ~/.cortex then stays; the end lists what was left behind.

  --yes, -y   do not ask; needed when there is no terminal
  --purge     delete ~/.cortex too: config, CA, logs, usage and session history

Exit status: 0 removed, or nothing to remove; 1 something was left behind;
2 usage; 3 declined, or no terminal to ask on.
`

type uninstallOptions struct{ yes, purge bool }

// removal is one thing uninstall takes away. run returns its ✓ detail, and
// anything it could not remove as the line the ending lists under "Left behind".
// A non-nil err marks the row ✗, with a fix: line for each of its remedies: the
// ones the error carries, from withFixes, or else fix. The run still carries on
// to the next removal.
type removal struct {
	label        string         // checklist label: "unrouted", "stopped", "PATH", "removed", "purged", "bob shell"
	item         checklist.Item // the consent row
	run          func(act *checklist.Running) (detail string, left []string, err error)
	fix          remedy // for a failure whose error carries no remedy of its own
	dropsAgentop bool   // it removes agentop, so a fix before it cannot use agentop
	unlessFailed bool   // skipped, and left behind, once a removal before it has failed
	keep         *kept  // set on a row that removes nothing: it has no consent row and no run
}

// kept is a thing uninstall leaves on purpose, and why, as it leaves ~/.cortex
// without --purge: it is not left behind, and does not change the exit status.
type kept struct{ what, why string }

// remedy finishes by hand what a removal could not: with agentop, which puts
// back what Cortex recorded, while agentop stays installed; and without it, as
// uninstall removes agentop near the end. byHand is never empty; a fix done in
// steps has a line for each, each drawn as a fix: row.
type remedy struct{ agentop, byHand string }

// manual is a remedy that needs no agentop.
func manual(cmd string) remedy { return remedy{byHand: cmd} }

// line is the remedy as its fix: line prints it, given whether agentop will still
// be there to run it.
func (r remedy) line(agentop bool) string {
	if agentop && r.agentop != "" {
		return r.agentop
	}
	return r.byHand
}

// unfinished is a removal's error with the remedies for what it left.
type unfinished struct {
	err   error
	fixes []remedy
}

func (u *unfinished) Error() string { return u.err.Error() }
func (u *unfinished) Unwrap() error { return u.err }

// withFixes is err carrying fixes, the remedies for what it left: nil when err is.
func withFixes(err error, fixes ...remedy) error {
	if err == nil {
		return nil
	}
	return &unfinished{err: err, fixes: fixes}
}

// fixesFor are the remedies for err, the failure of r's run.
func (r removal) fixesFor(err error) []remedy {
	var u *unfinished
	if errors.As(err, &u) && len(u.fixes) > 0 {
		return u.fixes
	}
	return []remedy{r.fix}
}

// uninstallRemovalsHook lets tests inject a failure into the real removal list.
var uninstallRemovalsHook = func(r []removal) []removal { return r }

// uninstallFindOpenCode finds the opencode CLI the OpenCode row changes OpenCode
// with. A var so a test names a fake one, or none: no test may run the real one.
var uninstallFindOpenCode = findOpenCode

// uninstallSignals is the channel Ctrl-C and SIGTERM arrive on, and the func that
// stops them arriving: setupSignals' seam, for uninstall's tests.
var uninstallSignals = func() (<-chan os.Signal, func()) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	return c, func() { signal.Stop(c) }
}

// uninstallAnimate reports whether the screen animates on w: on a terminal. A var
// so a test can see the cursor the spinner hides come back.
var uninstallAnimate = isTerminal

// uninstallConfirm asks uninstall's one question, on /dev/tty as setup asks
// its own. A var for the reason claudeCodeConfirm gives.
var uninstallConfirm = func(w io.Writer) bool {
	tty, err := os.Open("/dev/tty")
	if err != nil {
		return false
	}
	defer func() { _ = tty.Close() }()
	return uninstallConfirmFrom(tty, w)
}

// uninstallConfirmFrom reads the answer. Only y or yes goes ahead: unlike setup's
// question, this one deletes, so Enter and EOF are no.
func uninstallConfirmFrom(r io.Reader, w io.Writer) bool {
	_, _ = fmt.Fprint(w, "  Continue? [y/N] ")
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// parseUninstallFlags parses uninstall's flags. -h or --help is flag.ErrHelp; any
// other error has been written to stderr with the usage.
func parseUninstallFlags(args []string, stderr io.Writer) (uninstallOptions, error) {
	var o uninstallOptions
	fs := newFlagSet("uninstall", stderr)
	fs.Usage = func() {} // printed from Parse's result, on stdout for -h, as setup's is
	fs.BoolVar(&o.yes, "yes", false, "")
	fs.BoolVar(&o.yes, "y", false, "")
	fs.BoolVar(&o.purge, "purge", false, "")
	if err := fs.Parse(args); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprint(stderr, uninstallUsage) // under Parse's own line
		}
		return o, err
	}
	if fs.NArg() > 0 {
		err := fmt.Errorf("unexpected argument %q", fs.Arg(0))
		_, _ = fmt.Fprintf(stderr, "agentop: %v\n%s", err, uninstallUsage)
		return o, err
	}
	return o, nil
}

// runUninstall plans every removal from what is on disk, asks once, then runs
// each, carrying on past a failure.
//
// Ctrl-C and SIGTERM are caught, as setup catches them, rather than killing
// uninstall: dying of one would skip the deferred Close and leave the cursor the
// spinner hid hidden. Before the removals a signal is a decline, as at setup's
// prompt. During them, the removal running finishes, as there is nothing to roll
// back, and the rest are skipped and listed as left behind.
func runUninstall(args []string, stdout, stderr io.Writer) int {
	opts, err := parseUninstallFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		_, _ = fmt.Fprint(stdout, uninstallUsage)
		return 0
	}
	if err != nil {
		return 2
	}
	sigs, stopSignals := uninstallSignals()
	defer stopSignals()
	env, err := newSetupEnv(setupOptions{})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "agentop: %v\n", err)
		return 1
	}
	ui := checklist.New(stdout, uninstallAnimate(stdout))
	defer ui.Close()
	ui.Header("rosso cortex · uninstall", version+" · "+env.goos+"/"+runtime.GOARCH)
	ui.Blank()

	dir := env.tilde(env.cortexDir)
	_, statErr := os.Lstat(env.cortexDir)
	keeps := statErr == nil && !opts.purge // ~/.cortex is there, and stays
	removals := uninstallRemovalsHook(planUninstall(env, opts))
	var items []checklist.Item
	var kepts []*kept
	for _, r := range removals {
		if r.keep != nil {
			kepts = append(kepts, r.keep)
		} else {
			items = append(items, r.item)
		}
	}
	if len(items) == 0 {
		ui.Plain("Nothing to uninstall.")
		for _, k := range kepts {
			ui.Plain("Kept " + k.what + ": " + k.why)
		}
		if keeps {
			ui.Plain(dir + " is still here (config, CA, logs, usage and session history): rm -rf " + dir)
		}
		return 0
	}
	ui.Consent(items)
	ui.Blank()
	for _, k := range kepts {
		ui.Faint("Kept: " + k.what + ", which other tools need: " + k.why)
	}
	if keeps {
		ui.Faint("Kept: " + dir + " (config, CA, logs, usage and session history) — add --purge to delete it")
	}
	if keeps || len(kepts) > 0 {
		ui.Blank()
	}
	if !opts.yes {
		if !setupHasTerminal() {
			ui.Plain("Not changed: no terminal to ask on. Re-run with --yes.")
			return exitDeclined
		}
		// Read aside, so a Ctrl-C at the prompt declines rather than waiting on the
		// terminal. The reader is left blocked: uninstall exits soon after.
		answer := make(chan bool, 1)
		go func() { answer <- uninstallConfirm(stdout) }()
		select {
		case yes := <-answer:
			if !yes {
				ui.Plain("Not changed.")
				return exitDeclined
			}
		case <-sigs:
			ui.Blank() // ends the prompt's line
			ui.Plain("Not changed.")
			return exitDeclined
		}
		ui.Blank()
	}
	select {
	case <-sigs: // one while planning, under --yes; the prompt reads its own
		ui.Plain("Not changed.")
		return exitDeclined
	default:
	}

	left, failed, interrupted := applyRemovals(env, ui, removals, sigs)
	ui.Blank()
	if interrupted {
		ui.Plain("Interrupted.")
	}
	if failed || len(left) > 0 {
		if len(left) > 0 {
			ui.Plain("Left behind:")
			for _, l := range left {
				ui.Plain("  " + l)
			}
		}
		return 1
	}
	ui.Plain("Uninstalled.")
	if keeps {
		// agentop is gone by now, so the line names a shell command, not one of its own.
		ui.Plain("Kept " + dir + ". Delete it with: rm -rf " + dir)
	}
	return 0
}

// applyRemovals runs each removal in order, never stopping at a failure, and
// draws its row: running while it runs, on a terminal; then ✓ with its detail, !
// when it left something behind without failing, ✗ with its error and a fix:
// line for each remedy. A detail's lines after the first are faint rows under it.
// A signal stops it once the removal running has finished: the ones after it are
// left, and listed as such.
//
// A fix uses agentop only when agentop is in the bin dir and no removal still to
// run takes it away: the user reads the fix once uninstall has finished.
func applyRemovals(env *setupEnv, ui *checklist.UI, removals []removal, sigs <-chan os.Signal) (left []string, failed, interrupted bool) {
	for i, r := range removals {
		if i > 0 {
			select {
			case <-sigs:
				for _, s := range removals[i:] {
					if s.keep == nil {
						left = append(left, "not removed (interrupted): "+s.label+" "+s.item.What)
					}
				}
				return left, failed, true
			default:
			}
		}
		if r.keep != nil {
			ui.Already(r.label, "kept "+r.keep.what+": "+r.keep.why)
			continue
		}
		if r.unlessFailed && failed {
			ui.Advise(r.label, "kept "+r.item.What+", as a removal above failed")
			left = append(left, r.item.What+" — once the fixes above are done: "+r.fix.byHand)
			continue
		}
		act := ui.Start(r.label)
		detail, l, err := r.run(act)
		left = append(left, l...)
		if err != nil {
			failed = true
			reason, rest := errorLines(err)
			for j := range rest {
				rest[j] = env.tildeText(rest[j])
			}
			act.Fail(env.tildeText(reason), rest...)
			agentop := isExecutable(filepath.Join(env.binDir, "agentop")) &&
				!slices.ContainsFunc(removals[i+1:], func(later removal) bool { return later.dropsAgentop })
			for _, f := range r.fixesFor(err) {
				for _, l := range strings.Split(f.line(agentop), "\n") { // one fix, in steps
					ui.Remedy("fix: ", l)
				}
			}
			continue
		}
		first, notes, _ := strings.Cut(detail, "\n")
		if len(l) > 0 {
			act.Advise(first)
		} else {
			act.Done(first)
		}
		for _, n := range strings.Split(notes, "\n") {
			if n != "" {
				ui.Faint("    " + n)
			}
		}
	}
	return left, failed, false
}

// planUninstall is every removal whose thing is on disk, in the spec's order.
// Each one's inputs are read now, before anything runs: --purge deletes
// ~/.cortex last, and the Claude Code plan and bobWanted read its state record
// and config, as the service paths read the config. A run uses what its plan read.
func planUninstall(env *setupEnv, opts uninstallOptions) []removal {
	rs := planUnrouteClaudeCode(env)
	for _, plan := range []func(*setupEnv) (removal, bool){planUnrouteOpenCode, planUnrouteBob, planRemoveBobShell, planStopService} {
		if r, ok := plan(env); ok {
			rs = append(rs, r)
		}
	}
	rs = append(rs, planRemovePATH(env)...)
	if r, ok := planRemoveBinaries(env); ok {
		rs = append(rs, r)
	}
	if opts.purge {
		if r, ok := planPurge(env); ok {
			rs = append(rs, r)
		}
	}
	return rs
}

// planUnrouteClaudeCode is a removal for each settings file Cortex routed: the
// one enable's record names, which --settings may have made a project's, and
// ~/.claude/settings.json when its own HTTPS_PROXY is Cortex's. The record is read
// now. A file the record does not name is unrouted only when its proxy is
// Cortex's: disable without the record deletes every managed key, a corporate
// proxy and CA included.
func planUnrouteClaudeCode(env *setupEnv) []removal {
	state, def := filepath.Join(env.home, stateRel), filepath.Join(env.home, settingsRel)
	named := recordedSettings(env)
	var rs []removal
	if named != "" {
		rs = append(rs, unrouteClaudeCode(env, named, state))
	}
	if (named == "" || !samePath(named, def)) && cortexProxyIn(env, def) {
		// Without a record of its own: it restores nothing, and leaves the record
		// alone unless the record names no file, as one that cannot be read does.
		record := ""
		if named == "" {
			record = state
		}
		rs = append(rs, unrouteClaudeCode(env, def, record))
	}
	return rs
}

// unrouteClaudeCode is disable for one settings file, planned now. state is the
// record disable restores from and then deletes, or "" for none. The record is
// read as the removal runs, which is first, before --purge could delete it.
func unrouteClaudeCode(env *setupEnv, settings, state string) removal {
	pl, planErr := planClaudeCodeDisable(settings, state)
	where := ""
	if settings != filepath.Join(env.home, settingsRel) {
		where = " · " + env.tilde(settings)
	}
	keys := managedKeys // which are set is unknown when the file cannot be read
	if planErr == nil && len(pl.present)+len(pl.left) > 0 {
		// Only the keys disable itself would change: a left key goes in its own
		// line below, as removing it is the user's call and not part of the fix.
		keys = pl.present
	}
	disable := remedy{
		agentop: "agentop configure claude-code disable --settings " + env.shellPath(settings),
		byHand:  unrouteByHand(env, settings, state, keys, pl.left),
	}
	// Keys whose value is no longer the one enable wrote are the user's edit, so
	// disable leaves them: they are this row's "Left behind" rather than its ✗.
	leftNote := claudeCodeLeftNote(env, settings, pl.left)
	return removal{
		label: "unrouted",
		item:  checklist.Item{Verb: "unroute", What: "Claude Code", Where: env.tilde(settings)},
		fix:   disable,
		run: func(*checklist.Running) (string, []string, error) {
			if planErr != nil {
				return "", []string{env.tilde(settings)}, planErr
			}
			if len(pl.present) == 0 {
				// Keys the file holds but whose values Cortex did not write: nothing
				// of ours to take out, and the record stays — it is the only note of
				// what they held before enable, and they are still set.
				if len(pl.left) > 0 {
					return "left " + someKeys(pl.left) + " as they are" + where, leftNote, nil
				}
				// The record of a routing the file no longer holds, the file deleted
				// say. applyClaudeCodeDisable touches nothing then, so the record goes
				// here: left, doctor reads Claude Code as routed for good.
				if state == "" {
					return "nothing routed in " + env.tilde(settings), nil, nil
				}
				if err := removeIfExists(state); err != nil {
					return "", []string{env.tilde(state)}, withFixes(err, manual("rm "+env.shellPath(state)))
				}
				return "removed Cortex's record (settings had nothing routed)", nil, nil
			}
			var errb bytes.Buffer
			restored, err := applyClaudeCodeDisable(pl, state, &errb)
			if err != nil {
				return "", []string{env.tilde(settings)}, err
			}
			detail := "Claude Code no longer goes through Cortex" + where
			if len(restored) > 0 {
				detail += " · restored your " + strings.Join(restored, ", ")
			}
			// Its warning, a record it could not read, as rows below.
			return detail + stderrNotes(env, errb.String()), leftNote, nil
		},
	}
}

// claudeCodeLeftNote is uninstall's "Left behind" line for the keys disable did not
// touch. Keys and the file, no values, as unrouteByHand names none: this text is
// printed at the end of a run and gets pasted.
func claudeCodeLeftNote(env *setupEnv, settings string, left []string) []string {
	if len(left) == 0 {
		return nil
	}
	return []string{someKeys(left) + " in " + env.tilde(settings) +
		", changed since Cortex set them — remove them by hand if you want them gone"}
}

// unrouteByHand is disable's edit to settings as fix lines, for when agentop is
// gone: the keys Cortex set, to remove; and, when the record holds an earlier
// value for some, those keys, to restore, and where the record is. It names no
// value, the record's least of all: a proxy URL can carry a password, and what a
// terminal shows gets pasted and logged. Each list names three keys, then "…", so
// a line stays near 120 columns. The record is read now, while it is there:
// --purge deletes it.
//
// left are the keys changed since enable, which disable does not touch and this
// does not tell the user to remove either — the value there is theirs, and the
// point of leaving it was not to decide for them. They get a line of their own
// instead, so a by-hand unroute does not quietly stop short of those keys.
func unrouteByHand(env *setupEnv, settings, state string, keys, left []string) string {
	var st *managedState
	if state != "" {
		st, _ = readState(state)
	}
	var remove, restore []string
	for _, k := range keys {
		if st != nil && st.Settings == settings && st.Prior[k] != nil {
			restore = append(restore, k)
		} else {
			remove = append(remove, k)
		}
	}
	file := "in " + env.tilde(settings) + ", "
	var lines []string
	if len(remove) > 0 {
		lines = append(lines, file+`remove the "env" keys `+someKeys(remove))
		file = "and "
	}
	if len(restore) > 0 {
		lines = append(lines, file+"restore "+someKeys(restore)+" — your earlier values are in "+env.tilde(state))
		file = "and "
	}
	if len(left) > 0 {
		lines = append(lines, file+"decide about "+someKeys(left)+" — changed since Cortex set them, so they are yours")
	}
	return strings.Join(lines, "\n")
}

// someKeys names up to three of keys, then "…".
func someKeys(keys []string) string {
	if len(keys) > 3 {
		return strings.Join(keys[:3], ", ") + ", …"
	}
	return strings.Join(keys, ", ")
}

// planUnrouteOpenCode takes Cortex's values out of OpenCode's service environment,
// as configure opencode disable does, when it holds any. It is read now, through
// the opencode CLI, as is enable's record, which --purge deletes. With no CLI there
// is nothing to change it with; one that cannot read it fails the row only when
// that record says Cortex routed OpenCode. A service that runs is restarted after
// the change, as disable restarts it, so it drops the proxy that is about to stop.
func planUnrouteOpenCode(env *setupEnv) (removal, bool) {
	bin, err := uninstallFindOpenCode()
	if err != nil {
		return removal{}, false
	}
	state := filepath.Join(env.home, opencodeStateRel)
	want := map[string]string{}
	if w, _, err := wantedFromConfig(env.configPath()); err == nil {
		want = openCodeValues(w)
	}
	// The CLI is judged by Cortex's values, as runOpenCode has it judged.
	openCodeCLIWant = want
	defer func() { openCodeCLIWant = nil }()
	pl, planErr := planOpenCodeDisable(bin, state, want)
	if (planErr != nil && !fileExists(state)) || (planErr == nil && len(pl.present) == 0) {
		return removal{}, false
	}
	where := "OpenCode's service environment"
	if svc, err := probeOpenCodeService(bin); err != nil {
		where += " · stops the service if it runs, interrupting its sessions"
	} else if svc.Running {
		where += " · restarts the service, interrupting its sessions"
	}
	cli := env.shellPath(bin)
	return removal{
		label: "unrouted",
		item:  checklist.Item{Verb: "unroute", What: "OpenCode", Where: where},
		fix:   remedy{agentop: "agentop configure opencode disable", byHand: openCodeByHand(env, cli, pl, planErr == nil, state)},
		run: func(*checklist.Running) (string, []string, error) {
			openCodeCLIWant = want
			defer func() { openCodeCLIWant = nil }()
			left := []string{"Cortex's values in OpenCode's service environment"}
			if planErr != nil {
				return "", left, planErr
			}
			svc, err := probeOpenCodeService(bin)
			running := err == nil && svc.Running
			var errb bytes.Buffer
			restored, _, err := applyOpenCodeDisable(pl, state, &errb)
			if err != nil {
				if cause := errors.Unwrap(err); cause != nil {
					err = cause // not disable's "run agentop configure opencode disable again": agentop goes
				}
				return "", left, err
			}
			detail := "OpenCode's service environment no longer routes it through Cortex"
			if len(restored) > 0 {
				detail += " · restored your " + strings.Join(restored, ", ")
			}
			notes := stderrNotes(env, errb.String())
			if running {
				if _, err := openCodeRun(bin, "service", "restart"); err != nil {
					return detail + notes, []string{"OpenCode's service, which may still run with Cortex's proxy — restart it: " + cli + " service restart"}, nil
				}
				detail += " · restarted its service"
			}
			return detail + notes, nil, nil
		},
	}, true
}

// openCodeByHand is the OpenCode row's change as fix lines, for when agentop is
// gone: a line per key to unset, or to set back where enable's record holds an
// earlier value, then the restart. It names no value, as unrouteByHand names none.
// Without a plan, from an environment the CLI could not read, which keys hold
// Cortex's values is for the user to see.
func openCodeByHand(env *setupEnv, cli string, pl openCodeDisablePlan, planned bool, state string) string {
	restart := "then, if OpenCode's service runs: " + cli + " service restart"
	if !planned {
		return "see which of " + strings.Join(openCodeKeys, ", ") + " hold Cortex's values: " + cli + " service get env\n" +
			"and unset each of those: " + cli + " service unset env <name>\n" + restart
	}
	var lines []string
	for _, k := range pl.present {
		if openCodePrior(pl.st, k) != nil {
			lines = append(lines, "set "+k+" back to your earlier value, in "+env.tilde(state)+": "+cli+" service set env "+k+" <value>")
		} else {
			lines = append(lines, cli+" service unset env "+k)
		}
	}
	return strings.Join(append(lines, restart), "\n")
}

// planUnrouteBob removes IBM Bob's proxy setting when it may be Cortex's: one
// bobOwns does not call someone else's. Bob's disable makes the edit, and its
// refusals are this row's ✗.
func planUnrouteBob(env *setupEnv) (removal, bool) {
	settings, err := bobSettingsPath(env.home)
	if err != nil || !fileExists(settings) {
		return removal{}, false
	}
	doc, err := readSettings(settings)
	if err != nil {
		return removal{}, false
	}
	val, isString := doc[bobProxyKey].(string)
	wantProxy, caPath := bobWanted(env.configPath(), env.home)
	if !isString || bobOwns(val, wantProxy) == bobNotOurs {
		return removal{}, false
	}
	cfg := env.configPath()
	return removal{
		label: "unrouted",
		item:  checklist.Item{Verb: "unroute", What: "IBM Bob", Where: env.tilde(settings)},
		fix:   remedy{agentop: "agentop configure bob disable", byHand: "remove \"" + bobProxyKey + "\" from " + env.tilde(settings)},
		run: func(*checklist.Running) (string, []string, error) {
			var out, errb bytes.Buffer
			if bobDisable(settings, cfg, wantProxy, caPath, true, &out, &errb) != 0 {
				return "", []string{env.tilde(settings)}, errors.New(lastLineOf(errb.String(), out.String()))
			}
			return "IBM Bob no longer goes through Cortex · restart Bob", nil, nil
		},
	}, true
}

// planRemoveBobShell removes the bob shell function when the rc file holds the
// block enable writes exactly once. A block that is not that, edited or there
// twice, is the user's: bobshell disable leaves it, so this only says so, with a
// ! row and the lines to delete by hand. The file is followed through one link,
// as bobshell's verbs follow it, so the edit lands in a dotfiles repo's copy
// rather than replacing the link.
func planRemoveBobShell(env *setupEnv) (removal, bool) {
	rc, err := bobShellRCPath(env.shell, env.home)
	if err != nil {
		return removal{}, false
	}
	target, hops, err := rcTarget(rc)
	if err != nil || hops > maxRCSymlinkHops {
		return removal{}, false
	}
	b, err := os.ReadFile(target) //nolint:gosec // the user's own shell rc file
	if err != nil || !strings.Contains(string(b), bobShellMarkerStart) {
		return removal{}, false
	}
	if strings.Count(string(b), bobShellBlock) != 1 {
		return removal{
			label: "bob shell",
			item:  checklist.Item{Verb: "leave", What: "the bob shell function", Where: env.tilde(rc) + " (not as enable wrote it)"},
			run: func(*checklist.Running) (string, []string, error) {
				return "the bob shell function in " + env.tilde(rc) + " is not as enable wrote it; left as it is",
					[]string{"the bob shell function in " + env.tilde(rc) + " — delete the lines from \"" +
						bobShellMarkerStart + "\" to \"" + bobShellMarkerEnd + "\""}, nil
			},
		}, true
	}
	return removal{
		label: "removed",
		item:  checklist.Item{Verb: "remove", What: "the bob shell function", Where: env.tilde(rc)},
		fix: remedy{agentop: "agentop configure bobshell disable",
			byHand: "delete the lines from \"" + bobShellMarkerStart + "\" to \"" + bobShellMarkerEnd + "\" in " + env.tilde(rc)},
		run: func(*checklist.Running) (string, []string, error) {
			var out, errb bytes.Buffer
			if bobShellDisable(target, true, &out, &errb) != 0 {
				return "", []string{env.tilde(rc)}, errors.New(lastLineOf(errb.String(), out.String()))
			}
			return "the bob shell function from " + env.tilde(rc), nil, nil
		},
	}, true
}

// lastLineOf is the last non-empty line of stderr, or of stdout when stderr has
// none, without agentop's "agentop: " prefix.
func lastLineOf(stderr, stdout string) string {
	for _, s := range []string{stderr, stdout} {
		if lines := trimAll(strings.Split(s, "\n")); len(lines) > 0 {
			return strings.TrimPrefix(lines[len(lines)-1], "agentop: ")
		}
	}
	return "it did not say why"
}

// planStopService stops and removes the service when its unit is installed, and
// the background proxy proxy.pid records when one runs: both, when both are
// there. The service paths and the proxy's pid are read now, from the config
// --purge deletes.
func planStopService(env *setupEnv) (removal, bool) {
	pidFile := filepath.Join(env.cortexDir, "proxy.pid")
	sp, spErr := resolveServicePaths(env.configPath(), "", filepath.Join(env.binDir, "cortex"))
	installed := spErr == nil && serviceInstalled(sp)
	pid, background := preRenameProxyRunning(env.binDir, pidFile)
	if !background {
		pid, background = proxyRunning(pidFile)
	}
	if !installed && !background {
		return removal{}, false
	}
	item := checklist.Item{Verb: "stop and remove", What: "the Cortex service", Where: env.tilde(sp.unitFile)}
	if !installed {
		item.What, item.Where = "the background Cortex", env.tilde(pidFile)
	}
	sup := supervisorName(env.goos)
	unload := unloadByHand(env, sp.unitFile) // it removes the unit too
	stillLoaded := "the " + sup + " job is still loaded — stop it with: " + unload
	kill := "kill " + strconv.Itoa(pid)
	fix := manual(unload)
	if !installed {
		fix = manual(kill)
	}
	return removal{label: "stopped", item: item, fix: fix, run: func(act *checklist.Running) (string, []string, error) {
		var done, left []string
		var errs []error
		var fixes []remedy
		if installed {
			act.Wait(strings.Fields(sup)[0]+" to unload Cortex", serviceBootoutTimeout)
			unloadErr, unitErr, stampErr := removeServiceReport(sp)
			loadedErr := unloadErr
			switch {
			case unloadErr == nil:
				// Before the next row: bootout returns while launchd is still tearing
				// the job down.
				loadedErr = waitUnloaded(env.goos)
			case !serviceLoaded(env.goos):
				loadedErr = nil // nothing was loaded to unload: a stopped service, say
			}
			if loadedErr != nil {
				left, errs, fixes = append(left, stillLoaded), append(errs, loadedErr), append(fixes, manual(unload))
			}
			if unitErr != nil {
				left = append(left, env.tilde(sp.unitFile))
				errs = append(errs, fmt.Errorf("could not remove %s: %w", sp.unitFile, unitErr))
				if loadedErr == nil {
					fixes = append(fixes, manual("rm "+env.shellPath(sp.unitFile)))
				}
			}
			if stampErr != nil {
				left = append(left, env.tilde(sp.stampFile))
				errs = append(errs, fmt.Errorf("could not remove %s: %w", sp.stampFile, stampErr))
				fixes = append(fixes, manual("rm "+env.shellPath(sp.stampFile)))
			}
			done = append(done, sup+" job removed")
		}
		if background {
			act.Wait("the background Cortex to stop", stopPIDTimeout)
			if err := stopIfAlive(pid); err != nil {
				left, fixes = append(left, kill), append(fixes, manual(kill))
				errs = append(errs, fmt.Errorf("could not stop the background Cortex (pid %d): %w", pid, err))
			} else if err := removeIfExists(pidFile); err != nil {
				left, fixes = append(left, env.tilde(pidFile)), append(fixes, manual("rm "+env.shellPath(pidFile)))
				errs = append(errs, err)
			} else {
				done = append(done, "background Cortex stopped")
			}
		}
		return strings.Join(done, " · "), left, withFixes(errors.Join(errs...), fixes...)
	}}, true
}

// stopPIDTimeout is how long stopPID waits for a proxy to go: 90 polls 200ms apart.
const stopPIDTimeout = 18 * time.Second

// stopIfAlive stops pid. One that is gone by the time it is signalled, or as
// stopPID fails, is stopped too. Never 0 or less: kill(2) reads those as a
// process group.
func stopIfAlive(pid int) error {
	if pid <= 0 {
		return nil
	}
	if err := stopPID(pid); err != nil && alive(pid) {
		return err
	}
	return nil
}

// pathProfiles are the profiles install.sh and setup's PATH step may have edited.
var pathProfiles = []string{".zshrc", ".bash_profile", ".bashrc", ".profile"}

// planRemovePATH removes the PATH lines from each profile that holds the marker,
// once per file: profiles linked to one another are one file. While the bin dir
// holds another tool's executable, Claude Code's native installer's claude say,
// it keeps them instead: without them that tool is off PATH too.
func planRemovePATH(env *setupEnv) []removal {
	var rs []removal
	seen := map[string]bool{}
	block := pathBlock(env.binDir)
	others := othersInBinDir(env)
	if len(others) > 3 {
		others = append(others[:3], "…")
	}
	for _, name := range pathProfiles {
		prof := filepath.Join(env.home, name)
		if !hasPathMarker(prof) {
			continue
		}
		// Through its links, as the PATH step writes: the lines are in the target.
		target, err := filepath.EvalSymlinks(prof)
		if err != nil || seen[target] {
			continue
		}
		seen[target] = true
		if len(others) > 0 {
			rs = append(rs, removal{label: "PATH", keep: &kept{
				what: "the PATH lines in " + env.tilde(prof),
				why:  env.tilde(env.binDir) + " also holds " + strings.Join(others, ", "),
			}})
			continue
		}
		rs = append(rs, removal{
			label: "PATH",
			item:  checklist.Item{Verb: "remove", What: "the PATH lines", Where: env.tilde(prof)},
			fix:   manual("delete the two lines marked \"added by Cortex\" from " + env.tilde(prof)),
			run:   func(*checklist.Running) (string, []string, error) { return removePathBlock(env, prof, target, block) },
		})
	}
	return rs
}

// othersInBinDir are the executables in the bin dir that are not Cortex's, by
// name. A link is followed, as Claude Code's native installer links claude to the
// version it keeps elsewhere.
func othersInBinDir(env *setupEnv) []string {
	entries, err := os.ReadDir(env.binDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if n := e.Name(); !cortexsOwn(env, n) && isExecutable(filepath.Join(env.binDir, n)) {
			out = append(out, n)
		}
	}
	return out
}

// cortexsOwn reports whether name in the bin dir is Cortex's: a binary setup
// installs, a pre-rename one by the cleanup step's ownership rule, or a leftover
// of installing one, which no one runs from PATH: make dev-install's <name>.new, a
// <name>.bak, or writeFileAtomic's temp file. A stranger's abctl is not.
func cortexsOwn(env *setupEnv, name string) bool {
	if strings.HasPrefix(name, ".agentop-setup-") {
		return true
	}
	base := strings.TrimSuffix(strings.TrimSuffix(name, ".new"), ".bak")
	switch {
	case slices.Contains(setupBinaryNames, base):
		return true
	case !slices.Contains(staleBinaries, base):
		return false
	}
	return base != name || isOurStaleBinary(filepath.Join(env.binDir, name), name)
}

// removePathBlock takes one copy of block out of target, keeping its mode. A
// marker without the block is lines the user has edited since: theirs now, so
// the file is left alone and the row advises rather than fails.
func removePathBlock(env *setupEnv, prof, target, block string) (string, []string, error) {
	fi, err := os.Stat(target)
	if err != nil {
		return "", []string{env.tilde(prof)}, err
	}
	b, err := os.ReadFile(target) //nolint:gosec // the user's own shell profile
	if err != nil {
		return "", []string{env.tilde(prof)}, err
	}
	if !strings.Contains(string(b), block) {
		return "the PATH lines in " + env.tilde(prof) + " were edited; left as they are",
			[]string{"the PATH lines in " + env.tilde(prof) + " — delete the two lines under \"" + pathMarker + "\""}, nil
	}
	out := strings.Replace(string(b), block, "", 1)
	if err := writeFileAtomic(target, []byte(out), fi.Mode().Perm()); err != nil {
		return "", []string{env.tilde(prof)}, err
	}
	return env.tilde(prof), nil, nil
}

// planRemoveBinaries removes the binaries setup installs that are there, and
// Cortex's own pre-rename ones.
func planRemoveBinaries(env *setupEnv) (removal, bool) {
	var names []string
	for _, n := range setupBinaryNames {
		if _, err := os.Lstat(filepath.Join(env.binDir, n)); err == nil {
			names = append(names, n)
		}
	}
	// The pre-rename ones, by the cleanup step's ownership rule: setup keeps one
	// while a service or background proxy still runs it, and this row runs after
	// the service row has stopped both.
	for _, n := range staleBinaries {
		if isOurStaleBinary(filepath.Join(env.binDir, n), n) {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return removal{}, false
	}
	list := strings.Join(names, ", ")
	rm := func(names []string) remedy {
		args := make([]string, 0, len(names))
		for _, n := range names {
			args = append(args, env.shellPath(filepath.Join(env.binDir, n)))
		}
		return manual("rm " + strings.Join(args, " "))
	}
	return removal{
		label:        "removed",
		item:         checklist.Item{Verb: "remove", What: list, Where: env.tilde(env.binDir)},
		fix:          rm(names),
		dropsAgentop: slices.Contains(names, "agentop"),
		run: func(*checklist.Running) (string, []string, error) {
			var left, kept []string
			var errs []error
			for _, n := range names {
				p := filepath.Join(env.binDir, n)
				if err := removeIfExists(p); err != nil {
					left, kept, errs = append(left, env.tilde(p)), append(kept, n), append(errs, err)
				}
			}
			return list + " from " + env.tilde(env.binDir), left, withFixes(errors.Join(errs...), rm(kept))
		},
	}, true
}

// planPurge deletes ~/.cortex, under --purge only, and last: the removals before
// it were planned from what it held.
func planPurge(env *setupEnv) (removal, bool) {
	if _, err := os.Lstat(env.cortexDir); err != nil {
		return removal{}, false
	}
	dir := env.tilde(env.cortexDir)
	return removal{
		label:        "purged",
		item:         checklist.Item{Verb: "delete", What: dir, Where: "config, CA, logs, usage and session history"},
		fix:          manual("rm -rf " + env.shellPath(env.cortexDir)),
		unlessFailed: true,
		run: func(*checklist.Running) (string, []string, error) {
			if err := os.RemoveAll(env.cortexDir); err != nil {
				return "", []string{dir}, err
			}
			return dir, nil, nil
		},
	}, true
}
