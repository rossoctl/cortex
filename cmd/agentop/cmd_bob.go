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

	// bobProxyKey is the one setting this command writes. VS Code's settings
	// document is FLAT — dotted keys are literal top-level keys, not a nested
	// object — so this is the whole path, unlike claude-code's "env" block.
	bobProxyKey = "http.proxy"

	// bobCACommonName is the subject every generated bridge CA carries. A literal
	// rather than an import: bridgeCACommonName lives in package main of
	// cmd/cortex and is not importable from here. darwinGoNote already
	// carries the same copy for the same reason.
	bobCACommonName = "authbridge-tls-bridge-ca"

	// bobSystemKeychain is machine-wide trust, which is the right scope for a
	// proxy that every process on the box is pointed at.
	bobSystemKeychain = "/Library/Keychains/System.keychain"
)

const bobUsage = `agentop configure bob — route IBM Bob through Cortex via its settings.json

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

IBM Bob is a VS Code fork, so it has a settings file and one flat key decides
where its networking goes: "http.proxy". enable sets it to Cortex's forward
proxy, read from ~/.cortex/config.yaml so the address always matches the proxy
that is actually running. Only that one key is written — every other setting is
left exactly as it was, and the first write copies the original to
settings.json.bak and never overwrites that copy.

disable removes "http.proxy" ONLY when it points at Cortex. A value that does
not — a corporate proxy, say — is reported and left alone, because a proxy agentop
did not write is not agentop's to delete.

The proxy is only half of it. Bob's requests are terminated by Cortex's TLS
bridge, so Bob must also trust the bridge CA, and that is an OS trust-store
change agentop does not make for you: it needs sudo, and a tool that silently
escalates to alter machine-wide trust is not one you can audit. enable prints
the command that does it, disable prints how to undo it, status prints how to
check it.

"agentop configure bobshell" is a different thing and the two are independent.
That one defines a "bob" shell function so typing "bob" runs through Cortex;
this one configures what the editor does on its own.

Comments: VS Code permits them in settings.json and this command does not. It
reads the file as strict JSON and refuses a commented one by name rather than
rewriting it, because a rewrite would silently delete the comments.

Exit status: 0 applied, already correct, or reported; 3 declined — at the prompt
or because there was no terminal to ask on; 1 something went wrong; 2 a usage
error.
`

// bobConfirm prompts before a write. A var so tests can substitute it: `go test`
// inherits the terminal it was launched from, so an unstubbed prompt blocks
// waiting on a human.
//
// Deliberately a separate var from bobShellConfirm rather than a reuse of it.
// They are identical today, but sharing one would mean a test stubbing one
// verb's prompt silently disarms the other command's too — and a test that
// cannot fail is worse than a duplicated three-line closure.
var bobConfirm = func(path, what string, stdout io.Writer) bool {
	fmt.Fprintf(stdout, "%s %s\n", what, path)
	return confirm(stdout)
}

// runBob dispatches `agentop configure bob`. Returns the process exit code.
func runBob(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, bobUsage)
		return 2
	}
	action := args[0]
	// Before anything else: --help asks for this command's usage, and reading it
	// as an action name would send someone looking for the command list to the one
	// branch that refuses to print it. Same split as bobshell's and claude-code's.
	switch action {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, bobUsage)
		return 0
	}

	// The verb is validated HERE, before any environment or filesystem work, for
	// the reason runBobShell documents at length: a step below can answer
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
	if *settingsPath == "" {
		p, perr := bobSettingsPath(home)
		if perr != nil {
			// 2, not 1: the missing fact is one the caller can supply on the command
			// line, which makes this a usage error rather than a failure. Unlike
			// bobshell's off-platform arm there is no block of text to paste that
			// would constitute an answer on its own.
			fmt.Fprintf(stderr, "agentop: %v.\n  Pass --settings PATH to point at it.\n", perr)
			return 2
		}
		*settingsPath = p
	}

	switch action {
	case "enable":
		// enable derives the CA path itself, from the config it already requires.
		return bobEnable(*settingsPath, *cortexCfgPath, yes, stdout, stderr)
	case "disable":
		wantProxy, caPath := bobWanted(*cortexCfgPath, home)
		return bobDisable(*settingsPath, *cortexCfgPath, wantProxy, caPath, yes, stdout, stderr)
	default:
		wantProxy, caPath := bobWanted(*cortexCfgPath, home)
		return bobStatus(*settingsPath, *cortexCfgPath, wantProxy, caPath, stdout)
	}
}

