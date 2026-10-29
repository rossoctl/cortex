package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// `agentop configure pi` routes Pi (pi.dev) through Cortex with two pieces, because
// neither alone is enough:
//
//   - a "pi" shell function in the rc file, which runs Pi under `agentop exec`. Pi is a
//     Node program and its trust store takes the bridge CA only from
//     NODE_EXTRA_CA_CERTS, which Node reads once at process start: Pi has no CA setting,
//     and its own httpProxy setting sets the proxy alone. So the variables have to be in
//     the environment Pi starts with, the same reason the bob shell function exists.
//   - an extension in Pi's agent directory that sends Pi's session id as x-session-id.
//     Pi sends no session header by default, so without it every Pi conversation lands
//     in one bucket. The header is added by Pi's before_provider_headers hook, which
//     runs on every provider API Pi has. Verified on Pi 1.4.0.

const (
	piShellMarkerStart = "# >>> cortex agentop (pi) >>>"
	piShellMarkerEnd   = "# <<< cortex agentop (pi) <<<"
	piShellEnvVar      = "CORTEX_PI"

	// piAgentDirEnv is Pi's own override for its agent directory; piAgentDirRel is the
	// default under HOME.
	piAgentDirEnv = "PI_CODING_AGENT_DIR"
	piAgentDirRel = ".pi/agent"
	piExtName     = "cortex-session.ts"
)

// piShellBlock follows bobShellBlock's rules: fixed bytes, opening with its own
// newline, so disable is a string comparison and the round trip is byte-identical.
const piShellBlock = "\n" + piShellMarkerStart + `
pi() {
  agentop exec -- pi "$@"
}
export ` + piShellEnvVar + `=1
` + piShellMarkerEnd + "\n"

var piShellRC = rcBlock{"pi", piShellMarkerStart, piShellMarkerEnd, piShellBlock}

// piExtension is the whole extension file, fixed for the same reason as the block:
// disable removes it only when its bytes are exactly these. The header is lowercase
// because that is how Pi spells it when it sends one itself (OpenRouter's affinity
// format), so the two cannot end up as two headers.
const piExtension = `// Written by "agentop configure pi enable"; "agentop configure pi disable" removes it.
// Sends Pi's session id to Cortex, so each Pi session gets its own row in agentop.
export default function (pi: any) {
  pi.on("before_provider_headers", (event: any, ctx: any) => {
    const id = ctx.sessionManager?.getSessionId?.();
    if (id) event.headers["x-session-id"] = id;
  });
}
`

// piExtensionPath is where Pi loads user extensions from: $PI_CODING_AGENT_DIR, else
// ~/.pi/agent, then extensions/.
func piExtensionPath(home string) string {
	dir := strings.TrimSpace(os.Getenv(piAgentDirEnv))
	switch {
	case dir == "":
		dir = filepath.Join(home, piAgentDirRel)
	case dir == "~" || strings.HasPrefix(dir, "~/"):
		// Pi expands a leading ~ itself (getAgentDir's normalizePath), so a quoted one
		// still means HOME there; left as is it would name a directory called ~.
		dir = filepath.Join(home, dir[1:])
	}
	return filepath.Join(dir, "extensions", piExtName)
}

type piExtState int

const (
	piExtAbsent  piExtState = iota
	piExtOurs               // byte-equal to piExtension
	piExtForeign            // a file at our path that agentop did not write
)

func readPiExt(path string) (piExtState, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the user's own Pi directory
	switch {
	case errors.Is(err, os.ErrNotExist):
		return piExtAbsent, nil
	case err != nil:
		return 0, err
	case string(b) == piExtension:
		return piExtOurs, nil
	}
	return piExtForeign, nil
}

// piConfirm is its own var, for the reason claudeCodeConfirm gives.
var piConfirm = confirm

