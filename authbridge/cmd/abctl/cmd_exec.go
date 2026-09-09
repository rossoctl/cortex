package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
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

// execCAExtraVars are the ADDITIVE trust variables: the runtime keeps its own
// root store and adds this file to it. Node is the only one of the four that
// works this way.
var execCAExtraVars = []string{"NODE_EXTRA_CA_CERTS"}

// execCAReplaceVars REPLACE the trust store: whatever file they name becomes the
// complete set of roots. CURL_CA_BUNDLE is libcurl, REQUESTS_CA_BUNDLE is Python
// requests, SSL_CERT_FILE is OpenSSL and most things built on it.
//
// This distinction is the whole reason execEnv writes a bundle instead of
// pointing everything at ca.crt. ca.crt is a single certificate — the bridge CA
// alone (tlsbridge/ca.go writes certPEM to it, not a chain) — so naming it here
// would leave the child trusting exactly one CA and nothing else. Every request
// the bridge does NOT terminate then fails verification against the real
// upstream certificate: tls_bridge.passthrough_hosts, listener.skip_hosts, any
// port outside tls_bridge.ports (default 443 + 8443), and the runtime
// passthrough decisions in tlsbridge/decision.go (non-TLS bytes, skip list).
// Verified rather than assumed: on a Linux/OpenSSL curl, CURL_CA_BUNDLE pointed
// at a lone CA fails `curl https://example.com` with exit 77.
//
// So these get a concatenation of the system roots and the bridge CA, written to
// a temp file for the child's lifetime. Bridged hosts verify against the bridge
// CA; everything excluded from bridging still verifies against the public roots
// it would have used anyway.
var execCAReplaceVars = []string{"CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE", "SSL_CERT_FILE"}

// execCAVars is every CA name, in the order --print emits them.
var execCAVars = append(append([]string{}, execCAExtraVars...), execCAReplaceVars...)

