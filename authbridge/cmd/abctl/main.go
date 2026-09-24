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
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/rossoctl/cortex/authbridge/authlib/observe/claude"
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
var dispatchableSubcommands = []string{"observe", "service", "configure", "claude-code", "exec", "tools", "pipeline", "pricing", "cost", "experimental"}

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
  abctl pipeline <action>    the plugin pipeline in effect: get
  abctl pricing              show the model rates in effect (--host <gateway>)
  abctl cost                 what your agents have spent (--window today|month|7d|1h)
  abctl experimental <action>
                             unstable helpers: read-claude-sessions

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
		case "pipeline":
			os.Exit(runPipeline(os.Args[2:], os.Stdout, os.Stderr))
		case "experimental":
			os.Exit(runExperimental(os.Args[2:], os.Stdout, os.Stderr))
		case "pricing":
			os.Exit(runPricing(os.Args[2:], os.Stdout, os.Stderr))
		case "cost":
			os.Exit(runCost(os.Args[2:], os.Stdout, os.Stderr))
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

// observeHarvester is the harvester runObserve hands the viewer, or nil for none.
//
// One line of decision, in a function, so a test asserts the SAME code production runs rather
// than a copy of its condition. Inlined in runObserve it was untestable without a terminal, and
// a test that restated the `if` passed with the real wiring deleted — confirmed by mutation,
// which is why this exists at all.
func observeHarvester(f observeFlags, warn io.Writer) tui.HarvestFunc {
	// Nil under --skip-claude-metadata, which is what turns the harvest off: the viewer then
	// shows whatever titles the metadata file already held, from the last run.
	if *f.skipClaudeMetadata {
		return nil
	}
	return claudeHarvester(warn)
}

