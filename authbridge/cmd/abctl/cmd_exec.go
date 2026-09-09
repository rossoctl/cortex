package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"sort"
	"strings"
	"syscall"
)

// exec runs one command with Cortex's proxy and CA already in its environment,
// for the many tools that read the environment and nothing else.
//
// `claude-code enable` exists because Claude Code has a settings file, so the
// variables can be written once and reach every session including background
// agents. Nothing else has that: curl, python, node, gh, a test suite all read
// the process environment, and the alternative is a shell export that leaks into
// every unrelated command in that terminal until it is unset. `abctl exec`
// scopes the routing to a single child.
//
// The values come from the same wanted() the enable path uses, so the two cannot
// drift: exec and enable point at the same proxy on the same port with the same
// CA, or they both fail for the same reason.

// execProxyVars are the names that get the forward-proxy URL. Four spellings
// because there is no agreed one: Go's http.ProxyFromEnvironment and most Unix
// tools read the lowercase pair, Node and the JVM-adjacent world the uppercase,
// and libcurl accepts either. A tool that reads only the spelling we omitted is
// a tool that silently bypasses the proxy — invisible, because it keeps working.
//
// HTTP_PROXY and HTTPS_PROXY both get the same value, and it is deliberately the
// http:// URL in both: the scheme in a *_PROXY variable names how to reach the
// PROXY, not what the proxied request is. Cortex's forward proxy speaks plain
// HTTP and CONNECT-tunnels TLS, so https:// here would make clients try TLS to
// the proxy itself and fail the handshake.
var execProxyVars = []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"}

// The CA variable names are NOT redeclared here. authlib owns that split:
// envCACerts is the one additive name (Node) and bundleKeys are the four that
// replace the trust store, both defined next to the trust bundle they describe.
// A local copy went stale the moment GIT_SSL_CAINFO joined the managed set —
// --print silently stopped emitting it — so exec reads the managed set instead.

const execUsage = `abctl exec — run a command with Cortex's proxy and CA in its environment

Usage:
  abctl exec [--cortex-stats-url URL] -- COMMAND [ARG...]
  abctl exec [--cortex-stats-url URL] --print

Everything after -- is passed to COMMAND exactly as given; abctl does not
interpret it, so the command's own flags need no escaping:

  abctl exec -- curl -sv https://api.anthropic.com/v1/messages
  abctl exec -- claude --dangerously-skip-permissions
  abctl exec -- bob

The child inherits your environment plus these, read from the RUNNING proxy's
/config so they always match the process that will serve the request:

  HTTP_PROXY  HTTPS_PROXY  http_proxy  https_proxy      the forward proxy URL
  NODE_EXTRA_CA_CERTS                                   ca.crt, ADDED to the
                                                        runtime's own roots
  CURL_CA_BUNDLE  REQUESTS_CA_BUNDLE                    bundle.crt: the bridge CA
  SSL_CERT_FILE   GIT_SSL_CAINFO                        plus the platform roots

Those last four REPLACE the trust store rather than extend it, so they get the
bundle: naming the bridge CA alone would leave the child trusting one CA and
break every host the bridge does not terminate (passthrough_hosts, skip_hosts,
ports outside tls_bridge.ports). Cortex writes both files itself, beside each
other in tls_bridge.ca_dir. Requires tls_bridge.mode: enabled.

Values already in your environment are replaced for this child only; nothing is
exported to your shell and no file is modified. Signals go to the child, and
abctl exits with the child's exit status, so it is safe in a pipeline or a
Makefile.

Flags:
  --cortex-stats-url URL
                 stats URL of the running Cortex (default http://localhost:47602/).
                 The values come from the RUNNING proxy's /config, not from a file,
                 so they describe the process that will actually serve the request —
                 and a proxy that is down is reported rather than yielding an
                 environment that points at nothing.
  --print        print the variables that would be set and exit, without running
                 anything. Shell-quoted, so: eval "$(abctl exec --print)"
                 Mutually exclusive with a command: --print emits settings for a
                 shell to keep (paths under the CA directory, which outlive this
                 process), whereas a command gets them for its own lifetime only.

Exit status: the child's, or 1 if the command could not be started (127 if it
was not found on PATH), or 2 for a usage error.
`

// execEnvNotFound is the exit code for "command not found", following the shell
// convention. A caller distinguishing "my command is missing" from "my command
// failed" gets the same answer from `abctl exec` as from `sh -c`.
const execEnvNotFound = 127

