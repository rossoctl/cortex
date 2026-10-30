package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/tlsbridge"
)

const (
	// bobSettingsRel is where IBM Bob keeps its user settings on macOS. Bob is a
	// VS Code fork, so this is VS Code's own layout with Bob's product name in it.
	bobSettingsRel = "Library/Application Support/IBM Bob/User/settings.json"

	// bobProxyKey is the setting this command writes on every platform (macOS adds
	// bobSysCertsKey). VS Code's settings document is FLAT — dotted keys are literal
	// top-level keys, not a nested object — so this is the whole path, unlike
	// claude-code's "env" block.
	bobProxyKey = "http.proxy"

	// bobCACommonName is the subject every generated bridge CA carries. A literal
	// rather than an import: bridgeCACommonName lives in package main of
	// cmd/cortex and is not importable from here. darwinGoNote already
	// carries the same copy for the same reason.
	bobCACommonName = "authbridge-tls-bridge-ca"

	// bobSystemKeychain is where earlier versions of enable had the user trust the
	// CA. Kept for bobUntrustNote, which prints the undo for anyone who did.
	bobSystemKeychain = "/Library/Keychains/System.keychain"
)

const bobUsage = `agentop configure bob — route Bob, editor and shell both, through Cortex

Usage:
  agentop configure bob enable  [--yes] [--settings PATH] [--config PATH]
  agentop configure bob disable [--yes] [--settings PATH] [--config PATH]
  agentop configure bob status  [--settings PATH] [--config PATH]

Flags:
  --yes            do not prompt for confirmation
  --settings PATH  IBM Bob's settings file. Defaults, on macOS only, to
                   ~/Library/Application Support/IBM Bob/User/settings.json
  --config PATH    Cortex config to read the proxy address from
                   (default ~/.cortex/config.yaml)

Bob is two faces of one agent and this command configures both:

  the editor     IBM Bob is a VS Code fork, so it has a settings file and flat
                 keys decide where its networking goes. enable sets "http.proxy"
                 to Cortex's forward proxy, read from ~/.cortex/config.yaml so
                 the address always matches the proxy that is actually running.
                 Every other setting is left exactly as it was, and the first
                 write copies the original to settings.json.bak and never
                 overwrites that copy. Bob writes settings.json only once a
                 setting has changed, so on a Bob still on its defaults enable
                 creates the file; it refuses only when Bob's settings directory
                 is missing too, which means Bob has not run here.
  the shell      a "bob" function is appended to your shell's rc file (~/.zshrc
                 for zsh, ~/.bashrc for bash) that runs "agentop exec -- bob", so
                 typing "bob" runs through Cortex. Open a new terminal, or source
                 the file, for it to take effect.

The two halves are computed together and confirmed ONCE: the preview lists every
change to settings.json and your rc file under a single Apply? prompt (--yes
skips it), and a declined prompt writes nothing. The writes themselves are
independent — a failure in one half does not stop the other — and the exit code
is the worse of the two: 1 if either half failed, 3 if the prompt was declined,
else 0.

The proxy is only half of it: Bob must also trust Cortex's CA. On macOS enable
does that without touching the keychain, in two more changes:

  "http.systemCertificates": false in settings.json, so Bob's extensions use
      Node's own trust store, which takes NODE_EXTRA_CA_CERTS
  a block in your shell's rc file that exports NODE_EXTRA_CA_CERTS for IBM Bob
      only — Bob runs your login shell when it starts from the Dock or Spotlight
      and takes its environment. The file is the shell half's: ~/.zshrc for zsh,
      ~/.bashrc for bash; any other shell gets the block printed to place
      yourself.

enable then starts your login shell the way Bob does to check the block is read,
and warns if it is not — bash's login shell reads ~/.bash_profile, which has to
source ~/.bashrc. Quit Bob and reopen it from the Dock or Spotlight afterwards: a
Bob started from a terminal takes that terminal's environment instead. Off macOS,
enable prints how to add the CA to the system trust store; that needs sudo, so
agentop does not do it for you.

disable removes "http.proxy" ONLY when it points at Cortex — a corporate proxy,
say, is reported and left alone — "http.systemCertificates" only when enable
recorded adding it (in ~/.cortex/bob-state.json) and it is still false, and each
rc block only when it is exactly what enable wrote: the launch-environment block
and the shell half's "bob" function. A missing settings.json is not an error:
there is simply nothing to remove from it. status reports all of it from the
files and runs nothing.

On a platform where IBM Bob's settings path is not known (any but macOS, unless
--settings names a file), the editor half is skipped with a note and the shell
half still runs: the rc file is where it always was, and the exit code is 1 —
the skipped half's change did not happen.

Comments: VS Code permits them in settings.json and this command does not. It
reads the file as strict JSON and refuses a commented one by name rather than
rewriting it, because a rewrite would silently delete the comments.

Exit status: 0 applied, already correct, or reported; 3 declined — at the prompt
or because there was no terminal to ask on; 1 something went wrong; 2 a usage
error.
`

// bobConfirm is the merged command's ONE prompt, covering every write the
// preview lists. A var so tests can substitute it: `go test` inherits the
// terminal it was launched from, so an unstubbed prompt blocks waiting on a
// human.
//
// It is confirm itself rather than a wrapper, because there is nothing left for
// a wrapper to say — the preview above the prompt names the files, and the
// question is the shared "Apply? [y/N]". Until 2026-10-16 each half prompted for
// its own write and this was two closures; one confirmation for one list of
// changes left them nothing to differ in.
var bobConfirm = confirm

// runBob dispatches `agentop configure bob`, the ONE command that configures both of
// Bob's faces. Returns the process exit code.
//
// Bob is two faces of one agent — the IBM Bob editor and the "bob" shell function —
// and this command configures both. They were two subcommands (`configure bob` and
// `configure bobshell`) until 2026-10-15, when the agent converged on one name and
// one command; until 2026-10-16 the merge still ran each half as its own prompted
// flow, which asked the user the same question twice and printed two reports. The
// halves are now computed whole and confirmed once, over plan/apply seams the tests
// drive directly (bobEnablePlan, bobShellEnablePlan, ...).
//
// THE HALVES ARE INDEPENDENT BY DESIGN where independence is about the OUTCOME, not
// the consent: a write failure in one does not stop the other, because neither needs
// the other to work — the editor reads its settings.json whatever the shell does,
// and the rc function is complete whatever the settings say. What the caller learns
// is the WORSE of the two outcomes, so a script that checks the exit code is not
// told "done" when half of it was not. The prompt is the one thing that is NOT
// per-half: one consent covers every write the preview lists, and a refusal
// declines both halves together.
func runBob(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, bobUsage)
		return 2
	}
	action := args[0]
	// Before anything else: --help asks for this command's usage, and reading it
	// as an action name would send someone looking for the command list to the one
	// branch that refuses to print it. Same split as claude-code's.
	switch action {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, bobUsage)
		return 0
	}

	// The verb is validated HERE, before any environment or filesystem work, for
	// the reason the halves' plans document at length: a step below can answer
	// successfully on its own — bobSettingsPath's off-macOS arm prints advice —
	// so validating the action last would report success for a verb that does not
	// exist. Nothing downstream can reach this check, so it comes first.
	switch action {
	case "enable", "disable", "status":
	default:
		fmt.Fprintf(stderr, "agentop: unknown bob action %q (enable, disable, status)\n", action)
		return 2
	}

	fs := flag.NewFlagSet("configure bob "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	settingsPath := fs.String("settings", "", "IBM Bob settings file")
	// Registered for all three verbs, status included: status reports whether the
	// value still matches the proxy the config names now, which needs the config.
	cortexCfgPath := fs.String("config", "", "Cortex config file")
	// --yes for enable and disable ONLY. status writes nothing, so it has nothing
	// to confirm; registering it unconditionally would make `status --yes` parse
	// and be silently ignored, which is the same "accepted and did nothing" that
	// `status extra` is already a usage error for. Read back into a plain bool
	// below so the call sites cannot nil-deref.
	var yesFlag *bool
	if action == "enable" || action == "disable" {
		yesFlag = fs.Bool("yes", false, "do not prompt for confirmation")
	}
	// The FlagSet's own usage would print a bare header and a short flag list;
	// this command's usage is the useful answer, and suppressing it here keeps
	// -h's single copy on stdout below.
	fs.Usage = func() {}
	if err := fs.Parse(args[1:]); err != nil {
		// -h and --help arrive as flag.ErrHelp, and asking for help is not a usage
		// error: stdout and 0. Any other parse failure is real, and Parse has
		// already named it on stderr.
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, bobUsage)
			return 0
		}
		return 2
	}
	yes := yesFlag != nil && *yesFlag
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "agentop: bob %s takes no arguments (got %q)\n", action, fs.Arg(0))
		return 2
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		fmt.Fprintf(stderr, "agentop: cannot determine your home directory: %v\n", err)
		return 1
	}
	if *cortexCfgPath == "" {
		*cortexCfgPath = filepath.Join(home, cortexCfgRel)
	}

	// The shell half's rc file, resolved on EVERY platform — typing "bob" is not
	// a macOS feature — by the same rules resolveBobLaunch applies on macOS, so
	// the two halves never disagree about which file to write or which
	// arrangements are too deliberate to write through.
	shellRC, shellAdvise, shellReadErr := rcResolve(home, os.Getenv("SHELL"))
	if shellReadErr != nil {
		// The editor half's rule, and the old shell command's: a caller that
		// cannot even stat the file has learned nothing it can act on from a
		// success code.
		fmt.Fprintf(stderr, "agentop: %v\n", shellReadErr)
		return 1
	}
	// Resolved once, here, for all three verbs: the rc file the editor half's
	// launch-environment block is written to on macOS is the one the shell half's
	// function block lives in too, so both halves see the same target.
	launch := resolveBobLaunch(home, os.Getenv("SHELL"))

	c := bobCommand{
		home:          home,
		cortexCfgPath: *cortexCfgPath,
		launch:        launch,
		shellRC:       shellRC,
		shellAdvise:   shellAdvise,
		yes:           yes,
		stdout:        stdout,
		stderr:        stderr,
	}
	// The editor half needs a settings path. Where the platform does not name one
	// and --settings did not either, the half is SKIPPED with a note rather than
	// failing the whole command: the shell half has no such dependency, and a user
	// on a platform the editor half does not know must still be able to configure
	// the function. The old single-purpose command exited 2 here; that was right
	// when this command had nothing else to do, and wrong now that it does.
	if *settingsPath == "" {
		p, perr := bobSettingsPath(home)
		if perr != nil {
			switch action {
			case "status":
				// The same verdict status prints for a settings document it
				// cannot treat as Bob's, on the same "report, don't fail" terms:
				// status exits 0 so it stays usable in a shell conditional. The
				// shell half's line still reports below.
				fmt.Fprintf(stdout, "%s\n  editor half: %v\n", bobStatusUnknown, perr)
				return c.statusShellOnly()
			default:
				// 1, like bobNotInstalled: the half's change did not happen, and
				// the note names the flag that would make it possible.
				fmt.Fprintf(stderr, "agentop: %v.\n  The editor half is skipped; the shell half still runs. Pass\n  --settings PATH to configure the editor too.\n", perr)
				c.ideSkip = true
			}
		} else {
			c.settingsPath = p
		}
	} else {
		c.settingsPath = *settingsPath
	}

	switch action {
	case "enable":
		return c.enable()
	case "disable":
		return c.disable()
	default:
		return c.status()
	}
}