// systemRootFiles are the usual system CA bundle locations, in the order
// crypto/x509 itself consults them (root_linux.go's certFiles, plus the common
// BSD/macOS paths). The first one that exists is treated as the system store.
//
// Read from disk rather than via x509.SystemCertPool because the output has to
// be a PEM *file* another process can open: SystemCertPool returns an opaque
// pool whose certificates cannot be re-serialised (the DER is not retained on
// every platform), so there is nothing to write back out.
var systemRootFiles = []string{
	"/etc/ssl/certs/ca-certificates.crt",                // Debian/Ubuntu/Alpine(ish)
	"/etc/pki/tls/certs/ca-bundle.crt",                  // Fedora/RHEL 6
	"/etc/ssl/ca-bundle.pem",                            // OpenSUSE
	"/etc/pki/tls/cacert.pem",                           // OpenELEC
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", // CentOS/RHEL 7
	"/etc/ssl/cert.pem",                                 // Alpine, macOS (Homebrew OpenSSL)
}

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
  NODE_EXTRA_CA_CERTS                                   the bridge CA (added to
                                                        the runtime's own roots)
  CURL_CA_BUNDLE  REQUESTS_CA_BUNDLE  SSL_CERT_FILE     a temporary bundle of the
                                                        system roots + bridge CA

The last three REPLACE the trust store rather than extend it, so they get a
bundle: naming the bridge CA alone would leave the child trusting one CA and
break every host the bridge does not terminate (passthrough_hosts, skip_hosts,
ports outside tls_bridge.ports). Requires tls_bridge.mode: enabled.

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

	inject, cleanup, err := execEnv(*cortexCfg, *printOnly)
	// Registered before the error branches below, so "cleanup always runs" is
	// enforced in one place. Nothing leaks today — writeTrustBundle removes its own
	// file on every error path — but that invariant was being held in two places,
	// and the next error return added there would have broken it silently.
	if cleanup != nil {
		defer cleanup()
	}
	// errNoSystemRoots comes back WITH a usable bundle: the bridge CA is in it, so
	// bridged hosts verify fine; only public trust is absent, and on an image with
	// no root store there was none to begin with. Reported, not fatal.
	if errors.Is(err, errNoSystemRoots) {
		fmt.Fprintf(stderr, "abctl: note: no system CA bundle found on this machine, so the child\n"+
			"  trusts only Cortex's bridge CA. Hosts the bridge does not terminate\n"+
			"  (tls_bridge.passthrough_hosts, listener.skip_hosts, ports outside\n"+
			"  tls_bridge.ports) will fail certificate verification.\n")
		err = nil
	}
	if err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}
	// A CA path that does not exist yet is a warning, not an error: enabling
	// before the first start is legitimate and the proxy writes it on boot. But
	// unlike the settings path, here the failure is loud rather than silent —
	// Node and curl both refuse to start with a CA file they cannot read — so it
	// is worth naming the cause before the child dies of it.
	//
	// Checked on the additive name, which is the bridge CA itself; the replacing
	// vars name the generated bundle, which by construction exists. Named
	// explicitly rather than by list position, so this warning does not depend on
	// the declaration order of a slice it does not own.
	if ca := inject[envCACerts]; ca != "" {
		if _, serr := os.Stat(ca); serr != nil {
			fmt.Fprintf(stderr, "abctl: note: %s does not exist yet — Cortex writes it on first start.\n"+
				"  TLS verification against the bridge will fail until then (abctl service start).\n", ca)
		}
	}

	if *printOnly {
		// Say so rather than silently discard it. Refusing outright would break
		// `abctl exec --print --`, the documented form, and rejecting only the
		// with-a-command form is a usage error for something harmless — but staying
		// silent about an ignored command is the same discourtesy the strict check
		// above exists to avoid.
		if len(cmdArgs) > 0 {
			fmt.Fprintf(stderr, "abctl: --print only prints the variables; %q was not run.\n",
				strings.Join(cmdArgs, " "))
		}
		printExecEnv(inject, stdout)
		if b := inject[execCAReplaceVars[0]]; b != "" {
			fmt.Fprintf(stderr, "abctl: wrote the trust bundle to %s (system roots + Cortex's bridge CA).\n"+
				"  It is rewritten on each --print, so a rotated CA is picked up.\n", b)
		}
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

	// Signal handling splits by how the signal arrives.
	//
	// tty-generated signals (SIGINT/SIGQUIT/SIGTSTP from a keystroke) are delivered
	// by the tty driver to the whole foreground process group, so the child already
	// gets them and relaying would double the signal. Those are left alone —
	// interactive children rely on handling their own Ctrl-C.
	//
	// A signal aimed at abctl's PID is different: `timeout 30 abctl exec -- …`, a CI
	// runner, or systemd sends SIGTERM to abctl alone. Go's default disposition then
	// kills abctl and leaves the child running, orphaned, with the injected
	// environment — and `timeout` in a Makefile is exactly what "safe in a pipeline
	// or a Makefile" claims to support. So SIGTERM and SIGHUP are relayed, then abctl
	// keeps waiting and still reports the child's real status.
	//
	// Notify BEFORE Start, not after: a SIGTERM landing in the window between them
	// would hit Go's default disposition and kill abctl with the child already
	// running — orphaning it with the injected environment, the exact failure this
	// relay exists to prevent. The channel is buffered, so a signal arriving during
	// that window is held and delivered to the relay below rather than dropped.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP)
	// Stopped before the relay goroutine is told to exit, so nothing can be queued
	// onto a channel with no reader.
	defer signal.Stop(sigs)

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "abctl: %s: %v\n", argv[0], err)
		return 1
	}

	// The relay starts only AFTER Start returns. cmd.Process is written by Start,
	// and reading it from the goroutine concurrently is a genuine data race that
	// -race catches — the process handle, not just a stale read. Capturing it here
	// means the goroutine touches nothing cmd owns.
	proc := cmd.Process
	relayDone := make(chan struct{})
	defer close(relayDone)
	go func() {
		for {
			select {
			case sig := <-sigs:
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

// execEnv builds the variables to inject, fanning the two values wanted()
// derives out over the eight names tools actually read.
//
// Reusing wanted() rather than re-reading the config is the point: exec and
// `claude-code enable` must agree about the proxy address and the CA, and the
// only way to guarantee that is one derivation. The telemetry key wanted() also
// returns is dropped here — it is a Claude Code setting, and exec's child is
// usually not Claude Code.
// persist selects where the trust bundle goes: a stable path beside the CA (for
// --print, whose output must outlive this process) or a temp file removed by
// cleanup (for a child, which reads it while running).
func execEnv(cortexCfgPath string, persist bool) (env map[string]string, cleanup func(), err error) {
	want, cfg, err := wantedFromConfig(cortexCfgPath)
	if err != nil {
		return nil, nil, err
	}
	proxy := want[envProxy]
	if proxy == "" {
		return nil, nil, fmt.Errorf("%s yields no proxy address", cortexCfgPath)
	}

	// The bridge posture, not just the CA path. Shared with `claude-code enable`
	// via bridgeEnabled/errBridgeDisabled so the two commands cannot disagree about
	// it — see errBridgeDisabled for why a disabled bridge has to be refused.
	if !bridgeEnabled(cfg) {
		return nil, nil, errBridgeDisabled(cortexCfgPath)
	}

	ca, ok := want[envCACerts]
	if !ok || ca == "" {
		// Refused rather than injecting the proxy alone. Without a trusted CA the
		// bridge cannot terminate TLS, so every https request either fails
		// verification or — worse, if the tool is lenient — tunnels through
		// unparsed, which looks exactly like success while Cortex sees nothing.
		return nil, nil, fmt.Errorf("%s has no tls_bridge.ca_dir, so the child would have no CA to trust;\n"+
			"  https requests through the bridge would fail verification. Enable the TLS bridge first", cortexCfgPath)
	}

	out := make(map[string]string, len(execProxyVars)+len(execCAVars))
	for _, k := range execProxyVars {
		out[k] = proxy
	}
	// Additive names take the bridge CA directly — the runtime keeps its own roots.
	for _, k := range execCAExtraVars {
		out[k] = ca
	}
	// Replacing names take a bundle, so excluded hosts keep public trust.
	bundle, cleanup, berr := writeTrustBundle(ca, persist)
	switch {
	case errors.Is(berr, errNoBridgeCA):
		// The CA is not on disk yet — Cortex writes it on first start, and enabling
		// before then is legitimate (the caller warns about it). Leave the replacing
		// vars UNSET rather than name a bundle without the bridge CA in it: unset
		// means the child keeps its own public roots and only bridged hosts fail,
		// which is the same state `claude-code enable` has always produced. Naming a
		// bridge-CA-less bundle would instead break every host, bridged or not.
		return out, cleanup, nil
	case berr != nil && !errors.Is(berr, errNoSystemRoots):
		return nil, cleanup, berr
	}
	for _, k := range execCAReplaceVars {
		out[k] = bundle
	}
	// errNoSystemRoots is returned with a usable bundle; propagate it so the caller
	// can report the missing public trust without treating it as a failure.
	return out, cleanup, berr
}

// writeTrustBundle concatenates the system roots with the bridge CA and returns
// the path to the result, plus a cleanup to remove it.
//
// Ordering is system-roots-then-bridge-CA, but only for readability: PEM trust
// files are an unordered set, and every consumer parses all of them.
//
// A missing system store is not fatal. A distroless or scratch-based image may
// genuinely have no root bundle, in which case the bridge CA alone is the whole
// trust set — which is exactly right there, since such an image had no public
// trust to lose. It is reported so the difference is not silent.
// When persist is true the bundle is written to a STABLE path beside the CA
// (ca_dir/trust-bundle.pem) and no cleanup is returned, because the caller is
// --print: the exported path has to outlive this process for
// `eval "$(abctl exec --print --)"` to mean anything. A temp file per eval would
// also leak with no owner, so a single rewritten file beside the CA it is derived
// from is both durable and self-limiting.
func writeTrustBundle(caPath string, persist bool) (path string, cleanup func(), err error) {
	caPEM, err := os.ReadFile(caPath) //nolint:gosec // path derived from the operator's own config
	if err != nil {
		// Signalled, not fatal: the CA is written by Cortex on first start and exec
		// deliberately works before that. The caller decides what to do — it leaves
		// the replacing vars unset, because a bundle without the bridge CA is worse
		// than no bundle at all.
		return "", func() {}, fmt.Errorf("%w: %s: %v", errNoBridgeCA, caPath, err)
	}

	var buf []byte
	for _, f := range systemRootFiles {
		// Stat first: some of these paths are a DIRECTORY on some distros
		// (/etc/ssl/certs is a hashed cert dir on Alpine), and ReadFile on a
		// directory fails on Linux but succeeds with garbage on some systems.
		if fi, serr := os.Stat(f); serr != nil || fi.IsDir() {
			continue
		}
		b, rerr := os.ReadFile(f) //nolint:gosec // fixed list of well-known system paths
		if rerr == nil && len(b) > 0 {
			buf = append(buf, b...)
			if !bytes.HasSuffix(buf, []byte("\n")) {
				buf = append(buf, '\n')
			}
			break
		}
	}
	systemFound := len(buf) > 0
	buf = append(buf, caPEM...)

	if persist {
		// Beside the CA, not in the temp dir: --print's output must still resolve
		// after this process exits. Rewritten in place each run so a rotated CA is
		// picked up, and atomically so a concurrent reader never sees a half-written
		// trust store.
		name := filepath.Join(filepath.Dir(caPath), "trust-bundle.pem")
		tmp := name + ".tmp"
		// 0644, matching ca.crt: a trust store is public material, and a file only
		// the writing user can read would break a child running as anyone else.
		if werr := os.WriteFile(tmp, buf, 0o644); werr != nil { //nolint:gosec // trust anchors are not secret
			return "", func() {}, werr
		}
		if rerr := os.Rename(tmp, name); rerr != nil {
			_ = os.Remove(tmp)
			return "", func() {}, rerr
		}
		if !systemFound {
			return name, func() {}, errNoSystemRoots
		}
		return name, func() {}, nil
	}

	// 0600 in the process's temp dir: this is a trust store, and a world-writable
	// one would let any local user add a root the child then trusts.
	f, err := os.CreateTemp("", "abctl-trust-*.pem")
	if err != nil {
		return "", func() {}, err
	}
	name := f.Name()
	rm := func() { _ = os.Remove(name) }
	if _, werr := f.Write(buf); werr != nil {
		f.Close()
		rm()
		return "", func() {}, werr
	}
	if cerr := f.Close(); cerr != nil {
		rm()
		return "", func() {}, cerr
	}
	if !systemFound {
		return name, rm, errNoSystemRoots
	}
	return name, rm, nil
}

// errNoSystemRoots is returned alongside a usable bundle when no system CA store
// was found, so the caller can say so without treating it as a failure.
var errNoSystemRoots = errors.New("no system CA bundle found")

// errNoBridgeCA means ca.crt is not on disk yet, which is an expected state
// before Cortex's first start rather than a misconfiguration.
var errNoBridgeCA = errors.New("bridge CA not written yet")

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
