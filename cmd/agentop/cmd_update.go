package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
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

Downloads nothing when agentop and cortex in ~/.local/bin are already at the
release asked for. Run agentop doctor to check the rest of the install.

Exit status: 0 already current, else the installer's, which is setup's once it
hands off.
`

// installerURL is where the documented one-liner fetches install.sh.
const installerURL = "https://www.rossoctl.dev/cortex/install.sh"

// latestReleaseURL redirects to the Latest release's tag page. install.sh reads
// the same redirect first. A var so tests can serve it.
var latestReleaseURL = "https://github.com/rossoctl/cortex/releases/latest"

// updateScript fetches the installer and pipes it to sh, as the one-liner does, so
// $0 is "sh" and install.sh re-runs the released copy of itself. Fetched first, not
// piped: a failed curl would hand sh an empty script, and that exits 0. The trap is
// install.sh's: dash waits out the installer, then dies of an INT it does not catch.
const updateScript = `trap : INT TERM HUP; script=$(curl -fsSL "$0") || exit 1; printf '%s\n' "$script" | sh -s -- "$@"`

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
	if v := currentRelease(env, *ref); v != "" {
		_, _ = fmt.Fprintf(stdout, "Already at %s.\n", v)
		return 0
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
	claudeCode, noService, _ := installShape(env)
	if claudeCode {
		flags = append(flags, "--claude-code")
	}
	if noService {
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

// currentRelease is the installed version when agentop and cortex both report it
// and it is the release ref names, or the newest one when ref is "". It is ""
// otherwise, and when the newest release cannot be read: the installer then
// runs, and finds the answer or the error itself. --ref=main is never current,
// as no release reports "main" as its version.
func currentRelease(env *setupEnv, ref string) string {
	env.readVersions()
	v := env.installedVersion
	if v == "" || v != installedVersion(filepath.Join(env.binDir, "cortex")) {
		return ""
	}
	if ref == "" {
		ref = latestRelease()
	}
	if ref != v {
		return ""
	}
	return v
}

// latestRelease is the newest stable release's tag, read off where
// latestReleaseURL redirects, or "" when it does not redirect to a tag page.
func latestRelease() string {
	c := &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := c.Get(latestReleaseURL)
	if err != nil {
		return ""
	}
	_ = resp.Body.Close()
	tag, ok := strings.CutPrefix(resp.Header.Get("Location"), "https://github.com/rossoctl/cortex/releases/tag/")
	if !ok {
		return ""
	}
	return tag
}