// bobCombine picks the merged command's exit code from its halves' own codes: the
// WORSE outcome wins, so a script branching on the code learns about the half that
// did not succeed even when the other did. With one prompt there is no per-half
// declined code — a refusal declines both halves and is decided in the verb — but
// failure (1) still outranks a declined prompt (3), and both outrank success.
// 2 cannot reach here (usage is validated before either half runs) but is ranked
// first anyway, so a future half that answers it cannot silently soften the code.
func bobCombine(ide, shell int) int {
	switch {
	case ide == 2 || shell == 2:
		return 2
	case ide == 1 || shell == 1:
		return 1
	case ide == exitDeclined || shell == exitDeclined:
		return exitDeclined
	}
	return 0
}

// bobCommand carries what the three merged verbs share: the paths both halves
// write, resolved once in runBob so the path that gets checked is the path that
// gets written, and the writers every report goes to. A struct rather than nine
// parameters, because the verbs are methods on it and each needs nearly all of it.
type bobCommand struct {
	home string
	// settingsPath is the editor half's target. Empty with ideSkip set when the
	// platform has no default and --settings named nothing — the shell half
	// still runs.
	settingsPath  string
	ideSkip       bool
	cortexCfgPath string
	launch        bobLaunch
	// shellRC is the shell half's rc file, symlink-resolved, on every platform.
	shellRC string
	// shellAdvise is non-nil when agentop will not write the rc file — an
	// unrecognised $SHELL, a dangling link, a chain past the hop limit — and the
	// block is printed for the user to place instead. Advice, not failure.
	shellAdvise error
	yes         bool
	stdout      io.Writer
	stderr      io.Writer
}

// bobSettingsPath is where IBM Bob's settings live, for the platforms where that
// is known.
//
// macOS only, deliberately. ~/.config/IBM Bob/User/settings.json is the VS Code
// convention on Linux and would be the obvious guess, but it is unverified for
// this app — and writing a proxy setting into a file nothing reads is a silent
// no-op that leaves the user no reason to look there. rcPathForShell makes the
// same call for an unrecognised shell: claim only what you know, and say so
// otherwise. --settings covers every platform, so nothing is blocked.
//
// home is a parameter rather than read from the environment, so the mapping is
// testable and runBob reads $HOME exactly once.
func bobSettingsPath(home string) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", fmt.Errorf("I only know where IBM Bob keeps its settings on macOS, and this is %s", runtime.GOOS)
	}
	return filepath.Join(home, bobSettingsRel), nil
}

// bobOwnership is how sure agentop is that it wrote an http.proxy value.
//
// Three states rather than a bool, because the third one exists and a bool made a
// caller answer it wrongly. Comparing the settings value against the address the
// config names needs BOTH; with no readable config there is no comparison to make,
// and a bool forced that case to report "not ours" — so `disable` told a user whose
// Cortex was uninstalled that Cortex's own default address was "not a Cortex proxy"
// and left it in the file. The off switch must not be broken by the absence of the
// thing being switched off, so "cannot tell" is a state callers have to handle
// rather than a false they can fall into.
type bobOwnership int

const (
	// bobNotOurs: parsed fine and is somebody else's — a corporate proxy, a
	// different port. Never delete.
	bobNotOurs bobOwnership = iota
	// bobOurs: matches the address the config names, host and port.
	bobOurs
	// bobUnknown: the value is a loopback http proxy, but there is no config to
	// compare it with. Shaped like something agentop writes and nothing contradicts
	// it. disable treats this as removable; status says plainly that it is a guess.
	bobUnknown
)

// bobOwns judges an http.proxy value against the address the config names.
//
// Deliberately NOT isCortexValue, which is the same question asked with
// strings.Contains. That form matches a substring anywhere, so
// "http://corp.example.com/?next=127.0.0.1:47600" and the hostname
// "localhost:47600.evil.com" both satisfy it. In claude-code the consequence of
// a false positive is REFUSING to overwrite, so it fails safe; here the
// consequence is delete, and the direction of failure inverts — a false positive
// silently removes someone's corporate proxy. Parsing and comparing Hostname()
// and Port() whole cannot be fooled by a path or a suffixed host.
//
// wantProxy comes from the config rather than from a port-range heuristic. The
// heuristic this replaced tested HasPrefix(port, "476") while documenting itself as
// "Cortex's 476xx block", which is not what a prefix match does: it also accepted
// 476 and 4769999. Comparing against forward_proxy_addr needs no port convention at
// all and is right for a user who moved the port.
//
// With no config, the answer is bobUnknown and not bobNotOurs — see bobOwnership.
// A loopback http proxy is then removable-on-a-guess, which disable does while
// naming the value; a non-loopback one is still somebody else's.
func bobOwns(val, wantProxy string) bobOwnership {
	u, err := url.Parse(strings.TrimSpace(val))
	if err != nil || u.Host == "" {
		return bobNotOurs
	}
	// http only: it is the only scheme enable ever writes, so anything else is
	// someone else's value.
	if u.Scheme != "http" {
		return bobNotOurs
	}
	// Everything enable writes is exactly scheme://host:port — no credentials, no
	// path, no query, no fragment. So anything carrying one of those was typed by
	// somebody else, and this is the check that keeps a delete off it. Without it
	// only Hostname() and Port() were compared, and url.Parse puts the rest in
	// fields nobody looked at:
	//
	//   http://user:secret@localhost:47600  credentials, deleted with the value
	//   http://localhost:47600/proxy.pac    a PAC script, not our proxy
	//   http://localhost:47600/?next=evil   a redirector that happens to be local
	//   http://localhost:47600#frag
	//
	// An empty path and a bare "/" both mean "no path": url.Parse gives "" for
	// http://h:p and "/" for http://h:p/, and enable's own value takes the first
	// form, so accepting both keeps a hand-typed trailing slash ours.
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return bobNotOurs
	}
	if wantProxy == "" {
		// Nothing to compare against. A loopback proxy is shaped like ours and
		// nothing contradicts it; anything else plainly is not.
		if bobIsLoopbackProxy(val) {
			return bobUnknown
		}
		return bobNotOurs
	}
	w, err := url.Parse(wantProxy)
	if err != nil || w.Host == "" {
		if bobIsLoopbackProxy(val) {
			return bobUnknown
		}
		return bobNotOurs
	}
	// Host AND port, compared whole. Hostname() is compared case-insensitively
	// because "LOCALHOST" resolves to the same place, and the loopback spellings are
	// folded together because wantedFromLoaded itself produces "localhost" from an
	// empty / 0.0.0.0 / :: bind address — so the config can name the same listener a
	// different way than the settings file does, and a user who hand-typed
	// 127.0.0.1 must not be told it is someone else's proxy.
	if u.Port() == w.Port() && bobSameLoopback(u.Hostname(), w.Hostname()) {
		return bobOurs
	}
	return bobNotOurs
}

// bobSameLoopback reports whether two hostnames name the same listener.
//
// Exact match, or both are loopback spellings. It does NOT resolve names: a DNS
// lookup would make an ownership test depend on the network, and a resolver that
// maps some.corp.host to 127.0.0.1 would then hand a foreign proxy our ownership.
// The three literal spellings are the ones wantedFromLoaded and a hand-edited
// settings file actually produce.
func bobSameLoopback(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	if a == b {
		return true
	}
	loopback := func(h string) bool {
		switch h {
		case "localhost", "127.0.0.1", "::1", "[::1]":
			return true
		}
		return false
	}
	return loopback(a) && loopback(b)
}

// bobIsLoopbackProxy reports whether val is an http proxy on this machine.
//
// A shape test, not an ownership test: it says nothing about who wrote the value. Every
// loopback http proxy satisfies it, Cortex's or not. bobOwns treats that shape as
// bobUnknown — never as bobOurs — precisely because it cannot tell them apart.
func bobIsLoopbackProxy(val string) bool {
	u, err := url.Parse(strings.TrimSpace(val))
	if err != nil || u.Host == "" || u.Scheme != "http" {
		return false
	}
	return bobSameLoopback(u.Hostname(), "localhost") && u.Port() != ""
}

