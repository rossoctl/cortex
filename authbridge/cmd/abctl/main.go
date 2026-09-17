// Command abctl inspects and runs Cortex: a terminal UI over AuthBridge's
// in-memory session store, plus subcommands for running Cortex as a service,
// pointing Claude Code at it, and costing tool definitions.
//
// `abctl observe` opens the viewer — a Namespaces → Pods picker, then the
// session-events view for the pod it port-forwards. Pass --endpoint to skip the
// picker and connect directly.
//
// Bare `abctl` still opens the viewer for compatibility, but is deprecated: with
// subcommands reachable only by name, it left users thinking the TUI was all
// abctl did.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/rossoctl/cortex/authbridge/cmd/abctl/cluster"
	"github.com/rossoctl/cortex/authbridge/cmd/abctl/edit"
	"github.com/rossoctl/cortex/authbridge/cmd/abctl/tui"
)

// version is the abctl build version, overridden at release time via
// -ldflags "-X main.version=<tag>". Defaults to "dev" for local builds.
var version = "dev"

// dispatchableSubcommands are the names main routes on, in the order the usage
// block lists them.
//
// One list rather than two: the unknown-subcommand error used to hardcode its own
// copy, so adding a subcommand meant editing both and forgetting one left a typo
// getting an incomplete list. A test holds the usage block to this slice.
var dispatchableSubcommands = []string{"observe", "service", "configure", "claude-code", "exec", "tools", "pricing"}

// unknownSubcommandMessage is the error for an unrecognised first argument.
func unknownSubcommandMessage(name string) string {
	return fmt.Sprintf("abctl: unknown subcommand %q (known: %s)",
		name, strings.Join(dispatchableSubcommands, ", "))
}

// writeRootUsage prints the root usage block to fs's output.
//
// Takes the FlagSet so the flag list underneath comes from the same set that
// parsed the arguments, and so a test can render it into a buffer.
func writeRootUsage(fs *flag.FlagSet) {
	fmt.Fprint(fs.Output(), `abctl — inspect and run Cortex

Usage:
  abctl observe              open the traffic viewer (TUI)
  abctl service <action>     run Cortex as a service: install, uninstall,
                             status, stop, start, restart
  abctl configure <agent>    point a coding agent at Cortex: claude-code,
                             bob, codex, opencode
  abctl exec -- CMD [ARG...] run CMD with Cortex's proxy and CA in its
                             environment, for tools with no settings file
  abctl tools <action>       tool-definition costs: scan
  abctl pricing              show the model rates in effect (--host <gateway>)

  abctl claude-code <action> deprecated: same as "abctl configure claude-code".
                             Still works; prefer the new spelling.
  abctl                      deprecated: same as "abctl observe". Bare abctl
                             will stop opening the viewer in a future release.

  abctl --version            print the version and exit

Run a subcommand with no action, or with --help, for its own usage.

Viewer flags (abctl observe):
`)
	fs.PrintDefaults()
}

func main() {
	// Subcommand dispatch happens before flag.Parse: a non-flag first
	// argument selects a subcommand, and anything else falls through to the
	// terminal UI, preserving the original flags-only invocation.
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		switch os.Args[1] {
		case "tools":
			os.Exit(runTools(os.Args[2:], os.Stdout, os.Stderr))
		case "pricing":
			os.Exit(runPricing(os.Args[2:], os.Stdout, os.Stderr))
		case "configure":
			os.Exit(runConfigure(os.Args[2:], os.Stdout, os.Stderr))
		case "claude-code":
			// The old spelling, kept working and kept discoverable. Same spirit as the
			// bare-`abctl` notice below: on stderr and not fatal, because anyone with
			// this in a script or in muscle memory must not have it break under them —
			// install.sh --claude-code still runs it, and the laptop docs still print it.
			//
			// Printed HERE rather than inside runClaudeCode, which is what keeps the
			// notice in exactly one place: `abctl configure claude-code` reaches
			// the same function through runConfigure, and a notice inside it would
			// fire for the new spelling too — telling a user who already typed the right
			// thing to type something else.
			fmt.Fprintln(os.Stderr, "abctl: `abctl claude-code` is now "+
				"`abctl configure claude-code`; the old spelling still works. "+
				"See `abctl --help`.")
			os.Exit(runClaudeCode(os.Args[2:], os.Stdout, os.Stderr))
		case "exec":
			os.Exit(runExec(os.Args[2:], os.Stdout, os.Stderr))
		case "service":
			os.Exit(runService(os.Args[2:], os.Stdout, os.Stderr))
		case "observe":
			os.Exit(runObserve(os.Args[2:]))
		default:
			fmt.Fprintln(os.Stderr, unknownSubcommandMessage(os.Args[1]))
			os.Exit(2)
		}
	}

	// --version belongs to the binary, not to any subcommand, so it is answered
	// here rather than inside runObserve. Handled before the flag set exists
	// because a bare `abctl --version` must not carry the viewer's flags into its
	// output, and `abctl observe --version` must be rejected as the unknown flag it
	// now is.
	if isVersionFlag(os.Args[1:]) {
		fmt.Println("abctl", version)
		return
	}

	// Bare `abctl` still opens the viewer, but is no longer the documented way in.
	// Subcommands were reachable only by name, so a user who never typed --help saw
	// a TUI and reasonably concluded that was all abctl did — the service commands
	// least discoverable of all, and those are what you need when Cortex is down.
	//
	// Warned on stderr rather than stdout, and not fatal: this path has to keep
	// working for anyone with it in a script or in muscle memory. Suppressed when
	// the caller only wants --help or --version, where a deprecation notice would
	// be noise ahead of the very text that explains the replacement.
	if !wantsInfoFlagOnly(os.Args[1:]) {
		fmt.Fprintln(os.Stderr, "abctl: opening the traffic viewer — use `abctl observe` instead; "+
			"bare `abctl` will stop doing this in a future release. See `abctl --help`.")
	}
	os.Exit(runObserve(os.Args[1:]))
}

