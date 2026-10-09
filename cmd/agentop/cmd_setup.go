package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/rossoctl/cortex/cmd/agentop/checklist"
)

const setupUsage = `agentop setup — install or repair Cortex on this machine, after one question

Usage:
  agentop setup [--from DIR] [--claude-code] [--yes] [--no-service]
                [--install-only] [--no-modify-path] [--restart]

Plans every change first and changes nothing until you agree; a failure or a
Ctrl-C undoes what it did. Without --from it repairs what is installed.

  --from DIR         install the binaries in DIR (the installer's staging dir,
                     or ./bin for a source build)
  --claude-code      route Claude Code through Cortex
  --yes, -y          do not ask; needed when there is no terminal
  --no-service       run the proxy without launchd/systemd
  --install-only     install the binaries and PATH only
  --no-modify-path   never edit a shell profile
  --restart          restart the service even when nothing changed

Exit status: 0 done or already current, 1 failed (and rolled back) or refused
before any change, 2 usage, 3 declined, interrupted before any change, or no
terminal to ask on.
`

// A Go panic exits 2 too, the runtime's own code, so it reads as a usage error.
// Accepted: setup recovers nothing, and either way the run did not apply.

// setupStepsHook lets tests inject a failure into the real step list.
var setupStepsHook = func(s []step) []step { return s }

// setupHasTerminal reports whether there is a terminal to ask on. A var for the
// reason claudeCodeConfirm gives.
var setupHasTerminal = func() bool {
	f, err := os.Open("/dev/tty")
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// setupConfirm asks setup's one question, on /dev/tty because stdin is the
// installer script. A var for the reason claudeCodeConfirm gives.
var setupConfirm = func(w io.Writer) bool {
	tty, err := os.Open("/dev/tty")
	if err != nil {
		return false
	}
	defer func() { _ = tty.Close() }()
	return setupConfirmFrom(tty, w)
}

// setupAnimate reports whether the screen animates on w: on a terminal. A var for
// the reason uninstallAnimate gives.
var setupAnimate = isTerminal

// installerDrewEnv is how install.sh says it drew its title and its download bar on
// the terminal, which stand in for setup's header and its ✓ downloaded row. An
// environment variable, because the handoff flags are frozen.
const installerDrewEnv = "CORTEX_INSTALLER_DREW"

// setupSignals is the channel Ctrl-C and SIGTERM arrive on, and the func that
// stops them arriving. A var so tests can send one without signalling themselves.
// Only those two: a SIGHUP, from a terminal closed mid-run, still kills setup.
var setupSignals = func() (<-chan os.Signal, func()) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	return c, func() { signal.Stop(c) }
}

// setupConfirmFrom reads the answer. Enter means yes: the user typed the install
// command and the list of changes is right above the prompt. EOF is not consent.
func setupConfirmFrom(r io.Reader, w io.Writer) bool {
	_, _ = fmt.Fprint(w, "  Continue? [Y/n] ")
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true
	}
	return false
}

// parseSetupFlags parses setup's flags. A request for help, -h or --help as a
// flag or help as an argument, is flag.ErrHelp; any other error has been written
// to stderr with the usage.
func parseSetupFlags(args []string, stderr io.Writer) (setupOptions, error) {
	var o setupOptions
	fs := newFlagSet("setup", stderr)
	// Printed from Parse's result, as cmd_pipeline's is: Usage fires for -h and for
	// a bad flag alike, and help belongs on stdout.
	fs.Usage = func() {}
	fs.StringVar(&o.from, "from", "", "")
	fs.BoolVar(&o.claudeCode, "claude-code", false, "")
	fs.BoolVar(&o.yes, "yes", false, "")
	fs.BoolVar(&o.yes, "y", false, "")
	fs.BoolVar(&o.noService, "no-service", false, "")
	fs.BoolVar(&o.installOnly, "install-only", false, "")
	fs.BoolVar(&o.noModifyPath, "no-modify-path", false, "")
	fs.BoolVar(&o.restart, "restart", false, "")
	fs.Int64Var(&o.handoffBytes, "handoff-bytes", 0, "")
	fs.IntVar(&o.handoffSeconds, "handoff-seconds", 0, "")
	_ = fs.Bool("local", false, "") // install.sh passes it; local is the only mode
	if err := fs.Parse(args); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprint(stderr, setupUsage) // under Parse's own line
		}
		return o, err
	}
	if fs.Arg(0) == "help" {
		return o, flag.ErrHelp
	}
	if fs.NArg() > 0 {
		err := fmt.Errorf("unexpected argument %q", fs.Arg(0))
		_, _ = fmt.Fprintf(stderr, "agentop: %v\n%s", err, setupUsage)
		return o, err
	}
	return o, nil
}