// claudeHarvester returns the background harvest `abctl observe` runs, or nil when the user
// asked for none.
//
// Returns a closure rather than harvesting here, because WHEN it runs is the point. It used to
// run before tui.Run and block on it: a full read of a large ~/.claude is about 0.7s of an
// empty terminal before the first frame, which is most of what startup felt like. Handed to the
// TUI instead, it runs on bubbletea's own goroutine while the picker paints, and the titles
// arrive whenever the scan finishes — usually before anyone has picked a pod.
//
// What makes that safe is that the harvest is not on the critical path for anything the first
// frame shows. The viewer reads ~/.cortex/session-metadata.json while building its model, so it
// opens with every title the LAST run wrote; this pass only adds names for sessions that are
// new or renamed since. Nothing regresses if it lands late, and nothing breaks if it never
// lands.
//
// Warnings go to warn, which is stderr before the alt screen goes up — the harvest itself
// finishes after that, so a failure it discovers cannot be printed. Rather than pretend
// otherwise, the two failures that are knowable UP FRONT are checked here, before returning:
// an unresolvable home directory, and a metadata file that is already corrupt. Those are the
// ones a user can act on, and the corrupt one is the one that repeats every launch.
//
// THE COST OF NOT BLOCKING: a tea.Cmd is a goroutine and nothing waits for it, so a viewer
// quit before the scan finishes abandons it and writes no file — the next launch then scans
// again. Measured, that window is the ~0.7s of a full first scan and ~2ms once the file exists.
// Accepted rather than closed: someone who quits inside it never saw a title either way, and
// the alternatives are worse — blocking startup is the thing being fixed, and waiting on the
// goroutine at teardown would put that delay on EXIT, where it is more surprising, on a
// keystroke the user expects to be instant.
func claudeHarvester(warn io.Writer) tui.HarvestFunc {
	// Checked here so the repair can actually be printed. Harvest would hit the same error on
	// the background goroutine, where there is nowhere to say so.
	if path, err := claude.SessionMetadataPath(); err != nil {
		fmt.Fprintf(warn, "abctl: not naming sessions from Claude Code: %v\n", err)
		return nil
	} else if _, err := claude.ReadMetadata(path); err != nil {
		// A FILE THAT DOES NOT PARSE IS NO LONGER CHECKED FOR HERE, because Harvest rebuilds it
		// on the background goroutine and the titles arrive on their own. What is left is the
		// file Harvest still refuses: one that could not be READ. That one repeats every launch
		// and needs a human — a permission fixed, a disk looked at — so it is worth the one
		// thing this position can still do, which is print before the alt screen goes up.
		//
		// Read-and-discard rather than a stat: the failure being checked for is the read itself,
		// and Harvest's own read is the one that decides. Not a wasted read either way — it was
		// already here, and what changed is only which of its errors is fatal.
		if errors.Is(err, fs.ErrPermission) {
			fmt.Fprintf(warn, "abctl: not naming sessions from Claude Code: %v\n"+
				"  Fix the file's permissions, or move it aside:\n"+
				"    mv %s %s.bad\n", err, path, path)
			return nil
		}
		// Too large is the same shape of problem: Harvest refuses it, so it repeats every launch
		// and needs a human. Different remedy — the file is intact and readable, just over the
		// cap — so it gets its own line rather than the permission advice.
		if errors.Is(err, claude.ErrMetadataTooLarge) {
			fmt.Fprintf(warn, "abctl: not naming sessions from Claude Code: %v\n"+
				"  Move it aside to start a fresh file:\n"+
				"    mv %s %s.bak\n", err, path, path)
			return nil
		}
		// EVERY OTHER READ ERROR FALLS THROUGH SILENTLY rather than disabling titles or warning
		// here, because this position cannot tell which of them Harvest will recover from. The
		// sentinel that says "this one does not parse" is unexported, and exporting it would
		// widen authlib's API to let this pre-flight re-derive a decision Harvest makes a few
		// lines later anyway.
		//
		// So the split is by what a human can DO, not by what went wrong: the two cases above
		// name an action and repeat every launch, which is worth printing before the alt screen
		// goes up. Not warning about the rest is the deliberate half — the common case among them
		// is the file that does not parse, which heals itself moments later, so a line here would
		// tell the operator titles are in trouble and then hand them titles. A harvest that does
		// go on to fail reports itself through the viewer's own error path.
	}
	return func() (map[string]tui.SessionMetadata, error) {
		// Incremental, unlike `abctl experimental read-claude-sessions`: that command's subject
		// IS the harvest, so it re-reads everything. This one runs on every launch, and a full
		// scan measured 0.74s against 0.002s for an incremental pass over the same tree.
		res, err := claude.Harvest(claude.Options{Merge: true, Incremental: true})
		if err != nil {
			return nil, err
		}
		// res.Partial IS DISCARDED HERE, and that is a real gap rather than an oversight: a
		// transcript whose read ended early still yields whatever title was found before the stop,
		// so the affected session shows a confidently-wrong — possibly stale — name with nothing
		// marking it. `abctl experimental read-claude-sessions` prints a bounded summary of exactly
		// this; the background path cannot.
		//
		// Why not: by the time this returns, tea.NewProgram owns the screen, and there is no log
		// sink in abctl to divert to — anything written to the terminal corrupts the frame. Marking
		// the rows would be the right answer instead of reporting, but Partial carries formatted
		// "path: err" strings rather than session ids, so the ids are not recoverable here without
		// widening the type and threading a per-entry flag into SessionMetadata and the TITLE cell.
		// That is a bigger change than this pass is for; the honest interim is that the explicit
		// subcommand is where a truncated transcript is visible, and it re-reads everything.
		//
		// The MERGED map, not just what this pass parsed: an incremental pass parses only the
		// transcripts that changed, so its own harvest is nearly empty and would name almost
		// nothing. Result.Meta is the whole thing Harvest wrote, which is also why this no
		// longer re-reads the file — that was a third read of one file in a single launch.
		return res.Meta, nil
	}
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
// taken only when it is ANSWERING and --kubernetes was not passed.
//
// kubernetes defaults false: a live local Cortex is taken, which keeps a bare
// `abctl observe` on a laptop working with no flag at all. Passing --kubernetes is
// how someone who runs one locally AND works against a cluster reaches the picker,
// which the probe would otherwise win every time — and which --endpoint could only
// substitute for by naming a namespace, a pod and a port-forward by hand.
//
// The picker is still reachable without the flag whenever no local Cortex answers,
// so this default costs a cluster user nothing on a machine that has no local
// install; it costs them one flag on a machine that does.
func chooseEndpoint(explicit, local string, localUp, kubernetes bool) string {
	if explicit != "" {
		return explicit
	}
	if localUp && !kubernetes {
		return local
	}
	return ""
}

// observeFlags are the viewer's flags, returned as one struct by
// registerObserveFlags.
type observeFlags struct {
	endpoint   *string
	prefs      *string
	kubernetes *bool
	// skipClaudeMetadata turns off the implicit harvest. See registerObserveFlags.
	skipClaudeMetadata *bool
}

// registerObserveFlags declares the viewer's flags on fs and returns the pointers.
//
// A function rather than inline registration in runObserve so a test can inspect
// what production actually registers — see TestKubernetesFlag_DefaultsToFalse,
// which reads --kubernetes's default from whatever this registers.
//
// fs is the caller's, so its error handling is too: runObserve uses ExitOnError
// (a bad flag has nothing useful to fall back to), while a test uses
// ContinueOnError so a parse failure is a failed assertion rather than a killed
// test binary.
func registerObserveFlags(fs *flag.FlagSet) observeFlags {
	return observeFlags{
		endpoint: fs.String("endpoint", "",
			"AuthBridge session API URL (e.g. http://localhost:9094). When omitted, abctl connects to the Cortex on this machine if one is running, otherwise it opens a Namespaces → Pods picker; --kubernetes forces the picker either way."),
		// Named --prefs rather than --config: `abctl service` and `abctl claude-code`
		// already spell the PROXY's config that way, and one flag name meaning two
		// different files in one binary is worse than a second word.
		//
		// No backticks in the usage string: flag.PrintDefaults reads the first
		// backquoted word as the value's NAME, so "`abctl service`" rendered the flag as
		// "-prefs abctl service" instead of "-prefs string".
		prefs: fs.String("prefs", "",
			"abctl's own settings file — events-table columns and the active filter, saved as you change them (default ~/.cortex/abctl-config.yaml). Not the Cortex proxy config, which is --config on 'abctl service' and 'abctl configure claude-code'."),
		// --kubernetes exists because "is a local Cortex answering?" is a poor proxy for
		// "which Cortex did you mean". Someone who runs Cortex on their laptop AND works
		// against a cluster otherwise has no way to reach the picker: the local probe
		// wins, every time, and --endpoint demands a namespace, a pod and a port-forward
		// they were using abctl to avoid setting up by hand. --kubernetes is that way.
		//
		// Default FALSE, so the common case is unchanged: a laptop Cortex that is up is
		// what a bare `abctl observe` connects to, which is the whole quickstart and
		// wants no flag. Reaching a cluster is the deliberate act, so it is the one that
		// gets spelled out — and the cluster stays reachable without the flag too, since
		// a local Cortex that is down still falls through to the picker.
		kubernetes: fs.Bool("kubernetes", false,
			"open the Namespaces → Pods picker even when a Cortex is running on this machine. Without it, a running local Cortex is connected to directly and the picker appears only if none is answering. Ignored when --endpoint is given."),
		// Default FALSE, so a bare `abctl observe` names its sessions with no flag at
		// all: a viewer showing bare UUIDs is the problem the metadata file exists to
		// solve, and nobody will run a separate subcommand first to get titles.
		//
		// The harvest is also INCREMENTAL — it parses the transcripts that changed since
		// the last run, measured at 2 of 124 files and 5.1 of 207 MB on a real tree —
		// but that is no longer why the default is affordable: since the scan moved to a
		// background tea.Cmd it costs no startup time either way. What is left for this
		// flag to decline is narrower, and it is the reason to keep it: a machine where
		// ~/.claude should simply not be touched.
		skipClaudeMetadata: fs.Bool("skip-claude-metadata", false,
			"do not harvest session titles from Claude Code's transcripts. By default abctl observe scans CLAUDE_CONFIG_DIR / ~/.claude in the background once the viewer is up and records titles in ~/.cortex/session-metadata.json, so sessions show a name instead of a bare UUID. This skips the scan; titles already recorded by earlier runs are still shown, so only sessions new or renamed since the last scan appear as bare ids."),
	}
}

// runObserve opens the traffic viewer: a direct connection when --endpoint names
// one, or when a local Cortex is answering and --kubernetes was not passed;
// otherwise the Namespaces → Pods picker. See chooseEndpoint for the precedence.
//
// This is the behaviour bare `abctl` has always had, extracted so the subcommand
// and the deprecated bare invocation cannot drift apart.
func runObserve(args []string) int {
	// Without this, `abctl --help` printed only -endpoint and -version, so the
	// subcommands were invisible to anyone who asked the tool what it could do — the
	// service commands most of all, since those are what you need when Cortex is down.
	fs := flag.NewFlagSet("abctl", flag.ExitOnError)
	fs.Usage = func() { writeRootUsage(fs) }
	f := registerObserveFlags(fs)
	endpoint, prefs, kubernetes := f.endpoint, f.prefs, f.kubernetes
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
	// block's: by default it is, which is why a laptop user does not have to type
	// `--endpoint http://localhost:47601`. Under --kubernetes it is offered on [l]
	// instead, which is how someone who also works against a cluster gets past a
	// probe that would otherwise win every time.
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
			// someone who has never wanted a cluster. Not under --kubernetes,
			// though: there the user asked for the cluster, and kubectl really is
			// what is missing.
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
	opts.Harvest = observeHarvester(f, os.Stderr)
	if prefsPath != "" {
		opts.Save = func(s tui.UserSettings) error { return saveUserConfig(prefsPath, s) }
	}
	if localUp {
		opts.LocalEndpoint = local
		// Only when it answered, same reasoning as LocalEndpoint above: these
		// are what let `e` edit the local pipeline, and offering that against a
		// proxy that is not running would apply an edit nothing reloads.
		opts.LocalConfigPath, opts.LocalStatsURL = localEditTargets()
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
