package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// IBM Bob's launch environment: how the editor's extension host comes to trust the
// bridge CA without the macOS keychain.
//
// Bob's model calls leave from its extension host's fetch, which is Node. Node trusts
// NODE_EXTRA_CA_CERTS, but VS Code's proxy layer hands that fetch an explicit CA list —
// Node's bundled roots plus the keychain — whenever http.systemCertificates is on, and
// an explicit list REPLACES Node's default store, dropping NODE_EXTRA_CA_CERTS with it.
// So two things together make Bob readable: http.systemCertificates set to false, which
// leaves fetch on Node's default store, and NODE_EXTRA_CA_CERTS in the extension host's
// environment.
//
// The second comes from the shell block below. Started from the Dock or Spotlight, Bob
// has no terminal to inherit from, so it runs the user's login shell once
// ($SHELL -i -l -c), with VSCODE_RESOLVING_ENVIRONMENT=1 set, and copies that shell's
// environment into the extension host. LaunchServices also gives the app its own
// __CFBundleIdentifier. The block tests both, so it exports the variable for Bob alone:
// not for a terminal, and not for another VS Code fork resolving its own environment.
//
// macOS only. The bundle id is a LaunchServices variable, and off macOS the existing
// route — the system trust store, which http.systemCertificates=true reads — is the one
// that works, so writing false there would break it.

const (
	// bobSysCertsKey is the second settings key enable writes on macOS, and it is
	// written as false: see the comment above for why that is what lets
	// NODE_EXTRA_CA_CERTS reach Bob's fetch.
	bobSysCertsKey = "http.systemCertificates"

	// bobBundleID is the CFBundleIdentifier in IBM Bob.app's Info.plist, which
	// LaunchServices exports as __CFBundleIdentifier to the app it launches.
	bobBundleID = "com.ibm.software.bob"

	// The markers delimit the block so disable can find it. Distinct from bobshell's,
	// and neither contains the other, so each command's Contains check sees only its
	// own block in a file that holds both.
	bobLaunchMarkerStart = "# >>> cortex agentop (bob) >>>"
	bobLaunchMarkerEnd   = "# <<< cortex agentop (bob) <<<"

	// bobStateRel records whether enable added http.systemCertificates, so disable
	// removes only a value agentop wrote. A false the user set themselves is
	// byte-identical to ours, which is the case claude-code's stateRel exists for.
	bobStateRel = ".cortex/bob-state.json"

	// bobProbeTimeout bounds the one run of the user's startup files that enable
	// makes. VS Code gives its own environment resolution 10s before giving up.
	bobProbeTimeout = 10 * time.Second
)

// bobLaunchBlock is what enable appends to the shell's rc file and what disable
// removes — one function, so the two cannot disagree.
//
// Built the way bobShellBlock is, and for the same reasons: it opens with its own
// newline so the round trip is byte-identical, and disable matches it whole rather
// than parsing shell. Unlike bobShellBlock it carries one machine-specific value, the
// CA path from the Cortex config, so disable rebuilds it from the same config; a block
// written for a different ca_dir reads as edited and is left for the user to remove.
//
// POSIX test syntax, which bash and zsh share and which is all VS Code runs it under.
func bobLaunchBlock(caPath string) string {
	return "\n" + bobLaunchMarkerStart + `
# IBM Bob reads this when it starts from the Dock or Spotlight, so it trusts Cortex.
if [ "$VSCODE_RESOLVING_ENVIRONMENT" = 1 ] && [ "$__CFBundleIdentifier" = ` + bobBundleID + ` ]; then
  export ` + envCACerts + `=` + shellQuote(caPath) + `
fi
` + bobLaunchMarkerEnd + "\n"
}

// bobLaunch is what enable, disable and status need to know about the launch
// environment, resolved once in runBob so the path that gets checked is the path that
// gets written.
//
// A struct passed in rather than read from the process, so tests point every field at
// a temporary directory: `go test` runs with the developer's real $HOME and $SHELL, and
// a test reaching for those would edit their actual ~/.bashrc.
type bobLaunch struct {
	// macOS turns the whole mechanism on. runtime.GOOS in production; a field so the
	// macOS paths are tested on the Linux CI runners too.
	macOS bool
	// home is the user's home directory, for abbreviating the paths the findings
	// name: a report line that reads ~/Library/... stays a line, where the full
	// path wraps past the edge of a terminal and takes its sentence with it.
	home string
	// shell is $SHELL, which picks the rc file and is what the probe runs.
	shell string
	// rcPath is the rc file to edit, symlink-resolved. Empty when rcErr says why
	// agentop will not edit it.
	rcPath string
	// rcErr is a reason agentop will not edit the rc file but can say what to do by
	// hand: an unrecognised shell, a dangling link, a chain past the hop limit —
	// bobshell's three advice cases.
	rcErr error
	// rcReadErr is a failure to look at all (EACCES, EIO). bobshell exits 1 on it,
	// and so does this command.
	rcReadErr error
	// statePath is the ownership record for http.systemCertificates.
	statePath string
}

