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
var dispatchableSubcommands = []string{"observe", "service", "claude-code", "tools"}

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
  abctl claude-code <action> point Claude Code at Cortex: enable, disable, status
  abctl tools <action>       tool-definition costs: scan

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
		case "claude-code":
			os.Exit(runClaudeCode(os.Args[2:], os.Stdout, os.Stderr))
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

// runObserve opens the traffic viewer: the Namespaces → Pods picker, or a direct
// connection when --endpoint is given or a local Cortex is answering.
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
		"AuthBridge session API URL (e.g. http://localhost:9094). When omitted, abctl connects to the Cortex on this machine if one is running, otherwise it opens a Namespaces → Pods picker.")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Best-effort sweep of edit-tempfiles older than 24h. Tempfiles are
	// intentionally left in place on every exit path (success / abort /
	// crash) so a user can recover an in-progress edit; the sweep keeps
	// $TMPDIR bounded for users who edit often.
	_ = edit.SweepStaleTempfiles()

	// With no --endpoint, prefer a Cortex running on this machine. Before this,
	// a bare `abctl` on a laptop demanded kubectl and opened a cluster picker,
	// so the local install — the whole quickstart — needed
	// `--endpoint http://localhost:47601` typed every time.
	//
	// Only when it is actually answering: a stale config from an install that is
	// no longer running must not hijack abctl away from the picker for someone
	// working against a cluster.
	local := localSessionEndpoint()
	localUp := localSessionAPIUp(local)
	if *endpoint == "" && localUp {
		*endpoint = local
	}

	// Friendly check: if picker mode and no kubectl, fail fast with a
	// clear message instead of a stack trace later.
	if *endpoint == "" {
		if _, err := exec.LookPath("kubectl"); err != nil {
			msg := "abctl: kubectl not found on PATH; install it or pass --endpoint http://..."
			// Name the more likely cause first when there is a local install that
			// simply is not running — "install kubectl" is unhelpful advice to
			// someone who has never wanted a cluster.
			if local != "" && !dialable(local) {
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