// isVersionFlag reports whether args are exactly a request for the version.
//
// Exactly, not merely containing one: `abctl --endpoint x --version` is a
// confused invocation, and printing a version while silently discarding the rest
// would hide that. Anything else falls through to the viewer, whose flag set
// rejects what it does not know.
func isVersionFlag(args []string) bool {
	if len(args) != 1 {
		return false
	}
	switch args[0] {
	case "-version", "--version":
		return true
	}
	return false
}

// wantsInfoFlagOnly reports whether args ask only for help or version output.
//
// Those two make the deprecation notice counterproductive: --help is where the
// replacement is documented, so printing a warning above it just pushes the
// answer further up the scrollback.
func wantsInfoFlagOnly(args []string) bool {
	if len(args) == 0 {
		return false
	}
	for _, a := range args {
		switch a {
		case "-h", "--help", "-help", "-version", "--version":
		default:
			return false
		}
	}
	return true
}

// chooseEndpoint decides which session API abctl connects to, or "" for the
// Namespaces → Pods picker.
//
// Split out of runObserve as the one part of that function testable without a
// terminal: runObserve goes on to open the TUI, so the decision itself had no test
// until it was a function of its arguments.
//
// Precedence: an explicit --endpoint always wins — it names a specific proxy, and
// second-guessing that would make the flag advisory. Otherwise a local Cortex is
// taken only when it is ANSWERING and --kubernetes is off.
//
// kubernetes defaults true, so a running local Cortex no longer claims the session
// merely by existing. Before that, someone who ran Cortex on their laptop and also
// worked against a cluster could not reach the picker at all: the probe won every
// time, and --endpoint demanded the namespace, pod and port-forward they were using
// abctl to avoid. The local one stays one keystroke away on [l], which is why
// preferring the cluster here costs nothing; the reverse is not true, since no key
// summons a picker that was never wired up.
func chooseEndpoint(explicit, local string, localUp, kubernetes bool) string {
	if explicit != "" {
		return explicit
	}
	if localUp && !kubernetes {
		return local
	}
	return ""
}