// resolveBobLaunch maps $SHELL and $HOME to a bobLaunch, applying bobshell's rules for
// which file and how far to follow a link — rcResolve itself, so the two halves
// always pick the same file and refuse the same arrangements.
func resolveBobLaunch(home, shell string) bobLaunch {
	l := bobLaunch{
		macOS:     runtime.GOOS == "darwin",
		home:      home,
		shell:     shell,
		statePath: filepath.Join(home, bobStateRel),
	}
	if !l.macOS {
		return l
	}
	path, advise, readErr := rcResolve(home, shell)
	l.rcErr, l.rcReadErr = advise, readErr
	if advise == nil && readErr == nil {
		l.rcPath = path
	}
	return l
}

// bobRCBlockState is what the rc file holds of the block: how many exact copies, and
// whether the markers appear without an exact copy (edited, or written for another
// CA path).
func bobRCBlockState(content, block string) (copies int, edited bool) {
	copies = strings.Count(content, block)
	return copies, copies == 0 && strings.Contains(content, bobLaunchMarkerStart)
}

// bobSysCertsWanted reports whether enable should add http.systemCertificates=false.
//
// Absent: add it, and record that agentop did. Already false: nothing to do, and NOT
// recorded — the value is the user's, and disable must leave it. Anything else, true
// included, is a value somebody chose; overwriting it is not enable's call, for the
// same reason a foreign http.proxy is refused.
func bobSysCertsWanted(doc map[string]any) (bool, error) {
	v, present := doc[bobSysCertsKey]
	if !present {
		return true, nil
	}
	if b, ok := v.(bool); ok && !b {
		return false, nil
	}
	return false, fmt.Errorf("%q is %v. IBM Bob reads Cortex's CA only with it set to false,\n"+
		"  and agentop does not overwrite a value it did not write — change it yourself if\n"+
		"  you want Cortex to read Bob's traffic", bobSysCertsKey, v)
}

// bobOwnsSysCerts reports whether disable may remove http.systemCertificates: the
// record says enable added it to THIS settings file, and it still holds the false
// enable wrote. A changed value is the user's now and stays.
func bobOwnsSysCerts(st *managedState, settingsPath string, doc map[string]any) bool {
	if st == nil || st.Settings != settingsPath {
		return false
	}
	prior, recorded := st.Prior[bobSysCertsKey]
	if !recorded || prior != nil || st.Written[bobSysCertsKey] != "false" {
		return false
	}
	b, ok := doc[bobSysCertsKey].(bool)
	return ok && !b
}

// bobSysCertsRecord is the ownership record enable writes when it adds the key.
func bobSysCertsRecord(settingsPath string) managedState {
	return managedState{
		Settings: settingsPath,
		Prior:    map[string]*string{bobSysCertsKey: nil},
		Written:  map[string]string{bobSysCertsKey: "false"},
	}
}

// bobProbeLaunch runs the user's login shell the way IBM Bob does when it starts from
// the Dock, and returns the NODE_EXTRA_CA_CERTS that shell ends up with.
//
// It is the only way to know the block works. bash starts Bob's shell as a LOGIN
// shell, which reads ~/.bash_profile and reaches ~/.bashrc only if the profile sources
// it — the arrangement bobshell's usage already warns about. A block in a file nothing
// reads does not fail; Bob just goes back to being passed through unread. So enable
// asks the shell rather than reasoning about its startup order.
//
// Reproduced from Bob's own code: the -i -l -c flags, VSCODE_RESOLVING_ENVIRONMENT=1,
// a marker around the value so whatever the rc files print is ignored, and the
// variable removed from the inherited environment first, so a value agentop itself
// was started with cannot pass for one the shell set. Setsid detaches it from the
// terminal, as a Dock-launched app's shell is: an interactive shell on agentop's
// terminal would take it over. stdin is /dev/null for the same reason.
//
// A var so tests substitute it rather than run the developer's real startup files.
var bobProbeLaunch = func(shell string) (string, error) {
	const mark = "__agentop_bob_probe__"
	ctx, cancel := context.WithTimeout(context.Background(), bobProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, "-i", "-l", "-c",
		`printf '%s%s%s' `+mark+` "$`+envCACerts+`" `+mark)
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case envCACerts, "__CFBundleIdentifier", "VSCODE_RESOLVING_ENVIRONMENT":
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env, "VSCODE_RESOLVING_ENVIRONMENT=1", "__CFBundleIdentifier="+bobBundleID)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", fmt.Errorf("%s did not finish within %s", shell, bobProbeTimeout)
	}
	start := bytes.Index(out, []byte(mark))
	if start < 0 {
		if err != nil {
			return "", fmt.Errorf("%s -i -l -c failed: %w", shell, err)
		}
		return "", fmt.Errorf("%s -i -l -c printed nothing agentop could read", shell)
	}
	rest := out[start+len(mark):]
	end := bytes.Index(rest, []byte(mark))
	if end < 0 {
		return "", fmt.Errorf("%s -i -l -c printed nothing agentop could read", shell)
	}
	return string(rest[:end]), nil
}

