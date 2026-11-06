package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
)

const updateUsage = `agentop update — install the newest Cortex release over this one

Usage:
  agentop update [--ref=REF] [--yes]

Runs the same installer as the one-liner. The installer downloads and verifies
the release, then gives it to agentop setup. Update keeps three things as they
are now: Claude Code's routing, a proxy with no service (--no-service), and the
cpex build of cortex.

  --ref=REF   install REF instead of the newest stable release, e.g.
              --ref=v0.9.0-rc.3 for a release candidate, --ref=main
  --yes, -y   do not ask; needed when there is no terminal

Exit status: the installer's, which is setup's once it hands off.
`

// installerURL is where the documented one-liner fetches install.sh.
const installerURL = "https://www.rossoctl.dev/cortex/install.sh"

// updateScript fetches the installer and pipes it to sh, as the one-liner does, so
// $0 is "sh" and install.sh re-runs the released copy of itself. Fetched first, not
// piped: a failed curl would hand sh an empty script, and that exits 0.
const updateScript = `script=$(curl -fsSL "$0") || exit 1; printf '%s\n' "$script" | sh -s -- "$@"`

func runUpdate(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("update", stderr)
	fs.Usage = func() {} // printed below, on stdout for -h, as setup's is
	ref := fs.String("ref", "", "")
	var yes bool
	fs.BoolVar(&yes, "yes", false, "")
	fs.BoolVar(&yes, "y", false, "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprint(stdout, updateUsage)
			return 0
		}
		_, _ = fmt.Fprint(stderr, updateUsage) // under Parse's own line
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "agentop: unexpected argument %q\n%s", fs.Arg(0), updateUsage)
		return 2
	}
	env, err := newSetupEnv(setupOptions{})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "agentop: %v\n", err)
		return 1
	}
	cmd := exec.Command("sh", append([]string{"-c", updateScript, installerURL}, updateFlags(env, *ref, yes)...)...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// Ctrl-C reaches the installer too, and setup undoes its steps on it: caught
	// here, so the prompt does not come back while that rollback still prints.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		_, _ = fmt.Fprintf(stderr, "agentop: %v\n", err)
		return 1
	}
	return 0
}

// updateFlags are the installer flags that keep this install as it is, decided as
// doctor decides its fix: without them the installer would unroute nothing but
// put a service in place of a background proxy and the default cortex in place
// of the cpex one.
func updateFlags(env *setupEnv, ref string, yes bool) []string {
	var flags []string
	// A project's settings file is not ~/.claude/settings.json, which is the only
	// one setup's --claude-code writes.
	elsewhere := recordedSettings(env)
	if (elsewhere == "" || samePath(elsewhere, filepath.Join(env.home, settingsRel))) && claudeCodeRouted(env) {
		flags = append(flags, "--claude-code")
	}
	if backgroundOnly(env) {
		flags = append(flags, "--no-service")
	}
	if installedIsCpex(filepath.Join(env.binDir, "cortex")) {
		flags = append(flags, "--cpex")
	}
	if ref != "" {
		flags = append(flags, "--ref="+ref)
	}
	if yes {
		flags = append(flags, "--yes")
	}
	return flags
}