// bobProxyIsListening reports whether something is accepting connections on the
// host:port val names. Second, independent signal to the address comparison above.
//
// A dial, not a request: the question is "is this address live", and sending an
// HTTP request to someone else's proxy to find out would be a side effect status
// has no business causing. Short timeout because this runs in the path of a
// user-facing report — a firewalled address must not hang the command.
//
// Advisory ONLY, and never part of the ownership decision. Cortex being stopped is
// the normal state of a laptop, so "not listening" cannot be allowed to mean "not
// ours" — that would make disable refuse to clean up exactly when the user has
// already uninstalled the thing. It is reported, not acted on.
func bobProxyIsListening(val string) bool {
	u, err := url.Parse(strings.TrimSpace(val))
	if err != nil || u.Host == "" {
		return false
	}
	c, err := net.DialTimeout("tcp", u.Host, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// bobTrustNote is the trust-store step agentop does NOT take off macOS, printed for
// the user to run.
//
// Printed rather than executed on purpose: adding a root to the system store needs
// sudo, and a tool that escalates on its own to change machine-wide trust is not one
// anybody can audit after the fact.
//
// Not printed on macOS, where enable's launch environment does the job with no trust
// change at all — see cmd_bob_launch.go. Earlier versions printed a System-keychain
// command there; bobUntrustNote still prints its undo for anyone who ran it.
//
// caPath is ca.crt — the single bridge CA — and NOT bundle.crt: the trust store is
// ADDITIVE, so it wants one certificate, and bundle.crt holds every platform root as
// well. See core/tlsbridge/bundle.go's header, which states which file is for which
// job.
func bobTrustNote(caPath string) string {
	q := shellQuote(caPath)
	// Suggestive by design. The exact route differs per distribution and a
	// confidently wrong command is worse than a named one plus a caveat.
	return "Bob must also trust the bridge CA, or every https request fails verification.\n" +
		"  Add " + q + " to this system's trusted certificate store. The exact\n" +
		"  step depends on the distribution; the usual ones are:\n\n" +
		"    Debian/Ubuntu: copy it into /usr/local/share/ca-certificates/ (renamed to\n" +
		"                   end in .crt) and run sudo update-ca-certificates\n" +
		"    Fedora/RHEL:   copy it into /etc/pki/ca-trust/source/anchors/ and run\n" +
		"                   sudo update-ca-trust\n\n" +
		"  Some applications keep their own trust store and need it added there too.\n" +
		"  That is the single bridge CA, not the " + tlsbridge.TrustBundleName + " beside it, which\n" +
		"  additionally carries every platform root.\n\n"
}

// bobUntrustNote is the undo for bobTrustNote, and says plainly that it is
// optional: a CA left in the store signs nothing but Cortex's own forged leaves,
// and removing it is tidiness rather than a fix.
//
// On macOS it is the undo for what EARLIER versions of enable printed — this one makes
// no trust change there — so it is worded as conditional on having run that command.
func bobUntrustNote(caPath string, macOS bool) string {
	q := shellQuote(caPath)
	if macOS {
		// Two commands, because the add did two things and one undo does not cover
		// both. `add-trusted-cert -d` writes TRUST SETTINGS to the admin domain, and
		// `remove-trusted-cert -d` is its documented inverse — the -d has to be
		// repeated or it removes from the user domain, which the add never wrote to.
		// `delete-certificate -t` was wrong here on its own: it deletes the
		// certificate and, per its own usage text, "user trust settings", leaving the
		// admin-domain trust the add created in place. The keychain must be named on
		// the delete for the same reason -d is repeated on the remove: an add to the
		// System keychain is not undone by a delete that defaults to the login one.
		return "  The bridge CA, if an earlier agentop had you trust it in the System keychain,\n" +
			"  is safe to leave — it only validates certificates Cortex itself issues. To remove\n" +
			"  it:\n" +
			"    sudo security remove-trusted-cert -d " + q + "\n" +
			"    sudo security delete-certificate -c " + bobCACommonName + " \\\n" +
			"      -t " + bobSystemKeychain + "\n\n"
	}
	return "  The bridge CA, if you added it to this system's trust store, is safe to leave —\n" +
		"  it only validates certificates Cortex itself issues. To remove it, delete the\n" +
		"  copy you added (under /usr/local/share/ca-certificates/ or\n" +
		"  /etc/pki/ca-trust/source/anchors/) and re-run update-ca-certificates /\n" +
		"  update-ca-trust.\n\n"
}

// bobWanted is the best-effort form of wantedFromConfig, for the two verbs that must
// keep working when the config does not.
//
// Returns the proxy URL agentop would write and the CA path it would name, either of
// which may be "" when the config is missing, unreadable or has no forward proxy.
// enable does NOT use this — it needs a readable config and refuses without one.
// disable and status do: a user who has uninstalled Cortex and wants Bob working
// again must not find the off switch broken by the absence of the thing being
// switched off, and a status report is more useful than a parse error.
//
// The CA falls back to the default location because the trust-store note is only
// ever printed, so naming the usual path is better than naming none. The proxy does
// NOT fall back: it feeds the ownership decision, and a guessed address there would
// be a guess about which values are safe to delete.
func bobWanted(cortexCfgPath, home string) (proxy, caPath string) {
	caPath = filepath.Join(home, ".cortex", "ca", "ca.crt")
	want, _, err := wantedFromConfig(cortexCfgPath)
	if err != nil {
		return "", caPath
	}
	if want[envCACerts] != "" {
		caPath = want[envCACerts]
	}
	return want[envProxy], caPath
}

// bobSetKey writes one top-level key into a settings file, changing nothing else
// in it — byte for byte. It is the reason bob does not call writeSettings.
//
// writeSettings round-trips through json.MarshalIndent over a map[string]any, and
// that is lossy in ways JSON does not consider meaningful but a person reading a
// diff does: Go sorts map keys, so a hand-grouped file is alphabetized; indentation
// is normalized to two spaces; and an inline array like [80, 120] is exploded onto
// one line per element. On a real settings file, adding one key moved
// workbench.colorTheme from first to last and rewrote ten lines. These files are
// hand-curated and frequently committed to a dotfiles repo, so a semantically
// equal but textually large diff is a real cost — and it contradicted the
// "Nothing else in the file changes" line printed directly above the write.
//
// So the edit is textual and surgical, and the parser drives it rather than a
// regex: json.Decoder reports byte offsets, which is what makes it safe to splice
// a document this way. A key whose name appears inside some other string value
// cannot be mistaken for the real member, because the offsets come from the
// tokenizer, not from a search.
//
// value == nil means delete the key. Returns the new file content.
func bobSetKey(src []byte, key string, value any) ([]byte, error) {
	start, end, found, err := bobFindMember(src, key)
	if err != nil {
		return nil, err
	}

	if value == nil {
		if !found {
			return src, nil
		}
		return bobSpliceOut(src, start, end), nil
	}

	// Encoded the same way either way, so a replaced value and an inserted one are
	// formatted identically.
	vb, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	member := append([]byte(strconv.Quote(key)+": "), vb...)

	if found {
		out := make([]byte, 0, len(src)+len(member))
		out = append(out, src[:start]...)
		out = append(out, member...)
		out = append(out, src[end:]...)
		return out, nil
	}
	return bobInsertMember(src, member)
}

// errBobDuplicateKey marks the one bobFindMember failure its callers act on: the same
// top-level key present twice. Every other failure is a tokenizer error that readSettings
// describes better. A sentinel rather than a substring of the message, so rewording the
// sentence cannot quietly disable the gate in bobDuplicateKey.
var errBobDuplicateKey = errors.New("duplicate top-level key")

// bobFindMember locates the byte span of a top-level member, from the opening quote
// of its name through the last byte of its value. Offsets come from json.Decoder,
// so they are the tokenizer's view of the document and not a textual guess.
//
// A document may legally name the same key twice, and both Go and VS Code take the
// LAST one. Splicing out a single span cannot express "remove both", and removing
// either one alone leaves the key still set while every message this command prints
// says it is gone — so this refuses instead, by name. The whole document is scanned
// rather than returning at the first match, which is what makes the second occurrence
// visible at all.
func bobFindMember(src []byte, key string) (start, end int, found bool, err error) {
	dec := json.NewDecoder(bytes.NewReader(src))
	tok, err := dec.Token()
	if err != nil {
		return 0, 0, false, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return 0, 0, false, fmt.Errorf("top level is not a JSON object")
	}
	for dec.More() {
		// InputOffset before reading the name is the offset just past the previous
		// token, so skip whitespace forward to the quote that opens this name.
		nameStart := int(dec.InputOffset())
		for nameStart < len(src) && src[nameStart] != '"' {
			nameStart++
		}
		name, err := dec.Token()
		if err != nil {
			return 0, 0, false, err
		}
		// Reading the value advances the offset to just past it, which is the end of
		// the whole member. Decoding into json.RawMessage consumes a value of any
		// shape — object, array, scalar — in one step.
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return 0, 0, false, err
		}
		if name == key {
			if found {
				// Wrapped, not just worded: bobDuplicateKey classifies this verdict and
				// used to do it by searching the message text, so rewording the sentence
				// below silently turned that gate off. errors.Is cannot drift that way.
				return 0, 0, false, fmt.Errorf("%w: %q appears more than once at the top "+
					"level; remove the duplicate first — this command edits one member and "+
					"cannot say which of them wins", errBobDuplicateKey, key)
			}
			start, end, found = nameStart, int(dec.InputOffset()), true
		}
	}
	return start, end, found, nil
}

// bobTailIsOnlySeparator reports whether what follows a member on its own line is
// nothing but a comma and whitespace — i.e. no other key shares the line to the right.
//
// Separate from a plain TrimSpace check because the member's own trailing comma is
// part of the separator and not a neighbour.
func bobTailIsOnlySeparator(tail []byte) bool {
	t := bytes.TrimSpace(tail)
	if len(t) > 0 && t[0] == ',' {
		t = bytes.TrimSpace(t[1:])
	}
	return len(t) == 0
}

// bobSpliceOut removes a member and exactly one of the commas around it, leaving the
// surrounding layout intact.
//
// The subtlety is which whitespace goes with the member. Taking only the member's own
// bytes leaves its indentation behind as a line of trailing spaces — invisible in a
// terminal, visible in a diff and to every whitespace linter. So the span removed runs
// from the start of the member's own LINE (its leading indentation) to just past the
// newline that ends it. A blank line the user wrote as a grouping separator sits before
// that indentation and is therefore kept.
//
// The comma is the other half: it goes on whichever side has one, preferring the
// FOLLOWING comma so that removing the last member does not leave a trailing comma,
// which is invalid JSON.
func bobSpliceOut(src []byte, start, end int) []byte {
	// Is the member alone on its line? Only then can the line be taken whole — and
	// "alone" means on BOTH sides. A member that is first on its line but shares it
	// with a later key still may not have its line's leading indentation removed:
	// that indentation belongs to the key that survives. Checking only the left side
	// left `{\n    "http.proxy": ..., "b": 2,\n` as `{\n "b": 2,` — the indent gone
	// and one stray space in its place.
	lineStart := bytes.LastIndexByte(src[:start], '\n') + 1
	// Scanned from end, the end of the VALUE, not from start, the start of the key. A
	// member may straddle a line break — `"http.proxy":\n    "http://..."` is valid
	// JSON and VS Code's own formatter produces it on a long value — and then the first
	// newline after the key is INSIDE the member. Measuring from start there yields a
	// lineEnd below end, and the src[end:lineEnd] slice below panicked on a file that
	// was never malformed: `slice bounds out of range [79:46]`, from `configure bob
	// disable`, after it had already printed that it was keeping a .bak.
	lineEnd := bytes.IndexByte(src[end:], '\n')
	if lineEnd < 0 {
		lineEnd = len(src)
	} else {
		lineEnd += end
	}
	ownLine := len(bytes.TrimSpace(src[lineStart:start])) == 0 &&
		bobTailIsOnlySeparator(src[end:lineEnd])

	// Walk forward past whitespace to a following comma, if there is one.
	after := end
	for after < len(src) && isBobSpace(src[after]) {
		after++
	}

	from, to := start, end
	if after < len(src) && src[after] == ',' {
		// Not the last member: take the member, the comma, and — only when the member
		// had the line to itself — the rest of that line including the newline, so the
		// next member keeps its own indentation.
		//
		// The ownLine guard is load-bearing. A member SHARING a line with another key
		// still has a blank line-tail after its comma when the next key is on the line
		// below, so without the guard this arm consumed that newline and the following
		// line's indentation too, welding two lines together:
		//
		//   {"a": 1,\n    "b": 2, "http.proxy": "...",\n    "c": 3}
		//     became   ...\n    "b": 2,     "c": 3\n...
		//
		// which is exactly the layout change "Nothing else in the file changes" says
		// this function does not make. When the member shares its line, only its own
		// bytes and its comma may go.
		to = after + 1
		if nl := bytes.IndexByte(src[to:], '\n'); ownLine && nl >= 0 && len(bytes.TrimSpace(src[to:to+nl])) == 0 {
			to += nl + 1
		} else if !ownLine {
			// The member shares its line — either a single-line document,
			// `{"a": 1, "http.proxy": "...", "b": 2}`, or one line of a multi-line one.
			// Its own line cannot be taken, and the space that separated this member
			// from what follows would be left beside the space after the previous
			// comma, making a double space. Take one of them.
			for to < len(src) && src[to] == ' ' {
				to++
			}
			// Nothing but the line's end follows: the space just taken was the only
			// thing between the comma and the newline, so the comma we absorbed leaves
			// a trailing space behind instead. Walk BACK over the spaces before the
			// member so the surviving key ends at its own comma. Trailing whitespace is
			// invisible in a terminal and loud in a diff, and it is what
			// TestBobDisable_LeavesNoStrayWhitespace forbids.
			if to < len(src) && src[to] == '\n' {
				for from > 0 && src[from-1] == ' ' {
					from--
				}
			}
		}
		if ownLine {
			from = lineStart
			// A member sitting BETWEEN two separators leaves both behind once it is
			// gone, and two blank lines where the user wrote one is a change to the
			// file's layout — the thing this function exists not to make. So when the
			// bytes on each side of the removed line are both separators of the same
			// kind, one of them goes with it. Same reasoning one line down for the
			// single-line-document form, where the separator is a space rather than a
			// newline and removing a middle member would leave a double space.
			from = bobAbsorbSeparator(src, from, to)
		}
	} else {
		// Last member: take the preceding comma and the whitespace between it and us,
		// so the member that becomes last does not end with a dangling comma.
		for from > 0 && isBobSpace(src[from-1]) {
			from--
		}
		if from > 0 && src[from-1] == ',' {
			from--
		}
		// Anything the user left between the value and the line's end goes too. It is
		// the member's own tail, but walking back to the preceding comma moves the cut
		// ABOVE it, so leaving it behind strands it on the line that survives:
		// `"a": 1,\n    "http.proxy": "..." \n}` became `"a": 1 \n}` — a trailing space
		// on a line the user did not touch. Only spaces and tabs, and only as far as
		// the newline: a following blank line is the user's own grouping, and the
		// ownLine arm above already owns the newline case.
		for to < len(src) && (src[to] == ' ' || src[to] == '\t') {
			to++
		}
	}

	out := make([]byte, 0, len(src)-(to-from))
	out = append(out, src[:from]...)
	out = append(out, src[to:]...)
	return out
}

// bobInsertMember adds a member after the last existing one, matching that member's
// own indentation so the insertion looks hand-written rather than appended.
func bobInsertMember(src []byte, member []byte) ([]byte, error) {
	// The closing brace of the top-level object is the last '}' in the document.
	closing := bytes.LastIndexByte(src, '}')
	if closing < 0 {
		return nil, fmt.Errorf("top level is not a JSON object")
	}

	// Is the object empty? Then there is no last member to follow.
	trimmed := bytes.TrimSpace(src[:closing])
	empty := bytes.HasSuffix(trimmed, []byte("{"))

	// End of the last member, which is where the comma and the new line go.
	insertAt := closing
	for insertAt > 0 && isBobSpace(src[insertAt-1]) {
		insertAt--
	}

	// The indentation to use: the LAST MEMBER's own, not the closing brace's. Copying
	// the brace's line is the obvious-looking choice and it is wrong — in a
	// conventionally formatted file that line is column 0, so every inserted key
	// landed unindented while every existing one was indented. Read it from where the
	// last member starts instead, which is the line the new member will sit beside.
	indent := bobIndentOf(src, insertAt)
	if indent == "" {
		// Either an empty object, or a single-line document with no indentation to
		// copy. Two spaces is the only width this function ever invents, and only
		// when the file itself shows none.
		indent = "  "
	}

	var ins []byte
	if empty {
		ins = append(ins, '\n')
		ins = append(ins, indent...)
		ins = append(ins, member...)
		ins = append(ins, '\n')
	} else {
		ins = append(ins, ',', '\n')
		ins = append(ins, indent...)
		ins = append(ins, member...)
	}

	out := make([]byte, 0, len(src)+len(ins))
	out = append(out, src[:insertAt]...)
	out = append(out, ins...)
	out = append(out, src[insertAt:]...)
	return out, nil
}

// bobIndentOf returns the leading whitespace of the line containing off.
func bobIndentOf(src []byte, off int) string {
	lineStart := bytes.LastIndexByte(src[:off], '\n') + 1
	i := lineStart
	for i < off && (src[i] == ' ' || src[i] == '\t') {
		i++
	}
	return string(src[lineStart:i])
}

// bobAbsorbSeparator extends a removal backward over one blank line when the removed
// span has a blank line on both sides, so a grouping separator is not duplicated.
func bobAbsorbSeparator(src []byte, from, to int) int {
	// Is what follows the removed span a blank line?
	nl := bytes.IndexByte(src[to:], '\n')
	if nl < 0 || len(bytes.TrimSpace(src[to:to+nl])) != 0 {
		return from
	}
	// Is what precedes it also one? The from == 0 test comes first because the walk
	// below slices src[:from-1], which panics on from == 0 rather than reporting it.
	// Unreachable today — every caller passes a from inside an object, so at least the
	// opening brace precedes it — which is exactly why the order is worth fixing now
	// rather than after a new caller makes it reachable.
	if from == 0 || src[from-1] != '\n' {
		return from
	}
	// Walk back over the previous line.
	prevStart := bytes.LastIndexByte(src[:from-1], '\n') + 1
	if len(bytes.TrimSpace(src[prevStart:from-1])) != 0 {
		return from
	}
	return prevStart
}

func isBobSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// bobEdit is one key bobWriteKeys sets, or deletes when value is nil.
type bobEdit struct {
	key   string
	value any
}

// bobWriteKey is bobWriteKeys for a single key.
func bobWriteKey(path, key string, value any) error {
	return bobWriteKeys(path, []bobEdit{{key, value}})
}

// bobWriteKeys is bob's replacement for writeSettings: same backup and atomic-rename
// behaviour, but the content is the original file with the given keys edited rather
// than a re-marshal of the whole document.
//
// Every edit lands in ONE write. enable sets http.proxy and http.systemCertificates
// together, and two writes would leave a window — or, on a failed second write, a
// file — holding one without the other.
func bobWriteKeys(path string, edits []bobEdit) error {
	src, rerr := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if rerr != nil {
		if !os.IsNotExist(rerr) {
			return rerr
		}
		// No file yet: start from an empty object so the insert path has a document
		// to work on. Nothing to back up either. This is a first enable on a Bob that
		// has never saved a setting — bobNoDocument has already checked that Bob's
		// User directory is there.
		src = []byte("{}\n")
	} else {
		// Back the file up ONCE and never overwrite it, matching writeSettings: a
		// second enable, or an enable/disable pair, must not replace the pristine
		// pre-Cortex file with one agentop already edited.
		if berr := backupOnce(path, src); berr != nil {
			return berr
		}
	}

	// An empty or whitespace-only file is an empty document, the same reading
	// readSettings gives it.
	if len(bytes.TrimSpace(src)) == 0 {
		src = []byte("{}\n")
	}

	out := src
	for _, e := range edits {
		var err error
		if out, err = bobSetKey(out, e.key, e.value); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}

	// Never hand back something that is not valid JSON, whatever the splicing did.
	// Bob reads this file; a malformed one is worse than a reformatted one.
	var check map[string]any
	if jerr := json.Unmarshal(out, &check); jerr != nil {
		return fmt.Errorf("%s: edit would produce invalid JSON (%w); file left unchanged", path, jerr)
	}

	// Atomic and symlink-resolving; see writeSettings for why the fixed ".tmp"
	// name had to go (#1565).
	// 0600: this file commonly holds API tokens in the same block.
	return writeFileAtomic(path, out, 0o600)
}

// bobBackupNote describes what this particular write will and will not preserve, in
// one line or none.
//
// bobWriteKey makes three different choices and the message must not claim more
// than the first: it writes <path>.bak from the file's current contents ONLY when
// the file exists AND no .bak is there already — never overwriting, because a
// second run would otherwise replace the pristine pre-Cortex file with one agentop
// had already edited. So "a copy is kept" is false on a settings file that does not
// exist yet (there is nothing to copy — and nothing to say: the preview already
// shows the path enable creates), and when a .bak survives from an earlier run the
// copy kept is that older one, not this run's, which is the one line that case
// gets. Promising a backup that is not made is worse than promising none — it is
// the sentence a user leans on before saying yes.
//
// The "nothing else changes" half of the old prose moved to the merged preview's
// own closing line; what is left here is only the backup claim.
//
// home is for display only — the paths the note names are abbreviated the way
// every other line of the report is — while settingsPath is the REAL path and
// the one that gets statted. Handing this function the abbreviated form instead
// made the stat fail on every default layout, where settings.json lives under
// $HOME: no promise on the first write, and no warning when an earlier run's
// .bak is the copy that survives.
func bobBackupNote(home, settingsPath string) string {
	// The splice claim is a literal one: bobWriteKey splices a single member in
	// or out of the existing bytes, so key order, indentation, inline arrays and
	// blank-line grouping all survive. Pinned byte-for-byte by
	// TestBobEnable_ChangesOneLineAndNoOtherByte.
	//
	// The same sentence appears in claude-code's enable, where it still goes through the
	// shared writeSettings and therefore still overstates what happens. Not changed here:
	// that is a different command's behaviour and out of this change's scope.
	bakReal, bakShow := settingsPath+".bak", tildePath(home, settingsPath+".bak")
	if _, err := os.Stat(settingsPath); err != nil {
		// Covers a missing file and an unreadable one alike: in both cases this run
		// will not produce a .bak, which is the only thing being claimed. Reached by
		// enable on a Bob that has never saved a setting.
		return ""
	}
	if _, err := os.Stat(bakReal); err == nil {
		return "Note: " + bakShow + " already exists and holds the file as first found, not as it is now.\n"
	}
	return "A copy of the original is kept as " + bakShow + ".\n"
}

// bobDuplicateKey reports whether the settings file names either key this command
// writes more than once at the top level, which readSettings cannot show: it decodes
// into a map, and a map keeps only the last of two same-named members.
//
// Called before anything is printed or prompted. bobWriteKey refuses the same file, so
// leaving this out still fails safe — but it fails AFTER asking the user to approve a
// write that then cannot happen, and after status has already reported one of the two
// values as though it were the setting.
func bobDuplicateKey(settingsPath string) error {
	src, err := os.ReadFile(settingsPath)
	if err != nil {
		// Unreadable or absent is not this check's business: the caller's own
		// readSettings reports it, with its own wording.
		return nil
	}
	if len(bytes.TrimSpace(src)) == 0 {
		return nil
	}
	for _, key := range []string{bobProxyKey, bobSysCertsKey} {
		_, _, _, ferr := bobFindMember(src, key)
		// Only the duplicate verdict is this check's to report. Any other parse failure
		// is readSettings' to describe, and bob's readSettings arm says more about it
		// than a tokenizer error would.
		if ferr != nil && errors.Is(ferr, errBobDuplicateKey) {
			return ferr
		}
	}
	return nil
}

// bobNoDocument reports why the settings file cannot be treated as IBM Bob's, or nil.
//
// A MISSING settings.json is not that reason by itself. Bob writes the file only the
// first time a setting is changed, so a Bob that has run for months with every default
// has none — and enable refusing it told exactly that user that Bob "has not saved
// settings here". What says whether Bob has run is the directory the file goes in:
// Bob creates its User directory on first launch, whatever the settings. So a missing
// file inside an existing directory is an empty document (enable creates the file),
// and a missing directory is the --settings typo or the wrong machine this check was
// written for. An empty file reads the same way: the directory is there by definition.
//
// `null` is still refused: it is a document, and not an object, so bobSetKey cannot
// edit it — and the write path would only discover that after enable had printed what
// it will set, prompted, and copied the file to .bak. This runs before anything is
// printed for that reason.
//
// It is deliberately NOT fixed inside readSettings, which claude-code shares: coercing
// null to an empty document is the right reading for a command that re-marshals the whole
// file, and changing it there would change that command's behaviour.
//
// An unreadable file is not this check's business — readSettings reports it, with its own
// wording, and "exists but cannot be read" is a different problem from "is not there".
func bobNoDocument(settingsPath string) error {
	src, err := os.ReadFile(settingsPath) //nolint:gosec // operator-supplied path
	if err != nil {
		if os.IsNotExist(err) {
			dir := filepath.Dir(settingsPath)
			if st, serr := os.Stat(dir); serr == nil && st.IsDir() {
				return nil
			}
			return fmt.Errorf("%s does not exist, so IBM Bob has not run here", dir)
		}
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(src), []byte("null")) {
		return fmt.Errorf("%s holds only `null`, which sets nothing", settingsPath)
	}
	return nil
}

// bobSettingsExist reports whether the settings file is there at all, for the lines
// that say so. bobNoDocument has already decided a missing one is usable.
func bobSettingsExist(settingsPath string) bool {
	_, err := os.Stat(settingsPath)
	return err == nil
}

// bobNotInstalled is enable's refusal when there is no Bob to configure.
func bobNotInstalled(reason error, stderr io.Writer) int {
	fmt.Fprintf(stderr, "agentop: %v.\n"+
		"  Nothing to enable. Start IBM Bob once so it creates its settings directory,\n"+
		"  or pass --settings with the path to a settings.json it does use.\n", reason)
	return 1
}

// bobEnablePlan is the editor half of enable: everything it decided, computed
// whole BEFORE anything is written, so the merged command can show both halves'
// changes under one confirmation.
//
// The errors that used to exit 1 in front of the old per-half prompt still do —
// they print to stderr and come back as a nil plan with code 1 — because every
// one of them is a reason no settings write can honestly be proposed.
type bobEnablePlan struct {
	// edits are the settings.json splices: "http.proxy", and on macOS
	// "http.systemCertificates".
	edits []bobEdit
	// recordSysCerts writes the ownership record for the systemCertificates
	// edit, so a later disable removes only a value agentop wrote.
	recordSysCerts bool
	// addBlock appends the CA-trust block to the rc file (macOS only).
	addBlock bool
	// rcContent is the rc file as read, to append the block to.
	rcContent string
	// block is the CA-trust block itself, built from the config's CA path. Set
	// even on the already-enabled arm: the by-hand advice prints it.
	block string
	// caPath is the CA the block names — what the probe checks against.
	caPath string
	// blockNote is a finding about the rc file, printed with the preview: a
	// cortex bob block agentop does not recognise, left alone.
	blockNote string
	// caMissing: the CA file does not exist yet. Cortex creates it on first
	// start; until then, Bob's https through Cortex fails verification.
	caMissing bool
	// already: the editor half's requested state holds — nothing of its to
	// change.
	already bool
}

// planBobEnable computes the editor half's changes, printing its own refusals to
// stderr. Returns a nil plan with code 1 when it will not propose a write, and
// code 0 with already set when there is nothing of its to change.
func planBobEnable(settingsPath, cortexCfgPath string, launch bobLaunch, stderr io.Writer) (*bobEnablePlan, int) {
	want, cfg, err := wantedFromConfig(cortexCfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentop: %v\n", err)
		return nil, 1
	}
	// Same gate as claude-code and exec, and for the same reason: with the bridge
	// off Cortex terminates no TLS, so pointing Bob at the proxy would send every
	// https request into something that cannot answer it.
	if !bridgeEnabled(cfg) {
		fmt.Fprintf(stderr, "agentop: %v\n", errBridgeDisabled(cortexCfgPath))
		return nil, 1
	}
	caPath := want[envCACerts]
	if caPath == "" {
		fmt.Fprintf(stderr, "agentop: %s has no tls_bridge.ca_dir, so there is no CA for IBM Bob to\n"+
			"  trust; every https request would fail certificate verification. Enable the\n"+
			"  TLS bridge first.\n", cortexCfgPath)
		return nil, 1
	}
	proxy := want[envProxy]

	if nerr := bobNoDocument(settingsPath); nerr != nil {
		return nil, bobNotInstalled(nerr, stderr)
	}
	// The rc file the CA-trust block goes into: exit 1 when it cannot even be
	// looked at, because nothing below can say whether the block is already there.
	if launch.macOS && launch.rcReadErr != nil {
		fmt.Fprintf(stderr, "agentop: %v\n", launch.rcReadErr)
		return nil, 1
	}
	if derr := bobDuplicateKey(settingsPath); derr != nil {
		fmt.Fprintf(stderr, "agentop: %s: %v\n", settingsPath, derr)
		return nil, 1
	}
	doc, err := readSettings(settingsPath)
	if err != nil {
		// readSettings names the file and says to fix or move it. The clause about
		// comments is bob's own: Bob is a VS Code fork, VS Code permits comments in
		// settings.json and writes them back, so a commented file is the likeliest
		// way to arrive here — and the generic "not valid JSON" does not hint at it.
		fmt.Fprintf(stderr, "agentop: %v.\n"+
			"  If Bob has saved comments in it, that is why: VS Code allows them and this\n"+
			"  command reads strict JSON. Remove them, or point --settings elsewhere —\n"+
			"  agentop will not rewrite the file to strip them.\n", err)
		return nil, 1
	}

	setProxy := true
	switch existing := doc[bobProxyKey].(type) {
	case nil:
		// Not set. Nothing to weigh.
	case string:
		if existing == proxy {
			setProxy = false
			break
		}
		// bobNotOurs, not "!= bobOurs": enable reaches here only with a readable
		// config (it exits 1 above otherwise), so bobUnknown cannot occur — but if
		// that ever changes, a value agentop cannot judge should fall through to the
		// write it is about to describe and prompt for, not be refused as foreign.
		if bobOwns(existing, want[envProxy]) == bobNotOurs {
			// Refuse rather than overwrite: the overwhelmingly likely owner of a
			// foreign value is a corporate proxy the user needs, and this command
			// keeps no record that could restore it.
			fmt.Fprintf(stderr, "agentop: %s already sets %q to %q, which is not a Cortex proxy.\n"+
				"  Leaving it alone — remove or change it yourself if you want Cortex there\n"+
				"  instead.\n", settingsPath, bobProxyKey, existing)
			return nil, 1
		}
		// Reaching here means bobOwns said bobOurs while the value differs from the
		// one about to be written — which, now that ownership is an exact host+port
		// match, only spellings of the same listener can do: "localhost" against
		// "127.0.0.1", or a differing case. Falls through to the write, which
		// normalizes it to whatever the config derives. The plan's "ours but stale,
		// the port moved" case no longer lands here: a differing port is not ours by
		// that definition, so it is refused as foreign above. That is the safe
		// direction — enable never overwrites a value it cannot positively claim —
		// but it does mean a user whose forward_proxy_addr moved must remove the old
		// value by hand. Noted rather than changed: widening ownership back to a port
		// range is what the previous review had this command stop doing.
	default:
		// A non-string (a number, an object) is not something this command wrote and
		// not something it can compare. Same refusal as a foreign string.
		fmt.Fprintf(stderr, "agentop: %s sets %q to a non-string value (%T).\n"+
			"  Leaving it alone — fix it yourself if you want Cortex there instead.\n",
			settingsPath, bobProxyKey, existing)
		return nil, 1
	}

	// The launch environment, on macOS only — see cmd_bob_launch.go for why both the
	// settings key and the shell block are needed, and why neither applies elsewhere.
	block := bobLaunchBlock(caPath)
	var setSysCerts, addBlock bool
	var rcContent, blockNote string
	if launch.macOS {
		if setSysCerts, err = bobSysCertsWanted(doc); err != nil {
			fmt.Fprintf(stderr, "agentop: %s: %v.\n", settingsPath, err)
			return nil, 1
		}
		if launch.rcPath != "" {
			b, rerr := os.ReadFile(launch.rcPath)
			if rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
				fmt.Fprintf(stderr, "agentop: read %s: %v\n", launch.rcPath, rerr)
				return nil, 1
			}
			rcContent = string(b)
			switch copies, edited := bobRCBlockState(rcContent, block); {
			case copies > 0:
				// Already there.
			case edited:
				// bobshell's rule for its own block: appending a second copy beside a
				// hand-edited one leaves two competing definitions, which is worse
				// than declining. The probe after the write says whether the edited
				// one works.
				blockNote = fmt.Sprintf("%s already has a cortex bob block, but not one matching what\n"+
					"  this agentop writes. Left alone — remove it by hand and re-run enable to\n"+
					"  replace it.\n", launch.rcPath)
			default:
				addBlock = true
			}
		}
	}

	p := &bobEnablePlan{
		block:  block,
		caPath: caPath,
	}
	if !setProxy && !setSysCerts && !addBlock {
		p.already = true
		return p, 0
	}

	// Enabling before Cortex's first start is legitimate — the proxy generates the
	// CA on boot — but nothing can trust a file that is not there yet, so the plan
	// says so now rather than let it be discovered as failed requests.
	if _, serr := os.Stat(caPath); serr != nil {
		p.caMissing = true
	}
	if setProxy {
		p.edits = append(p.edits, bobEdit{bobProxyKey, proxy})
	}
	if setSysCerts {
		p.edits = append(p.edits, bobEdit{bobSysCertsKey, false})
		p.recordSysCerts = true
	}
	p.addBlock, p.rcContent, p.blockNote = addBlock, rcContent, blockNote
	return p, 0
}