const piUsage = `agentop configure pi — route Pi (pi.dev) through Cortex

Usage:
  agentop configure pi enable  [--yes]
  agentop configure pi disable [--yes]
  agentop configure pi status

enable makes two changes, shown together under one confirmation:

  a "pi" shell function   in ~/.zshrc or ~/.bashrc (picked from $SHELL). It runs
                          "agentop exec -- pi", so Pi starts with the proxy and the
                          bridge CA in its environment. Pi is a Node program and reads
                          the CA only from NODE_EXTRA_CA_CERTS at startup, so a Pi
                          setting cannot do this.
  an extension            ~/.pi/agent/extensions/cortex-session.ts (under
                          $PI_CODING_AGENT_DIR when that is set). It sends Pi's session
                          id as x-session-id, so each Pi session gets its own row in
                          agentop. Pi sends no session header of its own.

Open a new terminal after enable, then run "pi" as usual. A file at the
extension's path that agentop did not write, or an edited block in the rc file,
is left alone and reported. disable removes only what enable wrote. With an
unrecognised $SHELL, the block is printed for you to place.

Exit status: 0 applied or already correct, 3 declined (or no terminal to ask
on), 1 something went wrong or was left alone, 2 a usage error.

Flags:
  --yes   do not prompt for confirmation (enable and disable)
`

// piCommand is what the three verbs share, resolved once in runPi.
type piCommand struct {
	home, shell string
	rc          string // symlink-resolved rc file; "" when advise is set
	advise      error  // why agentop will not write the rc file itself
	ext         string
	yes         bool
	stdout      io.Writer
	stderr      io.Writer
}

func runPi(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, piUsage)
		return 2
	}
	action := args[0]
	switch action {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, piUsage)
		return 0
	case "enable", "disable", "status":
	default:
		fmt.Fprintf(stderr, "agentop: unknown pi action %q (enable, disable, status)\n", action)
		return 2
	}
	fs := flag.NewFlagSet("configure pi "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {}
	var yesFlag *bool
	if action != "status" {
		yesFlag = fs.Bool("yes", false, "do not prompt for confirmation")
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, piUsage)
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "agentop: pi %s takes no arguments (got %q)\n", action, fs.Arg(0))
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		fmt.Fprintf(stderr, "agentop: cannot determine your home directory: %v\n", err)
		return 1
	}
	if action == "status" {
		s := piState(home, os.Getenv("SHELL"))
		fmt.Fprintf(stdout, "%s: %s\n", s.State, s.Detail)
		return 0
	}
	c := piCommand{home: home, shell: os.Getenv("SHELL"), ext: piExtensionPath(home),
		yes: yesFlag != nil && *yesFlag, stdout: stdout, stderr: stderr}
	var readErr error
	c.rc, c.advise, readErr = rcResolve(home, c.shell)
	if readErr != nil {
		fmt.Fprintf(stderr, "agentop: %v\n", readErr)
		return 1
	}
	return c.run(action == "disable")
}

// run is enable, or disable when remove is set: plan both pieces, show what changes
// and any finding, ask once, then apply. A piece left alone makes the exit 1 even
// when the other piece applied, because the configuration is not what was asked for.
func (c piCommand) run(remove bool) int {
	ext, err := readPiExt(c.ext)
	if err != nil {
		fmt.Fprintf(c.stderr, "agentop: read %s: %v\n", c.ext, err)
		return 1
	}
	var shell rcPlan
	if c.advise == nil {
		plan := rcEnablePlan
		if remove {
			plan = rcDisablePlan
		}
		if shell, err = plan(piShellRC, c.rc); err != nil {
			fmt.Fprintf(c.stderr, "agentop: read %s: %v\n", c.rc, err)
			return 1
		}
	}
	writeExt := ext == piExtAbsent && !remove || ext == piExtOurs && remove
	var changes, findings []string
	if writeExt && !remove {
		changes = append(changes, "  write   "+c.ext+"   (sends Pi's session id to Cortex)")
	}
	if writeExt && remove {
		changes = append(changes, "  remove  "+c.ext)
	}
	if shell.write && !remove {
		changes = append(changes, "  add     the pi function to "+c.rc+"   (runs pi through agentop exec)")
	}
	if shell.write && remove {
		changes = append(changes, "  remove  the pi function from "+c.rc)
	}
	if ext == piExtForeign {
		findings = append(findings, c.ext+" is not the file agentop writes. Left alone.")
	}
	if shell.leftAlone() {
		findings = append(findings, shell.note)
	}

	for _, f := range findings {
		fmt.Fprintf(c.stdout, "%s\n", f)
	}
	if c.advise != nil {
		fmt.Fprintf(c.stdout, "agentop: %v\n\n", c.advise)
		if remove {
			rcAdviseManual(piShellRC, "disable", "the real file", c.stdout)
		} else {
			rcAdviseManual(piShellRC, "enable", "whichever file your shell reads at startup", c.stdout)
		}
	}
	code := 0
	if len(findings) > 0 {
		code = 1
	}
	if len(changes) == 0 {
		if code == 0 && c.advise == nil {
			if remove {
				fmt.Fprintln(c.stdout, "Not enabled. Nothing to do.")
			} else {
				fmt.Fprintln(c.stdout, "Already enabled: run pi in a new terminal and it goes through Cortex.")
			}
		}
		return code
	}

	fmt.Fprintf(c.stdout, "Changes:\n%s\n\n", strings.Join(changes, "\n"))
	if !c.yes && !piConfirm(c.stdout) {
		fmt.Fprintln(c.stdout, "Not changed.")
		return exitDeclined
	}
	if writeExt {
		if err := c.applyExt(remove); err != nil {
			fmt.Fprintf(c.stderr, "agentop: %v\n", err)
			code = 1
		}
	}
	if shell.apply(c.stderr) != 0 {
		code = 1
	}
	if code != 0 {
		return code
	}
	if remove {
		fmt.Fprintln(c.stdout, "Disabled. Open a new terminal for the pi function to be gone.")
	} else {
		fmt.Fprintln(c.stdout, "Enabled. Open a new terminal, then run pi as usual; it needs Cortex running.\n"+
			"agentop configure pi disable undoes this.")
	}
	return 0
}

