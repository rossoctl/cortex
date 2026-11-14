package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/rossoctl/cortex/cmd/agentop/updatestatus"
)

const updateUsage = `agentop update — install the newest Cortex release over this one

Usage:
  agentop update [--ref=REF] [--yes] [--auto]

For a release (the newest, or --ref=vX), agentop downloads the release itself,
checks each archive's SHA-256 against checksums.txt and its build provenance
against GitHub's attestation for this repository's release workflow and that
tag, and only then gives it to the new agentop setup. Any failed check stops the
update and changes nothing. --ref=main runs the same installer as the one-liner.
Update keeps these as they are now: Claude Code's routing, a proxy with no service (--no-service),
praxis mode (--praxis), an install with no config (--install-only), and a
shell profile the install did not edit (--no-modify-path). It moves an
install that runs the cpex build of cortex to the default build, and says so.

  --ref=REF   install REF instead of the newest stable release, e.g.
              --ref=v0.9.0-rc.3 for a release candidate, --ref=main
  --yes, -y   do not ask; needed when there is no terminal
  --auto      what the updater runs: install the newest release only when it is a plain release and update.wait (24h) old; never asks

Downloads nothing when agentop and cortex in ~/.local/bin are already at the
release asked for. Run agentop doctor to check the rest of the install.

Exit status: 0 already current, 1 when a download or check fails, else setup's
(or the installer's for --ref=main).
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
	auto := fs.Bool("auto", false, "")
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
	if *auto {
		if *ref != "" {
			_, _ = fmt.Fprintf(stderr, "agentop: --auto always installs the newest release; it takes no --ref\n%s", updateUsage)
			return 2
		}
		yes = true
	}
	env, err := newSetupEnv(setupOptions{})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "agentop: %v\n", err)
		return 1
	}
	unlock, held, err := lockUpdate(env.cortexDir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "agentop: %v\n", err)
		return 1
	}
	if held {
		if *auto {
			return 0
		}
		_, _ = fmt.Fprintln(stderr, "agentop: another update is running")
		return 1
	}
	defer unlock()
	if *auto {
		return runAutoUpdate(env, stdout, stderr)
	}
	latest := ""
	if *ref == "" {
		latest = latestRelease()
	}
	status := func(result string, err error) {
		s := updatestatus.Status{
			CheckedAt: updateNow().UTC(), Latest: latest, Result: result,
			Installed: installedVersion(filepath.Join(env.binDir, "agentop")),
		}
		if err != nil {
			s.Error = err.Error()
		}
		if err := writeUpdateStatus(env.cortexDir, s); err != nil {
			_, _ = fmt.Fprintf(stderr, "agentop: %v\n", err)
		}
	}
	if v := currentRelease(env, *ref, latest); v != "" {
		status(updatestatus.ResultCurrent, nil)
		_, _ = fmt.Fprintf(stdout, "Already at %s.\n", v)
		return 0
	}
	if err := installReleaseFn(env, *ref, updateFlags(env, *ref, yes), stdout, stderr); err != nil {
		status(updatestatus.ResultFailed, err)
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		_, _ = fmt.Fprintf(stderr, "agentop: %v\n", err)
		return 1
	}
	status(updatestatus.ResultUpdated, nil)
	return 0
}

// installReleaseFn is what callers use, so tests can replace the installer.
var installReleaseFn = installRelease

// provenanceCheckFn checks a release tarball's provenance; tests replace it.
var provenanceCheckFn = checkProvenance

// setupHandOffFlags are the flags of updateFlags that setup takes (D3).
var setupHandOffFlags = []string{"--claude-code", "--yes", "--no-service", "--install-only", "--no-modify-path", "--praxis"}

// releaseTag is the shape of a release tag that installRelease downloads and checks.
var releaseTag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

// cpexUpdateNotice is the one line an update prints when it replaces a cpex build.
const cpexUpdateNotice = "the laptop cpex build is removed: installed the default cortex; use praxis mode with praxis.policy for laptop policy (docs/praxis-policy.md)"

// installRelease installs ref ("" = newest release) with flags. A v* tag is
// downloaded, checked and staged here, then handed to the staged agentop setup;
// any other ref runs the website installer.
func installRelease(env *setupEnv, ref string, flags []string, stdout, stderr io.Writer) error {
	if ref == "" {
		// Fail closed: falling back to the installer would skip the check (D1).
		if ref = latestRelease(); !releaseTag.MatchString(ref) {
			return fmt.Errorf("could not read the newest release (got %q)", ref)
		}
	}
	var run func() error
	if strings.HasPrefix(ref, "v") && !releaseTag.MatchString(ref) {
		// The tag goes into a file name in the stage, a URL path and the SAN.
		return fmt.Errorf("--ref=%s is not a release tag like v1.2.3 or v1.2.3-rc.1", ref)
	}
	// Read before the install runs, because the install overwrites the binary.
	if installedIsCpex(filepath.Join(env.binDir, proxyBinary)) {
		_, _ = fmt.Fprintln(stdout, cpexUpdateNotice)
	}
	if !strings.HasPrefix(ref, "v") {
		cmd := exec.Command("sh", append([]string{"-c", updateScript, installerURL}, flags...)...)
		cmd.Stdout, cmd.Stderr = stdout, stderr
		run = cmd.Run
	} else {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		start := time.Now()
		names := releaseTarballs(ref, runtime.GOOS, runtime.GOARCH, slices.Contains(flags, "--praxis"))
		stage, size, err := stageRelease(ctx, ref, names, func(n string, d []byte) error { return provenanceCheckFn(ctx, n, d, ref) })
		stop()
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(stage) }() // setup removes it too, unless it exits early
		build := func(stage string) *exec.Cmd {
			args := []string{"setup", "--from", stage}
			for _, f := range flags {
				if slices.Contains(setupHandOffFlags, f) {
					args = append(args, f)
				}
			}
			args = append(args, fmt.Sprintf("--handoff-bytes=%d", max(size, 1)), fmt.Sprintf("--handoff-seconds=%d", int(time.Since(start).Seconds())))
			c := exec.Command(filepath.Join(stage, "agentop"), args...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, stdout, stderr
			c.Env = slices.DeleteFunc(os.Environ(), func(e string) bool { return strings.HasPrefix(e, installerDrewEnv+"=") })
			return c
		}
		run = func() (err error) {
			stage, err = runStaged(stage, env.home, build, stderr) // the deferred RemoveAll removes the stage left
			return err
		}
	}
	// Ctrl-C reaches the installer too, and setup undoes its steps on it: caught
	// here, so the prompt does not come back while that rollback still prints.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	return run()
}

// runStaged runs build(stage). A start error of EACCES is a noexec temp dir
// (install.sh's exit 126); an *exec.ExitError never matches, so setup has not
// run and a retry is safe: it copies the stage under home/.cortex/tmp, removes
// the first stage and runs once more. It returns the stage that still exists.
func runStaged(stage, home string, build func(string) *exec.Cmd, stderr io.Writer) (string, error) {
	err := build(stage).Run()
	if !errors.Is(err, fs.ErrPermission) {
		return stage, err
	}
	dir, err := restageUnderHome(stage, home)
	if err != nil {
		return stage, fmt.Errorf("cannot run programs from %s, and could not stage under ~/.cortex/tmp instead: %w", filepath.Dir(stage), err)
	}
	_, _ = fmt.Fprintf(stderr, "%s does not let programs run; staged under %s instead.\n", filepath.Dir(stage), filepath.Dir(dir))
	_ = os.RemoveAll(stage)
	return dir, build(dir).Run()
}

// restageUnderHome copies stage into a new stage under home/.cortex/tmp, a root
// isStagingDir accepts, as install.sh's restage does for a noexec temp dir.
func restageUnderHome(stage, home string) (string, error) {
	if !filepath.IsAbs(home) {
		return "", errors.New("HOME is not an absolute path")
	}
	root := filepath.Join(home, ".cortex", "tmp")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(root, "cortex-stage.")
	if err != nil {
		return "", err
	}
	if err := os.CopyFS(dir, os.DirFS(stage)); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// updateFlags are the installer flags that keep this install as it is: without
// them the installer would unroute nothing but put a service in place of a
// background proxy, direct mode in place of praxis mode, and a full setup in
// place of an install-only one, and would edit a profile the install left alone.
func updateFlags(env *setupEnv, ref string, yes bool) []string {
	var flags []string
	if !fileExists(env.configPath()) { // install-only writes no config
		flags = append(flags, "--install-only")
	}
	// A default install leaves the bin dir on PATH or its marker in the profile.
	// shortcut: a changed $SHELL reads as an opt-out, so setup prints the export line.
	if !onPath(env.pathEnv, env.binDir) && !hasPathMarker(pathProfile(env.shell, env.home)) {
		flags = append(flags, "--no-modify-path")
	}
	claudeCode, noService, _ := installShape(env)
	if claudeCode {
		flags = append(flags, "--claude-code")
	}
	if noService {
		flags = append(flags, "--no-service")
	}
	if configPraxisEnabled(env.configPath()) {
		flags = append(flags, "--praxis")
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
// and it is the release ref names, or latest when ref is "". It is ""
// otherwise, and when latest is "" (not read): the installer then
// runs, and finds the answer or the error itself. --ref=main is never current,
// as no release reports "main" as its version.
func currentRelease(env *setupEnv, ref, latest string) string {
	env.readVersions()
	v := env.installedVersion
	if v == "" || v != installedVersion(filepath.Join(env.binDir, proxyBinary)) {
		return ""
	}
	if ref == "" {
		ref = latest
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