// bobSettingsPath is where IBM Bob's settings live, for the platforms where that
// is known.
//
// macOS only, deliberately. ~/.config/IBM Bob/User/settings.json is the VS Code
// convention on Linux and would be the obvious guess, but it is unverified for
// this app — and writing a proxy setting into a file nothing reads is a silent
// no-op that leaves the user no reason to look there. bobShellRCPath makes the
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

// bobTrustNote is the trust-store step agentop does NOT take, printed for the user
// to run.
//
// Printed rather than executed on purpose. Adding a root to the System keychain
// needs sudo, and a tool that escalates on its own to change machine-wide trust
// is not one anybody can audit after the fact — the user should see the exact
// command before it runs. Same reasoning as darwinGoNote, which prints its
// keychain command too.
//
// caPath is ca.crt — the single bridge CA — and NOT bundle.crt, even though
// bundle.crt is the file most of agentop's other CA messages name. The keychain is
// ADDITIVE, so it wants one certificate; bundle.crt exists for the tools whose CA
// setting REPLACES their trust store, and it holds the bridge CA followed by
// every platform root. `add-trusted-cert -r trustRoot` on that file would install
// explicit root-trust settings for every public CA in it machine-wide — a count
// that varies by machine and by platform, which is the reason not to name one —
// and that is both far broader than intended and not undone by the single
// delete-certificate below. See
// core/tlsbridge/bundle.go's header, which states which file is for which job.
func bobTrustNote(caPath string) string {
	q := shellQuote(caPath)
	if runtime.GOOS == "darwin" {
		return "Bob must also trust the bridge CA, or every https request fails verification.\n" +
			"  That is a machine-wide trust change needing sudo, so agentop does not make it\n" +
			"  for you — run:\n\n" +
			"    sudo security add-trusted-cert -d -r trustRoot \\\n" +
			"      -k " + bobSystemKeychain + " " + q + "\n\n" +
			"  That is the single bridge CA, not the " + tlsbridge.TrustBundleName + " in the same\n" +
			"  directory: the keychain adds to what it already trusts, so it wants one\n" +
			"  certificate. The bundle carries every platform root as well, and trusting it\n" +
			"  as a root would change how this machine treats certificates that have\n" +
			"  nothing to do with Cortex.\n\n"
	}
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
func bobUntrustNote(caPath string) string {
	q := shellQuote(caPath)
	if runtime.GOOS == "darwin" {
		// Two commands, because the add did two things and one undo does not cover
		// both. `add-trusted-cert -d` writes TRUST SETTINGS to the admin domain, and
		// `remove-trusted-cert -d` is its documented inverse — the -d has to be
		// repeated or it removes from the user domain, which the add never wrote to.
		// `delete-certificate -t` was wrong here on its own: it deletes the
		// certificate and, per its own usage text, "user trust settings", leaving the
		// admin-domain trust the add created in place. The keychain must be named on
		// the delete for the same reason -d is repeated on the remove: an add to the
		// System keychain is not undone by a delete that defaults to the login one.
		return "Optionally, undo the trust change as well — the trust settings first,\n" +
			"  then the certificate:\n\n" +
			"    sudo security remove-trusted-cert -d " + q + "\n" +
			"    sudo security delete-certificate -c " + bobCACommonName + " \\\n" +
			"      -t " + bobSystemKeychain + "\n\n" +
			"  Safe to leave in place if you expect to re-enable: it only validates\n" +
			"  certificates Cortex itself issues, so nothing else starts being trusted\n" +
			"  because it is there.\n\n"
	}
	return "Optionally, remove " + shellQuote(caPath) + " from this system's\n" +
		"  trusted certificate store — delete the copy you added (under\n" +
		"  /usr/local/share/ca-certificates/ or /etc/pki/ca-trust/source/anchors/) and\n" +
		"  re-run update-ca-certificates / update-ca-trust.\n\n" +
		"  Safe to leave in place if you expect to re-enable: it only validates\n" +
		"  certificates Cortex itself issues.\n\n"
}

// bobVerifyNote is status's suggestion, and is only ever printed — running
// verify-cert here would turn a report into an action, and status writes nothing.
//
// Nothing is printed off macOS: there is no portable equivalent to suggest, and
// inventing one is the confidently-wrong-command failure bobTrustNote's non-mac
// arm already hedges against.
func bobVerifyNote(caPath string) string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	return "To check that this machine trusts the bridge CA:\n\n" +
		"    security verify-cert -c " + shellQuote(caPath) + "\n\n" +
		"  A failure does not by itself mean Bob is misconfigured: the CA only matters\n" +
		"  for hosts the bridge terminates, and it is trusted separately from the\n" +
		"  proxy setting above.\n"
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

// bobWriteKey is bob's replacement for writeSettings: same backup and atomic-rename
// behaviour, but the content is the original file with one key edited rather than a
// re-marshal of the whole document. value == nil deletes the key.
func bobWriteKey(path, key string, value any) error {
	src, rerr := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if rerr != nil {
		if !os.IsNotExist(rerr) {
			return rerr
		}
		// No file yet: start from an empty object so the insert path has a document
		// to work on. Nothing to back up either.
		//
		// Unreachable from enable and disable, for the same reason bobBackupNote's
		// no-backup arm is: bobNoDocument gates both verbs on the file existing and
		// being non-empty, so a caller that gets here has already found one. Kept
		// rather than deleted, on the same terms — bobWriteKey's contract is to edit
		// whatever path it is handed, and dropping the arm would turn a first write
		// into an error for any future caller that does not pre-check.
		src = []byte("{}\n")
	} else {
		// Back the file up ONCE and never overwrite it, matching writeSettings: a
		// second enable, or an enable/disable pair, must not replace the pristine
		// pre-Cortex file with one agentop already edited.
		bak := path + ".bak"
		if _, serr := os.Stat(bak); os.IsNotExist(serr) {
			if werr := os.WriteFile(bak, src, 0o600); werr != nil {
				return fmt.Errorf("writing backup %s: %w", bak, werr)
			}
		}
	}

	// An empty or whitespace-only file is an empty document, the same reading
	// readSettings gives it.
	if len(bytes.TrimSpace(src)) == 0 {
		src = []byte("{}\n")
	}

	out, err := bobSetKey(src, key, value)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	// Never hand back something that is not valid JSON, whatever the splicing did.
	// Bob reads this file; a malformed one is worse than a reformatted one.
	var check map[string]any
	if jerr := json.Unmarshal(out, &check); jerr != nil {
		return fmt.Errorf("%s: edit would produce invalid JSON (%w); file left unchanged", path, jerr)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	// 0600: this file commonly holds API tokens in the same block.
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// bobBackupNote describes what this particular write will and will not preserve.
//
// Three different true statements, because bobWriteKey makes three different
// choices and the message used to claim only the first. It writes <path>.bak from
// the file's current contents ONLY when the file exists AND no .bak is there
// already — never overwriting, because a second run would otherwise replace the
// pristine pre-Cortex file with one agentop had already edited.
//
// So "a copy is kept as <path>.bak" was false twice over: on a settings file that
// does not exist yet there is nothing to copy, and when a .bak survives from an
// earlier run the copy kept is that older one, not this run's. Promising a backup
// that is not made is worse than promising none — it is the sentence a user leans on
// before saying yes.
func bobBackupNote(settingsPath string) string {
	// This sentence is a literal claim, and bobWriteKey is what makes it one: it splices
	// a single member in or out of the existing bytes, so key order, indentation, inline
	// arrays and blank-line grouping all survive. It was NOT true of the first version
	// of this command, which re-marshalled the parsed document and so silently
	// alphabetized and reformatted the whole file — see bobWriteKey, and
	// TestBobEnable_ChangesOneLineAndNoOtherByte, which pins it byte-for-byte.
	//
	// The same sentence appears in claude-code's enable, where it still goes through the
	// shared writeSettings and therefore still overstates what happens. Not changed here:
	// that is a different command's behaviour and out of this change's scope.
	const unchanged = "Nothing else in the file changes"
	bak := settingsPath + ".bak"
	if _, err := os.Stat(settingsPath); err != nil {
		// Covers a missing file and an unreadable one alike: in both cases this run
		// will not produce a .bak, which is the only thing being claimed.
		//
		// Unreachable from enable and disable since bobNoDocument gates both on the file
		// existing, so a caller that prints this note has already found one. Kept rather
		// than deleted: the note's job is to describe bobWriteKey truthfully for any
		// path handed to it, and its own test still exercises this arm directly.
		return unchanged + ".\n  No backup is made — there is no existing file to copy.\n\n"
	}
	if _, err := os.Stat(bak); err == nil {
		return unchanged + "; " + bak + " already exists and is\n" +
			"  left as it is, so it still holds the file as first found, not as it is now.\n\n"
	}
	return unchanged + "; a copy is kept as " + bak + "\n\n"
}

// bobDuplicateKey reports whether the settings file names the proxy key more than
// once at the top level, which readSettings cannot show: it decodes into a map, and a
// map keeps only the last of two same-named members.
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
	_, _, _, ferr := bobFindMember(src, bobProxyKey)
	// Only the duplicate verdict is this check's to report. Any other parse failure is
	// readSettings' to describe, and bob's readSettings arm says more about it than a
	// tokenizer error would.
	if ferr != nil && errors.Is(ferr, errBobDuplicateKey) {
		return ferr
	}
	return nil
}

// bobNoDocument reports why the settings file cannot be treated as IBM Bob's, or nil.
//
// readSettings cannot answer this: it returns an empty map for a missing file, for an
// empty one, and for a bare `null` alike, so every one of the three reads back as "a
// settings document that happens to set nothing". They are not the same thing. IBM Bob
// writes this file the first time it stores a setting, so no document at all means Bob
// has not run here — most often a --settings typo, or the wrong machine.
//
// That matters because the write path only discovers it late: `null` reaches bobSetKey,
// which rejects a non-object top level, but by then enable has printed what it will set,
// prompted, and copied the file to .bak. Promising a write that cannot happen is the
// failure being closed, so this runs before anything is printed.
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
			return fmt.Errorf("%s does not exist, so IBM Bob has not saved settings here", settingsPath)
		}
		return nil
	}
	if len(bytes.TrimSpace(src)) == 0 {
		return fmt.Errorf("%s is empty, so IBM Bob has not saved settings here", settingsPath)
	}
	if bytes.Equal(bytes.TrimSpace(src), []byte("null")) {
		return fmt.Errorf("%s holds only `null`, which sets nothing", settingsPath)
	}
	return nil
}