// runObserve opens the traffic viewer: the Namespaces → Pods picker, or a direct
// connection when --endpoint names one, or when a local Cortex is answering and
// --kubernetes is off. See chooseEndpoint for the precedence.
//
// This is the behaviour bare `abctl` has always had, extracted so the subcommand
// and the deprecated bare invocation cannot drift apart.
func runObserve(args []string) int {
	// Without this, `abctl --help` printed only -endpoint and -version, so the
	// subcommands were invisible to anyone who asked the tool what it could do — the
	// service commands most of all, since those are what you need when Cortex is down.
	fs := flag.NewFlagSet("abctl", flag.ExitOnError)
	fs.Usage = func() { writeRootUsage(fs) }

	endpoint := fs.String("endpoint", "",
		"AuthBridge session API URL (e.g. http://localhost:9094). When omitted, abctl opens a Namespaces → Pods picker; with --kubernetes=false it connects to the Cortex on this machine instead, when one is running.")
	// Named --prefs rather than --config: `abctl service` and `abctl claude-code`
	// already spell the PROXY's config that way, and one flag name meaning two
	// different files in one binary is worse than a second word.
	//
	// No backticks in the usage string: flag.PrintDefaults reads the first
	// backquoted word as the value's NAME, so "`abctl service`" rendered the flag as
	// "-prefs abctl service" instead of "-prefs string".
	prefs := fs.String("prefs", "",
		"abctl's own settings file — events-table columns and the active filter, saved as you change them (default ~/.cortex/abctl-config.yaml). Not the Cortex proxy config, which is --config on 'abctl service' and 'abctl configure claude-code'.")
	// --kubernetes exists because "is a local Cortex answering?" is a poor proxy for
	// "which Cortex did you mean". Someone who runs Cortex on their laptop AND works
	// against a cluster had no way to reach the picker: the local probe won, every
	// time, and --endpoint demands a namespace, a pod and a port-forward they were
	// using abctl to avoid setting up by hand.
	//
	// Default true, so the picker is offered whenever no --endpoint was given — the
	// cluster is the case abctl cannot guess and the local one is a keystroke away
	// via [l]. --kubernetes=false restores the older behaviour of preferring a
	// running local Cortex, which is what a laptop-only user wants.
	kubernetes := fs.Bool("kubernetes", true,
		"offer the Namespaces → Pods picker when no --endpoint is given, even if a Cortex is running on this machine. Use --kubernetes=false to connect straight to the local one instead. Ignored when --endpoint is given.")
	// ExitOnError, so Parse exits 2 itself (0 for -h) rather than returning — there
	// is no error branch to write here. Chosen over ContinueOnError because a bad
	// flag has nothing useful to fall back to: the alternative is printing usage and
	// then opening the viewer anyway.
	fs.Parse(args) //nolint:errcheck // ExitOnError never returns

	// Best-effort sweep of edit-tempfiles older than 24h. Tempfiles are
	// intentionally left in place on every exit path (success / abort /
	// crash) so a user can recover an in-progress edit; the sweep keeps
	// $TMPDIR bounded for users who edit often.
	_ = edit.SweepStaleTempfiles()

	// Load the user's settings — and print any complaint about a broken file — here,
	// well before tea.NewProgram takes the alt screen: after that, anything written to
	// the terminal corrupts the frame instead of reaching the user.
	//
	// A path we cannot resolve (no $HOME) means no load and no save rather than a
	// failure. The viewer's job does not depend on remembering column choices.
	prefsPath := *prefs
	if prefsPath == "" {
		var perr error
		if prefsPath, perr = userConfigPath(); perr != nil {
			fmt.Fprintf(os.Stderr, "abctl: not loading or saving settings: %v\n", perr)
			prefsPath = ""
		}
	}
	tui.Settings = loadUserConfig(prefsPath, os.Stderr)

	// Locate the Cortex on this machine, if any, and find out whether it is up.
	//
	// Probed rather than assumed from the config: a stale ~/.cortex/config.yaml left
	// by an install that is no longer running must not hijack abctl away from the
	// picker, and localUp is also what decides whether [l] gets the configured
	// address or the in-cluster 9094 default.
	//
	// Whether a live local Cortex is CHOSEN is chooseEndpoint's call, not this
	// block's: under the default --kubernetes it is offered on [l] rather than
	// connected to. This once preferred it unconditionally, which is why a laptop
	// user no longer has to type `--endpoint http://localhost:47601` — and why
	// someone who also works against a cluster needed a way back to the picker.
	local := localSessionEndpoint()
	localUp := localSessionAPIUp(local)
	*endpoint = chooseEndpoint(*endpoint, local, localUp, *kubernetes)

	// Friendly check: if picker mode and no kubectl, fail fast with a
	// clear message instead of a stack trace later.
	if *endpoint == "" {
		if _, err := exec.LookPath("kubectl"); err != nil {
			msg := "abctl: kubectl not found on PATH; install it or pass --endpoint http://..."
			// Name the more likely cause first when there is a local install that
			// simply is not running — "install kubectl" is unhelpful advice to
			// someone who has never wanted a cluster. Only under --kubernetes=false,
			// though: with --kubernetes the user asked for the cluster, and kubectl
			// really is what is missing.
			if local != "" && !dialable(local) && !*kubernetes {
				msg = "abctl: nothing is listening on " + local + " (from ~/.cortex/config.yaml).\n" +
					"  Start it:  abctl service start   (or: abctl service install)\n" +
					"  Or pass --endpoint http://... , or install kubectl to browse a cluster."
			}
			fmt.Fprintln(os.Stderr, msg)
			return 1
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		cancel()
	}()

	// LocalEndpoint only when it answered. Passing an unresponsive configured
	// address would point [l] at it and take away the in-cluster default, so a
	// working `kubectl port-forward` on 9094 could not be reached with the one key
	// that exists for exactly that.
	opts := tui.RunOptions{Endpoint: *endpoint}
	if prefsPath != "" {
		opts.Save = func(s tui.UserSettings) error { return saveUserConfig(prefsPath, s) }
	}
	if localUp {
		opts.LocalEndpoint = local
	}
	if *endpoint == "" {
		opts.Lister = cluster.NewLister()
		opts.PortForwarder = cluster.NewPortForwarder()
	}
	if err := tui.Run(ctx, opts); err != nil {
		fmt.Fprintf(os.Stderr, "abctl: %v\n", err)
		return 1
	}
	return 0
}