func newSetupEnv(opts setupOptions) (*setupEnv, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil, fmt.Errorf("cannot determine your home directory (is $HOME set?)")
	}
	env := &setupEnv{
		home: home, goos: runtime.GOOS, shell: os.Getenv("SHELL"), pathEnv: os.Getenv("PATH"),
		binDir: filepath.Join(home, ".local", "bin"), cortexDir: filepath.Join(home, ".cortex"), opts: opts,
	}
	if opts.from != "" {
		abs, err := filepath.Abs(opts.from)
		if err != nil {
			return nil, err
		}
		env.fromDir = abs
	}
	return env, nil
}

func buildSetupSteps(env *setupEnv) []step {
	steps := []step{binariesStep{}, pathStep{}}
	if env.opts.installOnly {
		return append(steps, cleanupStep{afterService: false})
	}
	steps = append(steps, configStep{}, serviceStep{})
	if env.opts.claudeCode {
		steps = append(steps, claudeCodeStep{})
	}
	return append(steps, cleanupStep{afterService: true})
}

func runSetup(args []string, stdout, stderr io.Writer) int {
	// Help returns before the staging dir's defer: install.sh probes with --help.
	opts, err := parseSetupFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		_, _ = fmt.Fprint(stdout, setupUsage)
		return 0
	}
	if err != nil {
		return 2 // flags that did not parse name no stage, so it stays
	}
	// Caught from here on, rather than killing setup: install.sh exec'd it, past its
	// own EXIT trap, so the staging dir goes on every way out, a signal's included.
	// Before apply nothing has changed, so a signal is a decline; during it, a
	// rollback after the current step.
	sigs, stopSignals := setupSignals()
	defer stopSignals()
	if stage := installerStage(opts); stage != "" {
		defer func() { _ = os.RemoveAll(stage) }()
	}
	env, err := newSetupEnv(opts)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "agentop: %v\n", err)
		return 1
	}
	env.argv = append([]string{os.Args[0], "setup"}, args...)
	ui := checklist.New(stdout, setupAnimate(stdout))
	defer ui.Close()

	// install.sh's title and bar are on screen only when it says so, on a terminal: a
	// log gets setup's own lines, which say the same. The handoff flags must be there
	// too, so a value left in the environment cannot hide the header of a setup run by
	// hand.
	if ui.Animated() && opts.fromInstaller() && os.Getenv(installerDrewEnv) == "1" {
		env.readVersions() // what the header would have asked; the ending names the version
	} else {
		printSetupHeader(env, ui)
		if opts.handoffBytes > 0 {
			ui.Done("downloaded", downloadSize(opts.handoffBytes)+" · sha256 verified",
				time.Duration(opts.handoffSeconds)*time.Second)
		}
	}
	ui.Blank()

	planned, problems := planSteps(env, setupStepsHook(buildSetupSteps(env)))
	select {
	case <-sigs:
		ui.Plain("Not changed.")
		return exitDeclined
	default:
	}
	if len(problems) > 0 {
		for _, p := range problems {
			ui.Fail(p.label, p.reason)
			for _, f := range p.fix {
				ui.Remedy("fix: ", f)
			}
		}
		ui.Blank()
		if env.freshInstall {
			ui.Plain("Nothing was changed.")
		} else {
			// Said outright: a refusal is often the first thing a broken edit meets, and
			// with nothing else on screen about the proxy it read as stopped (#1282).
			ui.Plain("Nothing was changed; Cortex was not stopped or restarted.")
		}
		return 1
	}
	if allDone(planned) {
		// Nothing to change, but advice still stands: a PATH this shell lacks, say.
		// And a step's own bookkeeping still runs, as it does on applySteps' done
		// path — which this branch returns before ever reaching. A re-run of
		// `install.sh --claude-code` that re-stages the same version lands here, so
		// it is where the ownership record gets topped up on most machines.
		advised := false
		for _, p := range planned {
			if a := p.p.advice; a != nil && !p.p.hidden {
				ui.Advise(p.p.label, a.reason, a.fix...)
				advised = true
			}
			if finishAlready(env, ui, p.s) {
				advised = true
			}
		}
		if advised {
			ui.Blank()
		}
		state := "installed and running" // a background or supervised proxy alive, not asked
		switch {
		case opts.installOnly:
			state = "installed"
		case env.priorService: // the service plan's health check answered
			state = "installed and healthy"
		}
		text := fmt.Sprintf("cortex %s is %s.", env.newVersion(), state)
		if ui.Animated() && !opts.installOnly {
			ui.Logo(text) // under the blank line after the header, or after the advice
		} else {
			ui.Plain(text)
		}
		return 0
	}
	if !opts.yes {
		ui.Consent(consentItems(planned))
		ui.Blank()
		if hint := undoHint(env); hint != "" {
			ui.Faint(hint)
			ui.Blank()
		}
		if !setupHasTerminal() {
			ui.Plain("No terminal to ask on — re-run with --yes to apply.")
			return exitDeclined
		}
		// Read aside, so a Ctrl-C at the prompt declines rather than waiting on the
		// terminal. The reader is left blocked: setup exits soon after.
		answer := make(chan bool, 1)
		go func() { answer <- setupConfirm(stdout) }()
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

	// ready in is the time the changes took, from here: not the time spent reading
	// the list of them at the prompt.
	started := time.Now()
	if !applySteps(env, ui, planned, sigs) {
		return 1
	}
	for _, f := range env.onSuccess {
		f()
	}
	printSetupEnding(env, ui, time.Since(started))
	return 0
}