func (c piCommand) applyExt(remove bool) error {
	if remove {
		if err := os.Remove(c.ext); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(c.ext), 0o755); err != nil {
		return err
	}
	return writeRCFile(c.ext, piExtension)
}

// piState is Pi's row in `configure status`, and what `configure pi status` prints.
// It reads both pieces: the extension by byte comparison and the rc file for the exact
// block, so it can say which piece is missing. CORTEX_PI says whether this shell
// already has the function.
func piState(home, shell string) configureStatus {
	s := configureStatus{Agent: "pi"}
	extPath := piExtensionPath(home)
	var parts []string
	ext, err := readPiExt(extPath)
	extOn := err == nil && ext == piExtOurs
	switch {
	case err != nil:
		parts = append(parts, fmt.Sprintf("extension: cannot read %s (%v)", extPath, err))
	case ext == piExtOurs:
		parts = append(parts, "extension: "+tildePath(home, extPath))
	case ext == piExtForeign:
		parts = append(parts, "extension: "+tildePath(home, extPath)+" is not agentop's")
	default:
		parts = append(parts, "extension: missing")
	}

	shellOn, shellUnknown := false, false
	rc, advise, readErr := rcResolve(home, shell)
	switch {
	case readErr != nil:
		shellUnknown = true
		parts = append(parts, "shell: "+readErr.Error())
	case advise != nil:
		shellUnknown = true
		parts = append(parts, "shell: "+advise.Error())
	default:
		b, err := os.ReadFile(rc) //nolint:gosec // the user's own shell rc file
		n := strings.Count(string(b), piShellBlock)
		switch {
		case err != nil && !errors.Is(err, os.ErrNotExist):
			shellUnknown = true
			parts = append(parts, fmt.Sprintf("shell: cannot read %s (%v)", rc, err))
		case n == 1:
			shellOn = true
			parts = append(parts, "shell: "+tildePath(home, rc))
		case n > 1 || strings.Contains(string(b), piShellMarkerStart):
			parts = append(parts, "shell: the pi block in "+tildePath(home, rc)+" is not as enable wrote it")
		default:
			parts = append(parts, "shell: no pi function in "+tildePath(home, rc))
		}
	}
	if _, ok := os.LookupEnv(piShellEnvVar); ok {
		parts = append(parts, piShellEnvVar+" is set in this shell")
	} else if shellOn {
		parts = append(parts, piShellEnvVar+" is unset in this shell; open a new terminal")
	}

	switch {
	case extOn && shellOn:
		s.State = configureOn
	case extOn || shellOn:
		s.State = configurePartial
	case shellUnknown || err != nil:
		s.State = configureUnknown
	default:
		s.State = configureOff
	}
	s.Detail = strings.Join(parts, "; ")
	return s
}
