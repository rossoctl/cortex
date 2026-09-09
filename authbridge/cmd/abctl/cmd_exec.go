package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

// execCAVars are the names that get the bridge CA path. Same reasoning: each
// runtime invented its own. NODE_EXTRA_CA_CERTS is Node, CURL_CA_BUNDLE is
// libcurl, REQUESTS_CA_BUNDLE is Python requests, SSL_CERT_FILE is OpenSSL and
// most things built on it.
//
// Every one is a *_FILE / *_BUNDLE, not a directory, so all four take the same
// ca.crt path; a tool wanting a hashed CA directory (SSL_CERT_DIR) is not served
// here.
var execCAVars = []string{"NODE_EXTRA_CA_CERTS", "CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE", "SSL_CERT_FILE"}

const execUsage = `abctl exec — run a command with Cortex's proxy and CA in its environment

Usage:
  abctl exec [--config PATH] [--print] -- COMMAND [ARG...]

Everything after -- is passed to COMMAND exactly as given; abctl does not
interpret it, so the command's own flags need no escaping:

  abctl exec -- curl -sv https://api.anthropic.com/v1/messages
  abctl exec -- claude --dangerously-skip-permissions
  abctl exec -- bob

The child inherits your environment plus these, read from ~/.cortex/config.yaml
so they always match the running proxy:

  HTTP_PROXY  HTTPS_PROXY  http_proxy  https_proxy      the forward proxy URL
  NODE_EXTRA_CA_CERTS  CURL_CA_BUNDLE                   the bridge CA file
  REQUESTS_CA_BUNDLE   SSL_CERT_FILE

Values already in your environment are replaced for this child only; nothing is
exported to your shell and no file is modified. Signals go to the child, and
abctl exits with the child's exit status, so it is safe in a pipeline or a
Makefile.

Flags:
  --config PATH  Cortex config to read addresses from (default ~/.cortex/config.yaml)
  --print        print the variables that would be set and exit, without running
                 anything. Shell-quoted, so: eval "$(abctl exec --print --)"

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
	if !found {
		fmt.Fprint(stderr, execUsage)
		return 2
	}

	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, execUsage) }
	cortexCfg := fs.String("config", "", "Cortex config file")
	printOnly := fs.Bool("print", false, "print the variables instead of running a command")
	if err := fs.Parse(before); err != nil {
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

	if *cortexCfg == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			fmt.Fprintf(stderr, "abctl: cannot determine your home directory: %v\n", err)
			return 1
		}
		*cortexCfg = filepath.Join(home, cortexCfgRel)
	}

	inject, err := execEnv(*cortexCfg)
	if err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}

	// A CA path that does not exist yet is a warning, not an error: enabling
	// before the first start is legitimate and the proxy writes it on boot. But
	// unlike the settings path, here the failure is loud rather than silent —
	// Node and curl both refuse to start with a CA file they cannot read — so it
	// is worth naming the cause before the child dies of it.
	if ca := inject[execCAVars[0]]; ca != "" {
		if _, serr := os.Stat(ca); serr != nil {
			fmt.Fprintf(stderr, "abctl: note: %s does not exist yet — Cortex writes it on first start.\n"+
				"  TLS verification against the bridge will fail until then (abctl service start).\n", ca)
		}
	}

	if *printOnly {
		printExecEnv(inject, stdout)
		return 0
	}
	if len(cmdArgs) == 0 {
		fmt.Fprintln(stderr, "abctl: nothing to run after --")
		fmt.Fprint(stderr, execUsage)
		return 2
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
	cmd.Env = mergeEnv(os.Environ(), inject)
	// The child gets this process's real stdio, not a pipe: it may be interactive
	// (`abctl exec -- claude`), and a pipe would cost it the terminal, so it would
	// disable colour, refuse to prompt, or line-buffer its output. stdout/stderr
	// are still honoured when a caller passed something else, which is what makes
	// this testable.
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	// No signal forwarding, deliberately. The child is in this process's process
	// group and shares the terminal, so a Ctrl-C at the keyboard is delivered by
	// the tty driver to the whole group — the child included — and re-sending it
	// would double the signal. Interactive children (claude, a REPL) rely on
	// receiving SIGINT themselves; abctl dying first would orphan them mid-write.
	// abctl has nothing of its own to clean up, so it just waits and reports.
	if err := cmd.Run(); err != nil {
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

// execEnv builds the variables to inject, fanning the two values wanted()
// derives out over the eight names tools actually read.
//
// Reusing wanted() rather than re-reading the config is the point: exec and
// `claude-code enable` must agree about the proxy address and the CA, and the
// only way to guarantee that is one derivation. The telemetry key wanted() also
// returns is dropped here — it is a Claude Code setting, and exec's child is
// usually not Claude Code.
func execEnv(cortexCfgPath string) (map[string]string, error) {
	want, err := wanted(cortexCfgPath)
	if err != nil {
		return nil, err
	}
	proxy := want[envProxy]
	if proxy == "" {
		return nil, fmt.Errorf("%s yields no proxy address", cortexCfgPath)
	}
	ca, ok := want[envCACerts]
	if !ok || ca == "" {
		// Refused rather than injecting the proxy alone. Without a trusted CA the
		// bridge cannot terminate TLS, so every https request either fails
		// verification or — worse, if the tool is lenient — tunnels through
		// unparsed, which looks exactly like success while Cortex sees nothing.
		return nil, fmt.Errorf("%s has no tls_bridge.ca_dir, so the child would have no CA to trust;\n"+
			"  https requests through the bridge would fail verification. Enable the TLS bridge first", cortexCfgPath)
	}
	out := make(map[string]string, len(execProxyVars)+len(execCAVars))
	for _, k := range execProxyVars {
		out[k] = proxy
	}
	for _, k := range execCAVars {
		out[k] = ca
	}
	return out, nil
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
// deliberately, and folding them would collapse four of the eight names into
// one. (Windows environments are case-insensitive, but abctl's proxy/CA story is
// a Unix one.)
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

// printExecEnv writes the variables as shell export lines, in a fixed order:
// proxy names then CA names, each group in the order declared, so the output is
// diffable and reads as the two decisions it is rather than eight unrelated ones.
func printExecEnv(inject map[string]string, stdout io.Writer) {
	for _, k := range append(append([]string{}, execProxyVars...), execCAVars...) {
		if v, ok := inject[k]; ok {
			fmt.Fprintf(stdout, "export %s=%s\n", k, shellQuote(v))
		}
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