// reportProbe runs the probe and says what it found. Advisory: the settings and
// the block are written either way, and a failed check is a reason to look, not
// to undo.
//
// verbose is the already-enabled arm, where the probe's answer IS the point of
// the re-run — "a re-run of enable is how someone asks whether it is working".
// Otherwise only a finding is printed: a fresh enable that verified cleanly says
// nothing, because "Enabled." followed by silence reads as verified.
func (c bobCommand) reportProbe(caPath string, verbose bool) {
	l := c.launch
	if l.shell == "" {
		if verbose {
			fmt.Fprint(c.stdout, "Not checked: $SHELL is not set, so agentop cannot start your shell the way\n"+
				"  IBM Bob does.\n\n")
		}
		return
	}
	got, err := bobProbeLaunch(l.shell)
	switch {
	case err != nil:
		// Always said, even quiet: a check that could not run is a finding, not
		// a success — "Enabled." over silence would read as verified.
		fmt.Fprintf(c.stdout, "Not checked: %v.\n\n", err)
	case got == caPath:
		if verbose {
			fmt.Fprintf(c.stdout, "Checked: your login shell, started the way IBM Bob starts it, sets\n"+
				"  %s=%s.\n\n", envCACerts, caPath)
		}
	case got == "":
		fmt.Fprintf(c.stdout, "WARNING: your login shell, started the way IBM Bob starts it, does not set\n"+
			"  %s, so Bob will not trust Cortex's CA.\n", envCACerts)
		if filepath.Base(l.rcPath) == ".bashrc" {
			fmt.Fprint(c.stdout, "  bash starts it as a LOGIN shell, which reads ~/.bash_profile and not\n"+
				"  ~/.bashrc. Source ~/.bashrc from ~/.bash_profile — the usual arrangement — or\n"+
				"  move the block there.\n\n")
		} else {
			fmt.Fprint(c.stdout, "  Make sure the block is in a file your login shell reads.\n\n")
		}
	default:
		// Something later in the startup files sets the variable again. Naming the
		// value is the useful part: it is the user's own export, and they can find it.
		fmt.Fprintf(c.stdout, "WARNING: your login shell, started the way IBM Bob starts it, sets\n"+
			"  %s=%s, not Cortex's %s — something after the block sets it again.\n\n",
			envCACerts, got, caPath)
	}
}

// bobLaunchCheck is status's read of the launch half, from files alone: whether
// http.systemCertificates is false and whether the block is in the rc file. It never
// runs the probe — status writes nothing and runs nothing, and only enable starts the
// user's shell — so "the block is there" is as far as it can say.
//
// ok is both halves in place. problems is every finding, for the status report's
// indented lines; the status table's row takes only the first, as its detail.
func bobLaunchCheck(l bobLaunch, caPath string, doc map[string]any) (ok bool, problems []string) {
	fail := func(line string) { problems = append(problems, line) }
	sysOK := false
	switch v, present := doc[bobSysCertsKey]; {
	case !present:
		fail(fmt.Sprintf("%q is unset, so Bob's extensions do not read Cortex's CA", bobSysCertsKey))
	case v == false:
		sysOK = true
	default:
		fail(fmt.Sprintf("%q=%v; Bob's extensions read Cortex's CA only with it false", bobSysCertsKey, v))
	}

	blockOK := false
	switch {
	case l.rcReadErr != nil:
		fail(fmt.Sprintf("cannot tell whether the cortex bob block is in place: %v", l.rcReadErr))
	case l.rcErr != nil:
		fail(fmt.Sprintf("%v, so agentop has not looked for the cortex bob block", l.rcErr))
	default:
		rc := tildePath(l.home, l.rcPath)
		b, err := os.ReadFile(l.rcPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			fail(fmt.Sprintf("cannot read %s: %v", rc, err))
			break
		}
		switch copies, edited := bobRCBlockState(string(b), bobLaunchBlock(caPath)); {
		case copies > 0:
			blockOK = true
		case edited:
			fail(fmt.Sprintf("%s has a cortex bob block that is not the one agentop writes", rc))
		default:
			fail(fmt.Sprintf("the cortex bob block is not in %s", rc))
		}
	}
	return sysOK && blockOK, problems
}