// enable routes both faces of Bob through Cortex: the editor's settings.json and
// CA-trust block, and the shell's "bob" function. Both halves' changes are
// computed first and confirmed ONCE; the writes then happen independently, and a
// failure in one half does not stop the other.
func (c bobCommand) enable() int {
	// --- The plans: everything both halves would change, before anything is ---
	var ide *bobEnablePlan
	// 1 when the editor half is skipped, like bobNotInstalled: the half's change
	// did not happen, and bobCombine must not let the shell half's success read
	// as the whole command's. runBob has already said why on stderr.
	ideCode := 0
	if c.ideSkip {
		ideCode = 1
	} else {
		ide, ideCode = planBobEnable(c.settingsPath, c.cortexCfgPath, c.launch, c.stderr)
	}
	var shell rcPlan
	shellCode := 0
	if c.shellAdvise == nil {
		p, err := bobShellEnablePlan(c.shellRC)
		if err != nil {
			// The same unreadable rc file the editor half's block would ride on:
			// nothing can say whether either block is already there.
			fmt.Fprintf(c.stderr, "agentop: read %s: %v\n", c.shellRC, err)
			shellCode = 1
		} else {
			shell = p
		}
	}

	ideChanges := ide != nil && !ide.already
	shellChanges := c.shellAdvise == nil && shellCode == 0 && shell.write
	if !ideChanges && !shellChanges {
		return c.enableNothing(ide, shell, ideCode, shellCode)
	}

	// --- The preview: one header, one section per file, one line per change ---
	var lines []string
	count := 0
	if ideChanges && len(ide.edits) > 0 {
		lines = append(lines, "  "+tildePath(c.home, c.settingsPath))
		for _, e := range ide.edits {
			if e.key == bobProxyKey {
				lines = append(lines, fmt.Sprintf("    %q: %q", bobProxyKey, e.value))
			} else {
				lines = append(lines, fmt.Sprintf("    %q: false", bobSysCertsKey))
			}
		}
		count += len(ide.edits)
	}
	var rcLines []string
	if ideChanges && ide.addBlock {
		rcLines = append(rcLines, "    CA-trust block — exports "+envCACerts+" so IBM Bob trusts Cortex's CA")
		count++
	}
	if shellChanges {
		rcLines = append(rcLines, `    bob() — typing "bob" runs it through Cortex`)
		count++
	}
	if len(rcLines) > 0 {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, "  "+tildePath(c.home, c.shellRC))
		lines = append(lines, rcLines...)
	}
	plural := "s"
	if count == 1 {
		plural = ""
	}
	fmt.Fprintf(c.stdout, "Route IBM Bob through Cortex — %d change%s:\n\n", count, plural)
	for _, l := range lines {
		fmt.Fprintln(c.stdout, l)
	}

	// --- Findings the decision deserves to see, then the one prompt ---
	fmt.Fprintln(c.stdout)
	if ideChanges && ide.caMissing {
		fmt.Fprintf(c.stdout, "Note: %s does not exist yet — Cortex creates it on first start, and\n"+
			"  until then Bob's https requests through Cortex fail verification.\n", tildePath(c.home, ide.caPath))
	}
	if ideChanges && ide.blockNote != "" {
		fmt.Fprint(c.stdout, ide.blockNote)
	}
	// Not gated on shellChanges: a finding is set exactly when the half will NOT
	// write, which is when shellChanges is false. With editor-half changes
	// pending, this is the only line that says the rc file was left alone.
	// leftAlone, not note: a plain absence is not a finding.
	if shell.leftAlone() {
		fmt.Fprintln(c.stdout, shell.note)
	}
	if ideChanges && len(ide.edits) > 0 {
		fmt.Fprint(c.stdout, bobBackupNote(c.home, c.settingsPath))
	}
	fmt.Fprintln(c.stdout, "Nothing else in the files changes.")
	if !c.yes && !bobConfirm(c.stdout) {
		// exitDeclined, and nothing was written: one consent covers both halves,
		// so refusing it declines both. A script branching on 3 reads "the user
		// said no", never "half done".
		fmt.Fprintln(c.stdout, "Not changed. Re-run with --yes to apply without asking.")
		return exitDeclined
	}

	// --- Apply: the halves write independently; a failure in one does not stop
	// the other, and the exit code is the worse of the two. ---
	ideTotal := ideCode
	if ideChanges {
		ideTotal = c.applyEnableEditor(ide)
	}
	shellTotal := shellCode
	if shellChanges {
		shellTotal = shell.apply(c.stderr)
	}

	// --- The report ---
	c.enableReport(ide, shell, ideChanges, shellChanges, ideTotal, shellTotal)
	return bobCombine(ideTotal, shellTotal)
}