// downloadSize is the installer's byte count in decimal units, as the TUI's
// formatBytes shows one: a download under a megabyte in kB, so it never reads as
// "0.0 MB", and kB promoted to MB where the rounding carries, so it never reads as
// "1000.0 kB". MB is the last tier: a gigabyte prints as 1000.0 MB.
func downloadSize(n int64) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d B", n)
	case n < 999_950:
		return fmt.Sprintf("%.1f kB", float64(n)/1e3)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/1e6)
}

func printSetupHeader(env *setupEnv, ui *checklist.UI) {
	title := "rosso cortex · setup"
	if env.opts.fromInstaller() {
		title = "rosso cortex · installer"
	}
	env.readVersions()
	next := env.newVersion()
	right := next + " · " + env.goos + "/" + runtime.GOARCH
	if prev := env.installedVersion; env.fromDir != "" && prev != "" && prev != next {
		right = prev + " → " + right
	}
	if env.fromDir != "" && !env.opts.fromInstaller() {
		right += " · from " + env.tilde(env.fromDir)
	}
	ui.Header(title, right)
}

// stopBackgroundByHand stops the background proxy proxy.pid records.
func stopBackgroundByHand(env *setupEnv) string {
	return "kill $(cat " + env.tilde(filepath.Join(env.cortexDir, "proxy.pid")) + ")"
}

// undoHint names the command that undoes this run: agentop uninstall, which
// unroutes Claude Code and stops a background proxy as well as a service. An
// install-only run gets none.
func undoHint(env *setupEnv) string {
	if env.opts.installOnly {
		return ""
	}
	return "Undo any time: agentop uninstall"
}