// runExec handles the `exec` subcommand. Returns the process exit code.
func runExec(args []string, stdout, stderr io.Writer) int {
	// Split on the first -- ourselves, before the flag package sees anything.
	//
	// flag.Parse stops at -- and would leave the rest in fs.Args(), which sounds
	// like enough, but it also stops at the first non-flag argument and at any flag
	// it does not recognise. So `abctl exec curl -x` (no --) would be accepted with
	// "curl" as a positional, and `abctl exec -- claude --config x` risks the child's
	// own --config being read as ours if the delimiter were ever dropped. Requiring
	// the delimiter and cutting on it first makes the boundary syntactic: nothing
	// after it is ever parsed, so no child flag can collide with an abctl flag,
	// now or when a future flag is added.
	before, cmdArgs, found := cutArgs(args, "--")

	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, execUsage) }
	printOnly := fs.Bool("print", false, "print the variables instead of running a command")
	statsURL := fs.String("cortex-stats-url", defaultCortexStatsURL, "stats URL of the running Cortex")
	if err := fs.Parse(before); err != nil {
		return 2
	}

	// The delimiter is required to RUN something, not to print.
	//
	// `abctl exec --print` is a complete request on its own: it asks for the
	// environment, and there is no command for a delimiter to separate. Demanding
	// `--print --` answered a well-formed request with usage text.
	if !found && !*printOnly {
		fmt.Fprint(stderr, execUsage)
		return 2
	}
	// `abctl exec --print --` is the opposite mistake: the delimiter promises a
	// command and none follows. Refused rather than quietly read as plain --print —
	// it is the empty case of the mutual exclusion below, and the same reasoning
	// applies, so it gets the same answer.
	// `abctl exec --` with nothing after it is a usage error knowable from argv, so
	// it is answered here rather than after the config fetch below. Diagnosed later,
	// a user with Cortex down was told "no Cortex is running" and got exit 1 for what
	// execUsage documents as exit 2 — and paid a pointless round-trip to learn it.
	if found && !*printOnly && len(cmdArgs) == 0 {
		fmt.Fprintln(stderr, "abctl: nothing to run after --")
		fmt.Fprint(stderr, execUsage)
		return 2
	}
	if found && *printOnly && len(cmdArgs) == 0 {
		fmt.Fprintln(stderr, "abctl: --print takes no command, so `--` has nothing to separate.")
		fmt.Fprintln(stderr, "  Use `abctl exec --print` on its own.")
		return 2
	}
	// Stray positional arguments before the delimiter are a mistake, not something
	// to ignore: `abctl exec curl -- -sv` most likely means the user typed the
	// command in the wrong place, and silently running `-sv` is a worse answer than
	// saying so.
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "abctl: unexpected argument %q before --; the command goes after --\n", fs.Arg(0))
		return 2
	}

	// A command or --print, never both.
	//
	// Not merely "the command would be ignored": the two modes disagree about how
	// long their output is meant to last. --print emits paths for a shell to keep —
	// the same ca.crt and bundle.crt that `claude-code enable` writes into
	// settings.json — and those outlive any single invocation, because Cortex writes
	// them, not abctl. Running a command is the opposite: one process, one lifetime.
	// Asking for both in one breath is a contradiction about intent, not a spare
	// argument, so it is refused the way an argument in the wrong place is rather
	// than warned about and half-honoured.
	if *printOnly && len(cmdArgs) > 0 {
		fmt.Fprintf(stderr, "abctl: --print and a command are mutually exclusive.\n"+
			"  --print emits environment settings to keep (paths under the CA directory,\n"+
			"  which outlive this process); running a command applies them to that one\n"+
			"  child. Pick one:\n"+
			"    abctl exec --print               # print the settings\n"+
			"    abctl exec -- %s\n",
			strings.Join(cmdArgs, " "))
		return 2
	}

	inject, missingBundle, err := execEnv(*statsURL)
	if err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}
	// execEnv already dropped the replacing variables; say what that costs.
	if missingBundle != "" {
		fmt.Fprintf(stderr, "abctl: note: %s does not exist yet — Cortex writes it on first start.\n"+
			"  Until then the child keeps its own trusted roots and only hosts the bridge\n"+
			"  terminates will fail verification (abctl service start).\n", missingBundle)
	}

	if *printOnly {
		printExecEnv(inject, stdout)
		return 0
	}

	return runChild(cmdArgs, inject, stdout, stderr)
}