// shellAdviseOut prints the by-hand advice for an rc file agentop will not
// write: the reason first, then — enable only — the block to place. Disable
// prints just the markers to search for; a user removing the integration has the
// text already, and printing it again reads as an instruction to add it back.
// block is the CA-trust block, when the editor half's plan is at hand to name it.
func (c bobCommand) shellAdviseOut(verb, block string) {
	fmt.Fprintf(c.stdout, "agentop: %v\n\n", c.shellAdvise)
	if verb == "enable" {
		if c.launch.macOS && block != "" {
			bobManualBlock(c.shellAdvise, block, c.stdout)
		}
		bobShellAdviseManual("enable", "whichever file your shell reads at startup", c.stdout)
		return
	}
	bobShellAdviseManual("disable", "the real file", c.stdout)
}

// ideBlock is the plan's CA-trust block, or "" when there is no plan to name one.
func ideBlock(ide *bobEnablePlan) string {
	if ide == nil {
		return ""
	}
	return ide.block
}

// enableNothing is enable with nothing to write in either half. What the user
// gets depends on why: both halves confirmed already correct answers the
// question a re-run is usually asking — is it actually working? — so the probe
// reports in full; otherwise the half that could not act has said so on stderr,
// and the shell half's own findings still belong on stdout.
func (c bobCommand) enableNothing(ide *bobEnablePlan, shell rcPlan, ideCode, shellCode int) int {
	if ide != nil && ide.already {
		fmt.Fprintln(c.stdout, "Already enabled — IBM Bob routes through Cortex.")
		if ide.blockNote != "" {
			fmt.Fprint(c.stdout, ide.blockNote)
		}
		// Probed here too: a re-run of enable is how someone asks whether it is
		// working, and the block can be in place and still not be read.
		c.reportProbe(ide.caPath, true)
	}
	if c.shellAdvise != nil {
		c.shellAdviseOut("enable", ideBlock(ide))
	} else if shell.note != "" {
		fmt.Fprintln(c.stdout, shell.note)
	} else if shell.already && ide == nil {
		// Only the shell half was examined — the editor half was skipped with a
		// note on stderr — and it found its work already done.
		fmt.Fprintf(c.stdout, "The \"bob\" function is already in %s.\n", c.shellRC)
	}
	return bobCombine(ideCode, shellCode)
}