// bobNotInstalled is the refusal enable and disable share. One wording for both, because
// the reason is the same and only the verb differs.
func bobNotInstalled(what string, reason error, stderr io.Writer) int {
	fmt.Fprintf(stderr, "agentop: %v.\n"+
		"  Nothing to %s. Start IBM Bob and change any setting so it writes the file,\n"+
		"  or pass --settings with the path to a settings.json it does use.\n", reason, what)
	return 1
}

func bobEnable(settingsPath, cortexCfgPath string, yes bool, stdout, stderr io.Writer) int {
	want, cfg, err := wantedFromConfig(cortexCfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentop: %v\n", err)
		return 1
	}
	// Same gate as claude-code and exec, and for the same reason: with the bridge
	// off Cortex terminates no TLS, so pointing Bob at the proxy would send every
	// https request into something that cannot answer it.
	if !bridgeEnabled(cfg) {
		fmt.Fprintf(stderr, "agentop: %v\n", errBridgeDisabled(cortexCfgPath))
		return 1
	}
	caPath := want[envCACerts]
	if caPath == "" {
		fmt.Fprintf(stderr, "agentop: %s has no tls_bridge.ca_dir, so there is no CA for IBM Bob to\n"+
			"  trust; every https request would fail certificate verification. Enable the\n"+
			"  TLS bridge first.\n", cortexCfgPath)
		return 1
	}
	proxy := want[envProxy]

	if nerr := bobNoDocument(settingsPath); nerr != nil {
		return bobNotInstalled("enable", nerr, stderr)
	}

	if derr := bobDuplicateKey(settingsPath); derr != nil {
		fmt.Fprintf(stderr, "agentop: %s: %v\n", settingsPath, derr)
		return 1
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
		return 1
	}

	switch existing := doc[bobProxyKey].(type) {
	case nil:
		// Not set. Nothing to weigh.
	case string:
		if existing == proxy {
			fmt.Fprintf(stdout, "Already enabled: %s routes IBM Bob through Cortex.\n", settingsPath)
			return 0
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
			return 1
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
		return 1
	}

	// Enabling before Cortex's first start is legitimate — the proxy generates the
	// CA on boot — but the trust command printed below would fail on a missing
	// file, so say so now rather than let it be discovered at the sudo prompt.
	if _, serr := os.Stat(caPath); serr != nil {
		fmt.Fprintf(stdout, "Note: %s does not exist yet.\n"+
			"  Cortex creates it on first start. Start Cortex, then run the trust command\n"+
			"  below — until the CA is trusted, Bob's https requests fail verification.\n\n",
			caPath)
	}

	fmt.Fprintf(stdout, "Sets in %s:\n  %q: %q\n", settingsPath, bobProxyKey, proxy)
	fmt.Fprint(stdout, bobBackupNote(settingsPath))
	if !yes && !bobConfirm(settingsPath, "Write to", stdout) {
		fmt.Fprintln(stdout, "Not changed.")
		return exitDeclined
	}

	if werr := bobWriteKey(settingsPath, bobProxyKey, proxy); werr != nil {
		fmt.Fprintf(stderr, "agentop: %v\n", werr)
		return 1
	}

	// Deliberately not "all Bob traffic now goes through Cortex": http.proxy is what
	// VS Code's own networking and its extension host read, and it is the only
	// documented lever — but an extension bundling its own HTTP client can bypass
	// it. Claiming coverage agentop cannot deliver is how a user stops looking for the
	// real reason something is unparsed.
	fmt.Fprintf(stdout, "\nEnabled. %q in %s now points at Cortex — this is the setting\n"+
		"VS Code forks read for their own networking and their extension host.\n\n",
		bobProxyKey, settingsPath)
	// Restart, rather than a claim either way about live pickup: bobWriteKey is
	// temp+rename so Bob never sees a half-written file, but whether Bob re-reads a
	// proxy change without restarting is not something this command has verified.
	fmt.Fprint(stdout, "Restart Bob so it re-reads its settings.\n\n")
	fmt.Fprint(stdout, bobTrustNote(caPath))
	fmt.Fprintf(stdout, "Undo with: agentop configure bob disable\n")
	return 0
}

func bobDisable(settingsPath, cortexCfgPath, wantProxy, caPath string, yes bool, stdout, stderr io.Writer) int {
	if nerr := bobNoDocument(settingsPath); nerr != nil {
		return bobNotInstalled("disable", nerr, stderr)
	}

	if derr := bobDuplicateKey(settingsPath); derr != nil {
		fmt.Fprintf(stderr, "agentop: %s: %v\n", settingsPath, derr)
		return 1
	}

	doc, err := readSettings(settingsPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentop: %v\n", err)
		return 1
	}

	existing, ok := doc[bobProxyKey]
	if !ok {
		fmt.Fprintf(stdout, "Not enabled: %s sets no %q. Nothing to do.\n", settingsPath, bobProxyKey)
		return 0
	}
	s, isString := existing.(string)
	// One verdict, read three times below. bobOwns is pure and the inputs do not
	// change between those reads, so the repetition was only an invitation to let
	// two of them drift apart.
	owns := bobOwns(s, wantProxy)
	if !isString || owns == bobNotOurs {
		// Exit 0, not 1. "Remove it only if it points at Cortex" is the contract, and
		// this file already satisfies it — there is nothing for the user to fix, so
		// reporting a failure would be wrong.
		fmt.Fprintf(stdout, "%s sets %q to %v, which is not a Cortex proxy.\n"+
			"  Left alone — agentop removes only values it would have written.\n",
			settingsPath, bobProxyKey, existing)
		return 0
	}

	// A guess plus --yes is not consent. bobUnknown means the config could not be
	// read, so there was no address to compare against and all that is known is the
	// value's SHAPE: a loopback http proxy. That describes every local proxy anyone
	// runs — Squid on 3128, a corporate agent, a dev tunnel — not just Cortex's
	// 476xx block, because with no config there is no port to compare at all.
	//
	// Interactively that is survivable: the prompt prints the value and the user
	// recognizes their own proxy. --yes removes exactly that safeguard, so a
	// scripted `disable --yes` would silently delete a stranger's proxy. Refuse
	// instead, and say which flag turns the guess into an answer. Exit 1, not 0:
	// the user asked for a removal that did not happen.
	if yes && owns == bobUnknown {
		// Name the config path. Without it the message is the same whether Cortex is
		// uninstalled or --config was a typo, and only the second is worth retrying —
		// so the reader needs to see WHICH file was not read to tell them apart.
		// bobStatus's equivalent arm already names it; this one did not.
		fmt.Fprintf(stderr, "agentop: %s sets %q to %q, which is shaped like a Cortex\n"+
			"  proxy but cannot be confirmed as one: %s could not be read, so there is no\n"+
			"  address to compare against — every loopback http proxy looks like this.\n"+
			"  Refusing to delete it unattended. Re-run without --yes to see the value and\n"+
			"  decide, or pass --config with a readable Cortex config.\n",
			settingsPath, bobProxyKey, s, cortexCfgPath)
		return 1
	}

	fmt.Fprintf(stdout, "Removes from %s:\n  %q: %q\n", settingsPath, bobProxyKey, s)
	if owns == bobUnknown {
		// Say it is a guess, because it is: with no readable config there is no
		// address to compare against, and what is left is that the value is a
		// loopback http proxy — the shape agentop writes. Removing it is still the
		// right default (this is the uninstalled-Cortex case, when the off switch
		// matters most), but the user should know which of the two answers they are
		// getting, and the prompt below names the value before anything is written.
		fmt.Fprintf(stdout, "  (%s\n"+
			"   is not readable, so this is judged by shape alone — a loopback\n"+
			"   proxy, which is what agentop writes)\n", cortexCfgPath)
	}
	fmt.Fprint(stdout, bobBackupNote(settingsPath))
	if !yes && !bobConfirm(settingsPath, "Write to", stdout) {
		fmt.Fprintln(stdout, "Not changed.")
		return exitDeclined
	}

	// nil value means delete. bobWriteKey, not writeSettings: the promise printed
	// above is that nothing else in the file changes, and a re-marshal would
	// reorder and reindent every other key.
	if werr := bobWriteKey(settingsPath, bobProxyKey, nil); werr != nil {
		fmt.Fprintf(stderr, "agentop: %v\n", werr)
		return 1
	}

	fmt.Fprintf(stdout, "\nDisabled. IBM Bob no longer routes through Cortex.\n")
	fmt.Fprint(stdout, "Restart Bob so it re-reads its settings.\n\n")
	fmt.Fprint(stdout, bobUntrustNote(caPath))
	return 0
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

func bobStatus(settingsPath, cortexCfgPath, wantProxy, caPath string, stdout io.Writer) int {
	// The first line answers the question someone ran this to ask, in the words they
	// would use to ask it: is IBM Bob going through Cortex or not. Everything else is
	// detail under that, and the order is deliberate — this used to open with the raw
	// `"http.proxy"=...` key and make the reader derive the verdict from it.
	//
	// Always exit 0: "not configured" is a successful report, the same call
	// claudeCodeStatus and bobShellStatus make. A non-zero status here would make
	// `agentop configure bob status` unusable in a shell conditional for anything but
	// "is it on".
	// No document means no answer. The verdict this used to print was bobStatusNo with
	// `"http.proxy" is unset` under it — a positive report about a file that is not
	// there, indistinguishable from a real Bob that simply is not routed through Cortex.
	// Saying so is the difference between "Bob is not configured" and "agentop cannot tell
	// whether Bob is configured", and only the second is true here.
	if nerr := bobNoDocument(settingsPath); nerr != nil {
		fmt.Fprintf(stdout, "%s\n  %v\n", bobStatusUnknown, nerr)
		return 0
	}

	if derr := bobDuplicateKey(settingsPath); derr != nil {
		// Two values, and no way to say which one Bob uses without reimplementing its
		// precedence. Reporting either as "the setting" would be a guess, so the
		// verdict is "not" — this command cannot confirm the configuration — and the
		// reason is the actionable part. Still exit 0: it is a successful report.
		fmt.Fprintf(stdout, "%s\n  %s: %v\n", bobStatusNo, settingsPath, derr)
		return 0
	}

	doc, err := readSettings(settingsPath)
	if err != nil {
		// An unreadable or non-JSON settings file cannot be configured, so the verdict
		// is the same "not" — with the reason, which is the actionable part.
		fmt.Fprintf(stdout, "%s\n  %v\n", bobStatusNo, err)
		return 0
	}

	// Detail lines are collected rather than printed inline, so the verdict and any
	// WARNING can go first regardless of which branch produced the detail.
	var detail []string
	add := func(format string, args ...any) { detail = append(detail, fmt.Sprintf(format, args...)) }

	// Only a value this command recognises as the Cortex proxy is probed for liveness.
	// Probing a foreign one warns that someone's corporate proxy is down, which is
	// neither true (it is reachable from somewhere, just not here) nor any of Cortex's
	// business — and it reads as a complaint about a setting agentop deliberately leaves
	// alone. An unjudgeable loopback value IS probed: it is the shape agentop writes, and
	// disable would act on it, so its liveness is informative.
	//
	// The VERDICT is bobStateFrom's (configure_status.go), so this paragraph and the row
	// `agentop configure status` prints cannot disagree about whether Bob is routed —
	// and it is handed THE DOCUMENT ALREADY READ ABOVE rather than a path. Calling
	// bobState would have re-read the file, so a settings file edited between the two
	// reads produced a verdict contradicting the keys printed under it; that is the
	// same race claudeCodeStateFrom, codexStateFrom and openCodeStateFrom were split
	// out to avoid (#1164's review caught that bob had been left out of it).
	//
	// Only configureOn becomes bobStatusYes, so this command's wording is unchanged by
	// any of the table's finer distinctions: configurePartial is a drifted loopback
	// proxy and configureUnknown the unjudgeable-loopback case, both of which it has
	// always reported as bobStatusNo with the reason in the detail below.
	//
	// THE EARLY RETURNS ABOVE ARE THIS COMMAND'S OWN, not a shared wording. Of the
	// cases the table calls uncertain, only no-document prints bobStatusUnknown here;
	// a duplicate key and an unreadable file print bobStatusNo, and an unsupported
	// platform never reaches this function. So the two surfaces agree on the verdict
	// this line computes, and deliberately differ on how they word the file-level
	// problems each one reports first.
	verdict, listening := bobStatusNo, ""
	if bobStateFrom(settingsPath, doc, cortexCfgPath, wantProxy).State == configureOn {
		verdict = bobStatusYes
	}
	switch existing := doc[bobProxyKey].(type) {
	case nil:
		add("%q is unset in %s", bobProxyKey, settingsPath)
	case string:
		owns := bobOwns(existing, wantProxy)
		switch {
		case owns == bobOurs:
			listening = existing
			add("%q=%s in %s", bobProxyKey, existing, settingsPath)
		case owns == bobUnknown:
			listening = existing
			// The value cannot be judged without something to compare it against, and
			// claiming "not a Cortex proxy" here would be a statement about the
			// settings file resting on the absence of a config file. Report both.
			add("%q=%s in %s", bobProxyKey, existing, settingsPath)
			add("that is a loopback proxy, which is the shape agentop writes, but %s is not",
				cortexCfgPath)
			add("readable — so whether it is this machine's Cortex proxy cannot be told from here")
		case wantProxy == "":
			add("%q=%s in %s", bobProxyKey, existing, settingsPath)
			add("that is not a local proxy, and %s is not readable", cortexCfgPath)
		case bobIsLoopbackProxy(existing):
			// Drift: a loopback proxy that is not the one the config names now. Almost
			// always a Cortex address from before the port moved, which is worth
			// naming as such — but it is NOT ours by the ownership test, so disable
			// leaves it alone. Reporting drift is useful; deleting on a guess is not.
			add("%q=%s in %s", bobProxyKey, existing, settingsPath)
			add("that is a local proxy, but %s now names %s", cortexCfgPath, wantProxy)
			// NOT "run enable to move it", which is what this line said and what no
			// user could do: ownership is an exact host+port match, so enable sees a
			// drifted port as bobNotOurs and refuses it with exit 1 — the one arm that
			// falls through to the write is a differing SPELLING of the same address
			// (localhost vs 127.0.0.1), not a different port. Both halves of that
			// refusal are deliberate, so the advice is what changes: name the two
			// steps that do work, in the order they work in.
			add("to move it: change that value to %s by hand, or remove it and run", wantProxy)
			add("`agentop configure bob enable`")
		default:
			add("%q=%s in %s", bobProxyKey, existing, settingsPath)
			add("that is not this machine's Cortex proxy, which %s puts at %s",
				cortexCfgPath, wantProxy)
		}
	default:
		add("%q=%v in %s", bobProxyKey, existing, settingsPath)
		add("that value is a %T, not a string", existing)
	}

	fmt.Fprintln(stdout, verdict)

	// Second line, when it applies. Liveness is a separate axis from ownership, and
	// this is a WARNING rather than part of the verdict because the setting is correct
	// either way: Cortex being stopped is the normal state of a laptop, and the answer
	// is `agentop service start`, not an edit to Bob's settings.
	if listening != "" && !bobProxyIsListening(listening) {
		fmt.Fprintf(stdout, "WARNING: No proxy is listening at %s\n", listening)
	}

	for _, line := range detail {
		fmt.Fprintf(stdout, "  %s\n", line)
	}

	if note := bobVerifyNote(caPath); note != "" {
		fmt.Fprint(stdout, "\n"+note)
	}
	return 0
}