// runChild runs argv with inject layered over the current environment and
// returns its exit status.
func runChild(argv []string, inject map[string]string, stdout, stderr io.Writer) int {
	// LookPath first, so "not found" is reported as itself with the 127 a shell
	// would give, rather than arriving as a generic start failure.
	path, err := exec.LookPath(argv[0])
	if err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		if errors.Is(err, exec.ErrNotFound) {
			return execEnvNotFound
		}
		return 1
	}

	cmd := exec.Command(path, argv[1:]...) //nolint:gosec // the command is the user's, given after --
	// argv[0] as the user typed it, not the absolute path LookPath resolved.
	// exec.Command would pass "/usr/bin/foo", but a shell passes "foo": multi-call
	// binaries dispatch on argv[0], and tools echo it in their own usage and errors.
	// cmd.Path keeps the resolved path, so the 127-on-not-found behaviour is intact.
	cmd.Args[0] = argv[0]
	cmd.Env = mergeEnv(os.Environ(), inject)
	// The child gets this process's real stdio, not a pipe: it may be interactive
	// (`abctl exec -- claude`), and a pipe would cost it the terminal, so it would
	// disable colour, refuse to prompt, or line-buffer its output. stdout/stderr
	// are still honoured when a caller passed something else, which is what makes
	// this testable.
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	// Signal handling splits by how the signal arrives, and abctl has to survive
	// both kinds — it is a wrapper, so dying first strands the child on the terminal.
	//
	// tty-generated signals (SIGINT and SIGQUIT from a keystroke) go to the whole
	// foreground process group, so the child already has them: there is no Setpgid
	// here, deliberately, because an interactive child is supposed to see Ctrl-C.
	// Relaying them would double the signal. But abctl must still CATCH them, or
	// Go's default disposition kills the wrapper while the child keeps running —
	// for `abctl exec -- claude`, where Ctrl-C interrupts a turn rather than
	// quitting, that leaves the shell prompt and claude both reading one stdin, with
	// the terminal in whatever mode claude left it. Verified: with SIGINT unhandled,
	// abctl exits -2 and the child survives. SIGQUIT is the same shape and
	// additionally dumps Go's goroutine stacks over the user's screen.
	//
	// So they are caught and dropped: notified, never forwarded. The runtime absorbs
	// them, Wait keeps running, and the child's own death arrives as 128+signum —
	// what a shell would report anyway.
	//
	// NOT signal.Ignore: that sets SIG_IGN at the OS level, and exec() preserves
	// ignored dispositions across the exec (caught ones reset to default), so the
	// child would inherit the ignore and Ctrl-C would stop reaching it at all.
	//
	// A signal aimed at abctl's PID alone is the other kind: `timeout 30 abctl exec
	// -- …`, a CI runner, or systemd sends SIGTERM to the wrapper only, and nothing
	// reaches the child unless abctl passes it on. Those two ARE relayed.
	//
	// Notify BEFORE Start: a signal landing between them would hit the default
	// disposition and kill abctl with the child already running. The channel is
	// buffered, so one arriving in that window is held for the relay rather than
	// dropped.
	relayed := []os.Signal{syscall.SIGTERM, syscall.SIGHUP}
	absorbed := []os.Signal{syscall.SIGINT, syscall.SIGQUIT}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, append(append([]os.Signal{}, relayed...), absorbed...)...)

	if err := cmd.Start(); err != nil {
		signal.Stop(sigs)
		fmt.Fprintf(stderr, "abctl: %s: %v\n", argv[0], err)
		return 1
	}

	// The relay starts only AFTER Start returns. cmd.Process is written by Start,
	// and reading it from the goroutine concurrently is a genuine data race that
	// -race catches — the process handle, not just a stale read. Capturing it here
	// means the goroutine touches nothing cmd owns.
	proc := cmd.Process
	relayDone := make(chan struct{})
	// Registered AFTER the goroutine's own defer below, so LIFO order runs
	// signal.Stop first and then closes relayDone: deliveries stop before the reader
	// goes away, rather than the other way round. (No bug either way — sigs is
	// buffered and os/signal drops instead of blocking on a full channel — but the
	// previous comment claimed this order while the code did the reverse.)
	defer close(relayDone)
	defer signal.Stop(sigs)
	go func() {
		for {
			select {
			case sig := <-sigs:
				// Absorbed signals are caught so abctl survives them, and deliberately
				// NOT forwarded: the child already got them from the tty.
				if slices.Contains(absorbed, sig) {
					continue
				}
				// Best-effort: the child may have exited between the signal and here,
				// which is a benign race, not an error worth reporting.
				_ = proc.Signal(sig)
			case <-relayDone:
				return
			}
		}
	}()

	if err := cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// A child killed by a signal has no exit code; ExitCode() returns -1.
			// Report it the way a shell does (128+signum) rather than passing -1 up,
			// which os.Exit would turn into 255 and lose the cause.
			if code := ee.ExitCode(); code >= 0 {
				return code
			}
			return signaledExitCode(ee)
		}
		// Not an ExitError: the command existed but could not be executed at all
		// (a directory, no exec bit, bad interpreter line).
		fmt.Fprintf(stderr, "abctl: %s: %v\n", argv[0], err)
		return 1
	}
	return 0
}