// applyEnableEditor performs the editor half's writes, in the order that keeps
// every intermediate state safe. The block goes first: with the settings written
// and the block not, Bob's extensions would stop trusting the keychain and have
// no Cortex CA instead; the other way round, the block only exports a variable
// Bob does not yet use. The record goes before the key it describes: a record
// for a key that never got written makes disable find nothing to remove, while
// a key with no record would be left behind by disable as the user's.
func (c bobCommand) applyEnableEditor(p *bobEnablePlan) int {
	if p.addBlock {
		// Re-read rather than write the bytes the plan read: the shell half's plan
		// read this same file before anything was written, and two whole-file
		// writes from those pre-prompt reads would each drop the other's block.
		// The transform is an append, so a fresh read keeps both halves' changes.
		content, rerr := os.ReadFile(c.launch.rcPath)
		if rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			fmt.Fprintf(c.stderr, "agentop: read %s: %v\n", c.launch.rcPath, rerr)
			return 1
		}
		if werr := writeRCFile(c.launch.rcPath, string(content)+p.block); werr != nil {
			fmt.Fprintf(c.stderr, "agentop: %v\n", werr)
			return 1
		}
	}
	if p.recordSysCerts {
		if werr := writeState(c.launch.statePath, bobSysCertsRecord(c.settingsPath)); werr != nil {
			fmt.Fprintf(c.stderr, "agentop: recording that agentop sets %q: %v\n", bobSysCertsKey, werr)
			return 1
		}
	}
	if len(p.edits) > 0 {
		if werr := bobWriteKeys(c.settingsPath, p.edits); werr != nil {
			fmt.Fprintf(c.stderr, "agentop: %v\n", werr)
			return 1
		}
	}
	return 0
}

// enableReport is the merged post-apply report: a headline that claims only what
// landed, then one line of next-step advice per face that changed, then the
// probe's findings and any by-hand advice.
func (c bobCommand) enableReport(ide *bobEnablePlan, shell rcPlan, ideChanges, shellChanges bool, ideTotal, shellTotal int) {
	fmt.Fprintln(c.stdout)
	switch {
	case ideTotal == 0 && shellTotal == 0 && !shell.leftAlone():
		fmt.Fprintln(c.stdout, "Enabled.")
	case ideTotal == 0 && ideChanges:
		// The shell half failed, was skipped, or was left alone with a finding
		// printed above; in every one of those it did not act, and the headline
		// claims only the half that did.
		fmt.Fprintln(c.stdout, "Enabled the editor half.")
	case shellTotal == 0 && shellChanges:
		fmt.Fprintln(c.stdout, "Enabled the shell half.")
	default:
		fmt.Fprintln(c.stdout, "Not changed — the errors above say why.")
		return
	}
	if c.launch.macOS && ideChanges && ideTotal == 0 {
		// The environment is read once, when Bob starts, and only on a start that
		// does not come from a terminal — VS Code skips the shell when it can
		// inherit one.
		fmt.Fprint(c.stdout, "  Quit IBM Bob and reopen it from the Dock or Spotlight so it picks this up;\n"+
			"  a Bob started from a terminal (bobide) takes that terminal's environment\n"+
			"  instead and does not run the block.\n")
	} else if !c.launch.macOS && ideChanges && ideTotal == 0 {
		// Restart, rather than a claim either way about live pickup: bobWriteKey is
		// temp+rename so Bob never sees a half-written file, but whether Bob re-reads
		// a proxy change without restarting is not something this command has
		// verified.
		fmt.Fprint(c.stdout, "  Restart Bob so it re-reads its settings.\n")
		// Off macOS the CA must be trusted by hand, in the system store, and that
		// needs sudo — printed, never executed, so the escalation stays auditable.
		fmt.Fprint(c.stdout, bobTrustNote(ide.caPath))
	}
	if shellChanges && shellTotal == 0 {
		// The command is meant to be COPIED AND RUN, so it gets a line of its own
		// — and shellQuote, because an unquoted "source /Users/a b/.zshrc" splits
		// at the space and sources /Users/a, so the rc file is written correctly
		// and the instructions for loading it are broken: the worse of the two
		// failures to have.
		fmt.Fprint(c.stdout, "  Open a new terminal for the \"bob\" function, or run:\n")
		fmt.Fprintf(c.stdout, "    source %s\n", shellQuote(c.shellRC))
	}
	if c.launch.macOS && ide != nil {
		// Quiet unless something is wrong: "Enabled." followed by silence reads as
		// verified, and the already-enabled arm is where the probe's answer is the
		// point of the run. No plan, no CA to check against.
		c.reportProbe(ide.caPath, false)
	}
	if c.shellAdvise != nil {
		c.shellAdviseOut("enable", ideBlock(ide))
	}
	fmt.Fprintln(c.stdout, "Undo with: agentop configure bob disable")
}

// bobManualBlock is enable's half of bobShellAdviseManual: the block, for the user to
// place, when agentop will not edit the startup file itself.
func bobManualBlock(reason error, block string, stdout io.Writer) {
	fmt.Fprintf(stdout, "agentop will not edit your startup file: %v.\n"+
		"  Add this to whichever file your login shell reads at startup:\n%s\n", reason, block)
}

// bobDisablePlan is the editor half of disable, computed before anything is
// written for the same one-prompt reason as enable's. The notes are findings the
// preview shows: values and blocks agentop refuses to touch, left alone with the
// reason.
type bobDisablePlan struct {
	// edits are the settings.json removals (nil value = delete the member).
	edits []bobEdit
	// proxyVal is the proxy value being removed, for the preview line.
	proxyVal string
	// guessing marks the bobUnknown case: with no readable config there was no
	// address to compare against, and the value is judged by shape alone.
	guessing bool
	// removeRecord deletes the ownership record along with the key it describes.
	removeRecord bool
	// removeBlock removes the CA-trust block from the rc file (macOS only).
	// block is the exact bytes to remove, rebuilt from the same config.
	removeBlock bool
	block       string
	// notes are the left-alone findings, printed with the preview.
	notes []string
}