func printSetupEnding(env *setupEnv, ui *checklist.UI, took time.Duration) {
	agentop := "agentop"
	if !env.binOnPath && !env.binOnPathNewTerms {
		agentop = env.tilde(filepath.Join(env.binDir, "agentop"))
	}
	if env.opts.installOnly {
		ui.Blank()
		if !env.binOnPath && env.binOnPathNewTerms { // the PATH edit reaches new terminals only
			ui.Plain("Installed. Open a new terminal, then start Cortex with:  " + agentop + " setup")
			return
		}
		ui.Plain("Installed. Start Cortex with:  " + agentop + " setup")
		return
	}
	// On a terminal a run that applied ends on the logo, a fresh install, an upgrade or
	// a repair alike, as one with nothing to change does in runSetup.
	ui.Blank()
	if ui.Animated() {
		ui.Logo(env.newVersion() + " ready in " + checklist.FormatDuration(took))
	} else {
		ui.Plain("cortex " + env.newVersion() + " ready.")
	}
	// A fresh install always names the command to type. An upgrade or a repair names it
	// too when this shell cannot run it yet, which is what binOnPath says: the PATH edit
	// reaches new terminals only, so a run that stopped at the logo left nothing runnable
	// on screen, and the freshest identifier-shaped word there was "rosso", the brand in
	// the title, which is no command. freshInstall alone was the wrong question — it is
	// about what was in binDir before, not about what this shell can resolve, and the two
	// part company on exactly that upgrade. Someone whose shell already has binDir on
	// PATH is told nothing new by the block, so they still skip it. rossoctl/cortex#1285.
	if !env.freshInstall && env.binOnPath {
		return
	}
	cmd, comment := agentop, "watch your agent traffic live"
	if !env.opts.claudeCode {
		cmd, comment = agentop+" exec -- <cmd>", "send an agent through Cortex"
	}
	ui.Next("Open a new terminal, then:", cmd, comment)
	ui.Blank()
	if env.startedBackground {
		ui.Faint("Cortex runs without a supervisor here; stop it with: " + stopBackgroundByHand(env))
	}
	ui.Faint(undoHint(env))
}

// systemTempRoots are the temp dirs, besides ~/.cortex/tmp, that the installer
// stages into. A var because a test's HOME is itself under one of them.
var systemTempRoots = func() []string { return []string{os.TempDir(), "/tmp"} }

// installerStage is the dir setup deletes on its way out: --from, when the
// installer invoked setup and staged into it. Worked out from the flags alone, so
// a HOME that cannot be resolved still has the system temp roots to go by.
func installerStage(opts setupOptions) string {
	if !opts.fromInstaller() || opts.from == "" {
		return ""
	}
	dir, err := filepath.Abs(opts.from)
	if err != nil {
		return ""
	}
	home, _ := os.UserHomeDir()
	if !isStagingDir(dir, home) {
		return ""
	}
	return dir
}

// isStagingDir reports whether dir is one the installer staged into — strictly
// inside $TMPDIR, /tmp or ~/.cortex/tmp — and so one setup may delete. A source
// tree's ./bin never is, nor is anything under a TMPDIR of / or $HOME, which
// would hold every such tree. Without an absolute HOME there is no ~/.cortex/tmp:
// joined to "", it would be a .cortex/tmp under the working directory.
func isStagingDir(dir, home string) bool {
	if dir == "" {
		return false
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false
	}
	roots := systemTempRoots()
	if filepath.IsAbs(home) {
		roots = append(roots, filepath.Join(home, ".cortex", "tmp"))
	}
	realHome, _ := filepath.EvalSymlinks(home)
	for _, root := range roots {
		r, err := filepath.EvalSymlinks(root)
		if err != nil || r == "/" || r == realHome {
			continue
		}
		rel, err := filepath.Rel(r, real)
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(f.Fd())
}