// execEnv builds the variables to inject for one child.
func execEnv(statsURL string) (env map[string]string, missingBundle string, err error) {
	// The RUNNING proxy's config, not ~/.cortex/config.yaml. exec's whole job is to
	// point a child at a proxy that has to be up for it to work, so the values must
	// come from the process that will serve it — a file can have been edited since
	// boot (listener addresses are not hot-reloaded) and says nothing about whether
	// anything is listening.
	cfg, err := runningConfig(statsURL)
	if err != nil {
		return nil, "", err
	}
	// wanted() still owns the derivation from a config to the env vars, so exec and
	// `claude-code enable` produce identical values for the same Cortex; only the
	// SOURCE of the config differs (live process here, file there — enable writes
	// settings for sessions that outlive any one proxy run).
	want, err := wantedFromLoaded(cfg, "the Cortex at "+statsURL)
	if err != nil {
		return nil, "", err
	}
	proxy := want[envProxy]
	if proxy == "" {
		return nil, "", fmt.Errorf("the Cortex at %s yields no proxy address", statsURL)
	}

	// The bridge posture, not just the CA path. Shared with `claude-code enable`
	// via bridgeEnabled/errBridgeDisabled so the two commands cannot disagree about
	// it — see errBridgeDisabled for why a disabled bridge has to be refused.
	if !bridgeEnabled(cfg) {
		return nil, "", errBridgeDisabled("the Cortex at " + statsURL)
	}
	if want[envCACerts] == "" {
		// Refused rather than injecting the proxy alone. Without a trusted CA the
		// bridge cannot terminate TLS, so every https request either fails
		// verification or — worse, if the tool is lenient — tunnels through
		// unparsed, which looks exactly like success while Cortex sees nothing.
		return nil, "", fmt.Errorf("the Cortex at %s has no tls_bridge.ca_dir, so the child would\n"+
			"  have no CA to trust and https through the bridge would fail verification.\n"+
			"  Enable the TLS bridge first", statsURL)
	}

	out := make(map[string]string, len(want)+len(execProxyVars))
	// Every managed key except the Claude-Code-only telemetry switch: that is a
	// setting for one application, and exec's child is usually not that application.
	for _, k := range managedKeys {
		if k == envNoTelem {
			continue
		}
		if v := want[k]; v != "" {
			out[k] = v
		}
	}
	// The lowercase proxy spellings, plus HTTP_PROXY alongside wanted()'s HTTPS_PROXY.
	for _, k := range execProxyVars {
		out[k] = proxy
	}

	// Omit the REPLACING variables when the bundle is not on disk yet.
	//
	// Cortex writes bundle.crt on boot (tlsbridge.EnsureTrustBundle), and running
	// before that first start is legitimate. But those four names replace the trust
	// store rather than extend it, so pointing them at a missing file does not fall
	// back to the platform roots — git, curl and Python refuse outright ("error
	// setting certificate verify locations") and EVERY https request fails, bridged
	// or not. Leaving them unset costs only the bridged hosts, which is strictly
	// better and is what the README describes.
	//
	// NODE_EXTRA_CA_CERTS stays either way: it is additive, so a missing file makes
	// Node warn and keep its own roots — the same state `claude-code enable` has
	// always written.
	//
	// Here rather than in runExec, so the invariant travels with the derivation: any
	// caller of execEnv gets an environment that is safe to hand a child.
	if b := out[envSSLCert]; b != "" {
		if _, serr := os.Stat(b); serr != nil {
			for _, k := range bundleKeys {
				delete(out, k)
			}
			missingBundle = b
		}
	}
	return out, missingBundle, nil
}