// planBobDisable computes the editor half's removals, printing its refusals to
// stderr. Nil plan with code 1 means it will not act at all; code 0 with no
// changes means there was nothing of agentop's to remove.
func planBobDisable(settingsPath, cortexCfgPath, wantProxy, caPath string, launch bobLaunch, yes bool, stderr io.Writer) (*bobDisablePlan, int) {
	if launch.macOS && launch.rcReadErr != nil {
		fmt.Fprintf(stderr, "agentop: %v\n", launch.rcReadErr)
		return nil, 1
	}

	// The settings half. No settings file — or no Bob at all — holds nothing of
	// agentop's, which is a finding and not a failure: the shell block may still need
	// removing, and the off switch has to work whatever state Bob is in.
	p := &bobDisablePlan{}
	note := func(format string, args ...any) { p.notes = append(p.notes, fmt.Sprintf(format, args...)) }
	owns := bobNotOurs
	switch nerr := bobNoDocument(settingsPath); {
	case nerr != nil:
		note("%v; nothing to remove there.", nerr)
	case !bobSettingsExist(settingsPath):
		note("%s does not exist; nothing to remove there.", settingsPath)
	default:
		if derr := bobDuplicateKey(settingsPath); derr != nil {
			fmt.Fprintf(stderr, "agentop: %s: %v\n", settingsPath, derr)
			return nil, 1
		}
		doc, err := readSettings(settingsPath)
		if err != nil {
			fmt.Fprintf(stderr, "agentop: %v\n", err)
			return nil, 1
		}

		if existing, ok := doc[bobProxyKey]; !ok {
			note("%s sets no %q.", settingsPath, bobProxyKey)
		} else {
			s, isString := existing.(string)
			// One verdict, read three times below. bobOwns is pure and the inputs do
			// not change between those reads, so the repetition was only an invitation
			// to let two of them drift apart.
			owns = bobOwns(s, wantProxy)
			if !isString || owns == bobNotOurs {
				// Not a failure. "Remove it only if it points at Cortex" is the
				// contract, and this file already satisfies it — there is nothing for
				// the user to fix.
				note("%s sets %q to %v, which is not a Cortex proxy.\n"+
					"  Left alone — agentop removes only values it would have written.",
					settingsPath, bobProxyKey, existing)
			} else {
				// A guess plus --yes is not consent. bobUnknown means the config could
				// not be read, so there was no address to compare against and all that
				// is known is the value's SHAPE: a loopback http proxy. That describes
				// every local proxy anyone runs — Squid on 3128, a corporate agent, a
				// dev tunnel — not just Cortex's, because with no config there is no
				// port to compare at all.
				//
				// Interactively that is survivable: the preview prints the value and
				// the user recognizes their own proxy. --yes removes exactly that
				// safeguard, so a scripted `disable --yes` would silently delete a
				// stranger's proxy. Refuse instead, and say which flag turns the guess
				// into an answer. Exit 1, not 0: the user asked for a removal that did
				// not happen. Nothing else is removed either, so a refusal never leaves
				// half a disable behind.
				if yes && owns == bobUnknown {
					// Name the config path. Without it the message is the same whether
					// Cortex is uninstalled or --config was a typo, and only the second is
					// worth retrying.
					fmt.Fprintf(stderr, "agentop: %s sets %q to %q, which is shaped like a Cortex\n"+
						"  proxy but cannot be confirmed as one: %s could not be read, so there is no\n"+
						"  address to compare against — every loopback http proxy looks like this.\n"+
						"  Refusing to delete it unattended. Re-run without --yes to see the value and\n"+
						"  decide, or pass --config with a readable Cortex config.\n",
						settingsPath, bobProxyKey, s, cortexCfgPath)
					return nil, 1
				}
				p.proxyVal = s
				p.guessing = owns == bobUnknown
				p.edits = append(p.edits, bobEdit{bobProxyKey, nil})
			}
		}

		if launch.macOS {
			st, serr := readState(launch.statePath)
			_, present := doc[bobSysCertsKey]
			switch {
			case !present:
			case serr != nil:
				// An unreadable record cannot say the value is ours, and a false the
				// user set is indistinguishable from the one enable writes.
				note("%q left alone: the record of whether agentop set it is unreadable (%v).",
					bobSysCertsKey, serr)
			case bobOwnsSysCerts(st, settingsPath, doc):
				p.edits = append(p.edits, bobEdit{bobSysCertsKey, nil})
				p.removeRecord = true
			default:
				note("%q is %v, which agentop did not set. Left alone.", bobSysCertsKey, doc[bobSysCertsKey])
			}
		}
	}

	// The launch half: the CA-trust block, by the same rules bobshell applies to
	// its own.
	block := bobLaunchBlock(caPath)
	if launch.macOS {
		if launch.rcErr != nil {
			note("agentop will not edit your startup file: %v.\n"+
				"  If you added the block yourself, delete it between %s\n  and %s.",
				launch.rcErr, bobLaunchMarkerStart, bobLaunchMarkerEnd)
		} else {
			b, rerr := os.ReadFile(launch.rcPath)
			if rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
				fmt.Fprintf(stderr, "agentop: read %s: %v\n", launch.rcPath, rerr)
				return nil, 1
			}
			content := string(b)
			// Counting rather than testing presence: every count but 1 is a case where
			// removing one copy would leave the file in a state nobody chose.
			switch copies, edited := bobRCBlockState(content, block); {
			case copies == 1:
				p.removeBlock = true
				p.block = block
			case copies > 1:
				note("%s has %d copies of the cortex bob block.\n"+
					"  Left alone — remove them by hand, between the marker lines:\n\n  %s\n  ...\n  %s",
					launch.rcPath, copies, bobLaunchMarkerStart, bobLaunchMarkerEnd)
			case edited:
				// Edited by hand, or written for a CA path the config no longer names —
				// either way not the bytes this run would remove.
				note("%s has a cortex bob block that is not the one this agentop writes for\n"+
					"  %s. Left alone — remove it by hand if you want it gone, between the\n"+
					"  marker lines:\n\n  %s\n  ...\n  %s",
					launch.rcPath, caPath, bobLaunchMarkerStart, bobLaunchMarkerEnd)
			}
		}
	}
	return p, 0
}

// disable routes both faces of Bob direct again, removing only what enable
// wrote: one preview, one prompt, independent writes.
func (c bobCommand) disable() int {
	wantProxy, caPath := bobWanted(c.cortexCfgPath, c.home)

	// --- The plans ---
	var ide *bobDisablePlan
	// 1 when the editor half is skipped — the same rule as enable's, for the
	// same reason: the half's change did not happen.
	ideCode := 0
	if c.ideSkip {
		ideCode = 1
	} else {
		ide, ideCode = planBobDisable(c.settingsPath, c.cortexCfgPath, wantProxy, caPath, c.launch, c.yes, c.stderr)
	}
	var shell rcPlan
	shellCode := 0
	if c.shellAdvise == nil {
		p, err := bobShellDisablePlan(c.shellRC)
		if err != nil {
			fmt.Fprintf(c.stderr, "agentop: read %s: %v\n", c.shellRC, err)
			shellCode = 1
		} else {
			shell = p
		}
	}

	ideChanges := ide != nil && (len(ide.edits) > 0 || ide.removeBlock)
	shellChanges := c.shellAdvise == nil && shellCode == 0 && shell.write
	if !ideChanges && !shellChanges {
		// The off switch has to work whatever state Bob is in. What each half
		// found — nothing of ours to remove, or something it refuses to touch —
		// is the report; a half that failed has said why on stderr.
		if ide != nil {
			for _, n := range ide.notes {
				fmt.Fprintln(c.stdout, n)
			}
		}
		if shell.note != "" {
			fmt.Fprintln(c.stdout, shell.note)
		}
		if c.shellAdvise != nil {
			c.shellAdviseOut("disable", "")
		}
		// The closing line the old halves each printed in their own words: whatever
		// was found above, this run changed nothing.
		fmt.Fprintln(c.stdout, "Not enabled. Nothing to do.")
		return bobCombine(ideCode, shellCode)
	}

	// --- The preview ---
	var lines []string
	count := 0
	if ideChanges && len(ide.edits) > 0 {
		lines = append(lines, "  "+tildePath(c.home, c.settingsPath))
		for _, e := range ide.edits {
			if e.key == bobProxyKey {
				lines = append(lines, fmt.Sprintf("    remove %q (now %q)", bobProxyKey, ide.proxyVal))
			} else {
				lines = append(lines, fmt.Sprintf("    remove %q", bobSysCertsKey))
			}
		}
		count += len(ide.edits)
	}
	var rcLines []string
	if ideChanges && ide.removeBlock {
		rcLines = append(rcLines, "    remove the CA-trust block")
		count++
	}
	if shellChanges {
		rcLines = append(rcLines, "    remove the bob() function")
		count++
	}
	if len(rcLines) > 0 {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, "  "+tildePath(c.home, c.shellRC))
		lines = append(lines, rcLines...)
	}
	plural := "s"
	if count == 1 {
		plural = ""
	}
	fmt.Fprintf(c.stdout, "Route IBM Bob direct again — %d change%s:\n\n", count, plural)
	for _, l := range lines {
		fmt.Fprintln(c.stdout, l)
	}

	// --- Findings, then the one prompt ---
	// shell.leftAlone, not shellChanges and not note alone: a finding is set
	// exactly when the half will NOT write (so shellChanges is false), and a
	// plain absence is a note that is not a finding. With editor-half changes
	// pending, this is the only line that says the rc file was left alone.
	hasFindings := (ide != nil && len(ide.notes) > 0) || (ideChanges && ide.guessing) || shell.leftAlone()
	if hasFindings {
		fmt.Fprintln(c.stdout)
	}
	if ide != nil {
		for _, n := range ide.notes {
			fmt.Fprintln(c.stdout, n)
		}
	}
	if hasFindings && ideChanges && ide.guessing {
		// Say it is a guess, because it is: with no readable config there is no
		// address to compare against, and what is left is that the value is a
		// loopback http proxy — the shape agentop writes. Removing it is still the
		// right default (this is the uninstalled-Cortex case, when the off switch
		// matters most), but the user should know which of the two answers they are
		// getting, and the preview above names the value before anything is written.
		fmt.Fprintf(c.stdout, "Note: %s is not readable, so %q is judged by shape alone — a loopback\n"+
			"  proxy, which is what agentop writes.\n", c.cortexCfgPath, bobProxyKey)
	}
	if shell.leftAlone() {
		fmt.Fprintln(c.stdout, shell.note)
	}
	if !c.yes && !bobConfirm(c.stdout) {
		fmt.Fprintln(c.stdout, "Not changed. Re-run with --yes to apply without asking.")
		return exitDeclined
	}

	// --- Apply ---
	// nil value means delete. bobWriteKeys, not writeSettings: the promise printed
	// above is that nothing else in the file changes, and a re-marshal would
	// reorder and reindent every other key.
	ideTotal := ideCode
	if ideChanges {
		ideTotal = c.applyDisableEditor(ide)
	}
	shellTotal := shellCode
	if shellChanges {
		shellTotal = shell.apply(c.stderr)
	}

	// --- The report ---
	fmt.Fprintln(c.stdout)
	switch {
	case ideTotal == 0 && shellTotal == 0 && !shell.leftAlone():
		fmt.Fprintln(c.stdout, "Disabled — IBM Bob no longer routes through Cortex.")
	case ideTotal == 0 && ideChanges:
		// The shell half failed, was skipped, or was left alone with a finding
		// printed above; in every one of those bob() may still be in the rc
		// file, and the headline must not claim otherwise. A plain absence is
		// not any of those: nothing of the shell half's was there to remove,
		// so a full disable happened.
		fmt.Fprintln(c.stdout, "Disabled the editor half.")
	case shellTotal == 0 && shellChanges:
		fmt.Fprintln(c.stdout, "Disabled the shell half.")
	default:
		fmt.Fprintln(c.stdout, "Not changed — the errors above say why.")
		return bobCombine(ideTotal, shellTotal)
	}
	if ideChanges && ideTotal == 0 {
		if c.launch.macOS {
			fmt.Fprint(c.stdout, "  Quit IBM Bob and reopen it so it re-reads its settings.\n")
		} else {
			fmt.Fprint(c.stdout, "  Restart Bob so it re-reads its settings.\n")
		}
	}
	if shellChanges && shellTotal == 0 {
		fmt.Fprint(c.stdout, "  \"bob\" keeps working in shells already open; a new terminal won't have it.\n")
	}
	if c.shellAdvise != nil {
		c.shellAdviseOut("disable", "")
	}
	fmt.Fprint(c.stdout, bobUntrustNote(caPath, c.launch.macOS))
	return bobCombine(ideTotal, shellTotal)
}

// applyDisableEditor performs the editor half's removals: the keys, then the rc
// block, then the ownership record — the record last, so it only disappears once
// the key it describes is gone (left behind, it would claim a later false the
// user sets themselves as agentop's).
func (c bobCommand) applyDisableEditor(p *bobDisablePlan) int {
	if len(p.edits) > 0 {
		if werr := bobWriteKeys(c.settingsPath, p.edits); werr != nil {
			fmt.Fprintf(c.stderr, "agentop: %v\n", werr)
			return 1
		}
	}
	if p.removeBlock {
		// Re-read, for the same reason the enable apply does: the shell half
		// removes its own block from this same file, and each half's write must
		// carry the other's change.
		content, rerr := os.ReadFile(c.launch.rcPath)
		if rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			fmt.Fprintf(c.stderr, "agentop: read %s: %v\n", c.launch.rcPath, rerr)
			return 1
		}
		if werr := writeRCFile(c.launch.rcPath, strings.Replace(string(content), p.block, "", 1)); werr != nil {
			fmt.Fprintf(c.stderr, "agentop: %v\n", werr)
			return 1
		}
	}
	if p.removeRecord {
		if rerr := os.Remove(c.launch.statePath); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			fmt.Fprintf(c.stderr, "agentop: removing %s: %v\n", c.launch.statePath, rerr)
			return 1
		}
	}
	return 0
}

// bobDisableEditor applies the editor half's disable with no prompt — the
// `agentop uninstall` path, which wraps each removal in its own checklist row and
// captures the output. The interactive command goes through bobCommand.disable,
// which shows both halves' changes under one prompt.
func bobDisableEditor(settingsPath, cortexCfgPath, wantProxy, caPath string, launch bobLaunch, stdout, stderr io.Writer) int {
	p, code := planBobDisable(settingsPath, cortexCfgPath, wantProxy, caPath, launch, true, stderr)
	if code != 0 || p == nil {
		return code
	}
	if len(p.edits) == 0 && !p.removeBlock {
		for _, n := range p.notes {
			fmt.Fprintln(stdout, n)
		}
		return 0
	}
	c := bobCommand{settingsPath: settingsPath, cortexCfgPath: cortexCfgPath, launch: launch, stdout: stdout, stderr: stderr}
	return c.applyDisableEditor(p)
}

// The two verdicts status can reach, and the first line of every report it writes.
//
// Package-level so the tests assert the same strings the code prints rather than a
// copy: a wording change that updates only one of the two would otherwise pass.
//
// Note that bobStatusNo CONTAINS the prefix of bobStatusYes up to "is", so neither may
// be checked with a prefix or substring test that could accept the other — see
// TestBobStatus_ThreeStates.
const (
	bobStatusYes = "IBM Bob is configured to use the Cortex proxy"
	bobStatusNo  = "IBM Bob is *NOT* configured to use the Cortex proxy"
	// A third answer, not a flavour of bobStatusNo: "not configured" is a claim about
	// Bob's settings, and with no settings document there is nothing to make it about.
	bobStatusUnknown = "IBM Bob's Cortex status is unknown"
)

// status reports both faces in three lines: the verdict, then one line per face.
// It writes nothing and runs nothing — only enable starts the user's shell — so
// "the CA-trust block is there" is as far as the editor line can go.
//
// Always exit 0: "not configured" is a successful report, the same call
// claudeCodeStatus makes. A non-zero status here would make `agentop configure
// bob status` unusable in a shell conditional for anything but "is it on".
func (c bobCommand) status() int {
	wantProxy, caPath := bobWanted(c.cortexCfgPath, c.home)

	// No Bob, or a `null` document, means no answer. The verdict this used to print
	// was bobStatusNo with `"http.proxy" is unset` under it — a positive report about
	// a Bob that is not there, indistinguishable from a real Bob that simply is not
	// routed through Cortex. A settings.json missing from a Bob directory that
	// exists is not this case: that is a Bob on its defaults.
	if nerr := bobNoDocument(c.settingsPath); nerr != nil {
		fmt.Fprintf(c.stdout, "%s\n  editor: %v\n", bobStatusUnknown, nerr)
		fmt.Fprintf(c.stdout, "  shell: %s\n", bobShellStatusLine())
		return 0
	}
	if derr := bobDuplicateKey(c.settingsPath); derr != nil {
		// Two values, and no way to say which one Bob uses without reimplementing its
		// precedence. Reporting either as "the setting" would be a guess, so the
		// verdict is "not" — this command cannot confirm the configuration — and the
		// reason is the actionable part. Still exit 0: it is a successful report.
		fmt.Fprintf(c.stdout, "%s\n  editor: %s: %v\n", bobStatusNo, c.settingsPath, derr)
		fmt.Fprintf(c.stdout, "  shell: %s\n", bobShellStatusLine())
		return 0
	}
	doc, err := readSettings(c.settingsPath)
	if err != nil {
		// An unreadable or non-JSON settings file cannot be configured, so the verdict
		// is the same "not" — with the reason, which is the actionable part.
		fmt.Fprintf(c.stdout, "%s\n  editor: %v\n", bobStatusNo, err)
		fmt.Fprintf(c.stdout, "  shell: %s\n", bobShellStatusLine())
		return 0
	}

	// Findings are collected rather than printed inline, so the verdict and the
	// two face lines can go first regardless of which branch produced them.
	var findings []string
	add := func(format string, args ...any) { findings = append(findings, fmt.Sprintf(format, args...)) }

	// Only a value this command recognises as the Cortex proxy is probed for liveness.
	// Probing a foreign one warns that someone's corporate proxy is down, which is
	// neither true (it is reachable from somewhere, just not here) nor any of Cortex's
	// business — and it reads as a complaint about a setting agentop deliberately leaves
	// alone. An unjudgeable loopback value IS probed: it is the shape agentop writes, and
	// disable would act on it, so its liveness is informative.
	//
	// The VERDICT is bobStateFrom's (configure_status.go), so this paragraph and the row
	// `agentop configure status` prints cannot disagree about whether Bob is routed —
	// and it is handed THE DOCUMENT ALREADY READ ABOVE rather than a path, so a file
	// edited between the two reads cannot produce a verdict contradicting the line
	// printed under it.
	//
	// Only configureOn becomes bobStatusYes, so this command's wording is unchanged by
	// any of the table's finer distinctions: configurePartial is a drifted loopback
	// proxy, or on macOS our proxy without the launch half, and configureUnknown the
	// unjudgeable-loopback case — all reported as bobStatusNo with the reason among the
	// findings below.
	verdict, listening := bobStatusNo, ""
	if bobStateFrom(c.settingsPath, doc, c.cortexCfgPath, wantProxy, c.launch, caPath).State == configureOn {
		verdict = bobStatusYes
	}
	if !bobSettingsExist(c.settingsPath) {
		// A finding about Bob, not a fault: it writes the file only when a setting is
		// first changed, and enable creates it.
		add("%s does not exist yet — IBM Bob writes it the first time a setting changes,\n"+
			"    and `agentop configure bob enable` creates it", tildePath(c.home, c.settingsPath))
	}

	// The editor line: the setting, and on macOS the CA-trust half — the two things
	// enable writes — followed by whatever findings came out of judging them.
	var editor string
	switch existing := doc[bobProxyKey].(type) {
	case nil:
		editor = fmt.Sprintf("%q unset in %s", bobProxyKey, tildePath(c.home, c.settingsPath))
	case string:
		owns := bobOwns(existing, wantProxy)
		editor = fmt.Sprintf("%q=%s in %s", bobProxyKey, existing, tildePath(c.home, c.settingsPath))
		switch {
		case owns == bobOurs:
			listening = existing
		case owns == bobUnknown:
			listening = existing
			// The value cannot be judged without something to compare it against, and
			// claiming "not a Cortex proxy" here would be a statement about the
			// settings file resting on the absence of a config file. Report both.
			add("that is a loopback proxy, which is the shape agentop writes, but %s is not\n"+
				"    readable — so whether it is this machine's Cortex proxy cannot be told from here",
				c.cortexCfgPath)
		case wantProxy == "":
			add("that is not a local proxy, and %s is not readable", c.cortexCfgPath)
		case bobIsLoopbackProxy(existing):
			// Drift: a loopback proxy that is not the one the config names now. Almost
			// always a Cortex address from before the port moved, which is worth
			// naming as such — but it is NOT ours by the ownership test, so disable
			// leaves it alone. Reporting drift is useful; deleting on a guess is not.
			add("that is a local proxy, but %s now names %s", c.cortexCfgPath, wantProxy)
			// NOT "run enable to move it", which is what this used to say and what no
			// user could do: ownership is an exact host+port match, so enable sees a
			// drifted port as bobNotOurs and refuses it with exit 1. Both halves of
			// that refusal are deliberate, so the advice is what changes: name the two
			// steps that do work, in the order they work in.
			add("to move it: change that value to %s by hand, or remove it and run\n"+
				"    `agentop configure bob enable`", wantProxy)
		default:
			add("that is not this machine's Cortex proxy, which %s puts at %s", c.cortexCfgPath, wantProxy)
		}
	default:
		editor = fmt.Sprintf("%q=%v in %s", bobProxyKey, existing, tildePath(c.home, c.settingsPath))
		add("that value is a %T, not a string", existing)
	}

	// The launch half of the editor line, from files alone: whether
	// http.systemCertificates is false and whether the CA-trust block is in the rc
	// file. bobLaunchCheck is the same read the status table's row makes, so the
	// line and the row cannot disagree; its problems are the findings.
	if c.launch.macOS {
		if ok, problems := bobLaunchCheck(c.launch, caPath, doc); ok {
			editor += ", CA-trust block in " + tildePath(c.home, c.launch.rcPath)
		} else {
			findings = append(findings, problems...)
		}
	}

	fmt.Fprintln(c.stdout, verdict)
	// Liveness is a separate axis from ownership, and this is a WARNING rather than
	// part of the verdict because the setting is correct either way: Cortex being
	// stopped is the normal state of a laptop, and the answer is `agentop service
	// start`, not an edit to Bob's settings.
	if listening != "" && !bobProxyIsListening(listening) {
		fmt.Fprintf(c.stdout, "WARNING: No proxy is listening at %s\n", listening)
	}
	fmt.Fprintf(c.stdout, "  editor: %s\n", editor)
	for _, f := range findings {
		fmt.Fprintf(c.stdout, "    %s\n", f)
	}
	fmt.Fprintf(c.stdout, "  shell: %s\n", bobShellStatusLine())
	return 0
}

// bobShellStatusLine is the shell half's one line in status: what the environment
// says, which is all a child process can see of a shell function.
//
// The variable is inherited by every child process — including a NON-INTERACTIVE
// subshell, which does not read the rc file and therefore has no bob function at
// all. So "the variable is set" is evidence and not a verdict, which is why the
// line names the confirmation step rather than claiming the function. "type bob",
// NOT "which bob": in bash `which` is /usr/bin/which, a separate process, and a
// child cannot see its parent's function table — so with the function live and a
// bob binary on PATH, bash's `which bob` prints the BINARY'S path. `type` is a
// POSIX shell builtin, so it sees the function in every shell that matters.
func bobShellStatusLine() string {
	if bobShellState().State == configurePartial {
		return `"bob" configured — run "type bob" to confirm the function in this shell`
	}
	return "not enabled in this shell (just ran enable? open a new terminal — this one has not re-read the rc file)"
}

// statusShellOnly is status's answer when the platform names no settings path
// and --settings did not either: runBob has printed the unknown verdict and the
// reason, and the shell half's line is still a report worth having.
func (c bobCommand) statusShellOnly() int {
	fmt.Fprintf(c.stdout, "  shell: %s\n", bobShellStatusLine())
	return 0
}