// mergeEnv layers inject over env, replacing rather than appending.
//
// exec.Cmd passes Env to execve as given, and POSIX leaves duplicate names
// undefined: Go's own os.Getenv in a child takes the FIRST match, glibc takes the
// first, but some runtimes and most shells take the last. Appending would
// therefore set the variable for some children and not others depending on
// whether the user already had HTTPS_PROXY set — the one case where getting it
// right matters most. Replacing in place leaves exactly one entry per name.
//
// Comparison is case-sensitive on every platform, which is correct here: the
// lowercase and uppercase proxy spellings are distinct variables that we set
// deliberately, and folding them would collapse the four proxy names into one.
// (Windows environments are case-insensitive, but abctl's proxy/CA story is a Unix
// one.)
func mergeEnv(env []string, inject map[string]string) []string {
	out := make([]string, 0, len(env)+len(inject))
	seen := make(map[string]bool, len(inject))
	for _, kv := range env {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			// Not a NAME=VALUE pair. Passed through untouched rather than dropped:
			// it came from our own environment and is none of our business.
			out = append(out, kv)
			continue
		}
		if v, override := inject[name]; override {
			if seen[name] {
				// A duplicate of a name we are overriding: drop it, so the child sees
				// one entry whichever end its runtime reads from.
				continue
			}
			seen[name] = true
			out = append(out, name+"="+v)
			continue
		}
		out = append(out, kv)
	}
	// Names that were not in the environment at all.
	rest := make([]string, 0, len(inject))
	for k := range inject {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	// Sorted, so the environment we hand the child is byte-identical run to run.
	// Map order is randomised, and a non-deterministic env makes a failure that
	// depends on it unreproducible.
	sort.Strings(rest)
	for _, k := range rest {
		out = append(out, k+"="+inject[k])
	}
	return out
}

// printExecEnv writes the variables as shell export lines.
//
// Driven from the map itself, not a hand-kept name list: the previous version
// iterated a local slice and so silently omitted GIT_SSL_CAINFO the moment authlib
// added it to the managed set. Proxy names first in declaration order, then the CA
// names sorted, so the output is stable run to run and diffable.
func printExecEnv(inject map[string]string, stdout io.Writer) {
	seen := make(map[string]bool, len(inject))
	emit := func(k string) {
		if v, ok := inject[k]; ok && !seen[k] {
			seen[k] = true
			fmt.Fprintf(stdout, "export %s=%s\n", k, shellQuote(v))
		}
	}
	for _, k := range execProxyVars {
		emit(k)
	}
	rest := make([]string, 0, len(inject))
	for k := range inject {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		emit(k)
	}
}

// shellQuote makes s safe to eval. Paths can contain spaces (macOS
// "Application Support" is the common one), and --print is documented as
// eval-able, so an unquoted value would split mid-path and export a truncated CA
// file — which fails verification with no hint as to why.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	// Single quotes make everything literal, so the only character needing care is
	// the single quote itself: close, escape, reopen.
	if !strings.ContainsAny(s, "'") && !strings.ContainsAny(s, " \t\n\"$`\\*?[]{}();&|<>#~!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// cutArgs splits args at the first occurrence of sep, reporting whether it was
// found. Unlike strings.Cut's byte semantics, matching is on whole arguments, so
// a child argument that merely contains "--" is not a delimiter.
func cutArgs(args []string, sep string) (before, after []string, found bool) {
	for i, a := range args {
		if a == sep {
			return args[:i], args[i+1:], true
		}
	}
	return args, nil, false
}

// signaledExitCode renders a death by signal the way a shell does: 128+signum.
//
// ExitCode() returns -1 for a signaled child, and os.Exit(-1) becomes 255, which
// says "failed" but loses which signal — the difference between a Ctrl-C
// (SIGINT, 130) and an out-of-memory kill (SIGKILL, 137) is exactly what someone
// reading an exit status wants. Falls back to 1 when the platform does not give
// us a WaitStatus to ask.
func signaledExitCode(ee *exec.ExitError) int {
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return 1
}
