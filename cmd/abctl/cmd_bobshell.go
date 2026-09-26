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

const bobShellUsage = `abctl configure bobshell — run Bob through Cortex by typing "bob"

Usage:
  abctl configure bobshell enable  [--yes]
  abctl configure bobshell disable [--yes]
  abctl configure bobshell status

Flags:
  --yes           do not prompt for confirmation

enable appends a block to your shell's rc file defining a "bob" shell function
that runs "abctl exec -- bob", and exporting ` + bobShellEnvVar + `=1. Open a new
terminal, or source the file, for it to take effect. disable removes exactly that
block. Neither touches anything else in the file. Both ask before writing; --yes
skips the question, and with no terminal to ask on they write nothing and say so.

Which file: the basename of $SHELL picks it — zsh gets ~/.zshrc, bash gets
~/.bashrc. Any other shell gets the block printed for you to place yourself,
because where it belongs is a question about your shell that abctl does not try
to answer.

Note for bash on macOS: Terminal.app starts bash as a LOGIN shell, which reads
~/.bash_profile and not ~/.bashrc. If the function does not appear in a new
window, source ~/.bashrc from ~/.bash_profile — the usual arrangement — or move
the block there.

status reports whether bob is routed through Cortex in THIS shell, by looking for
` + bobShellEnvVar + ` in the environment. It says "not enabled" in the very shell
that just ran enable, until you source the file or open a new terminal. It reads
no files: recognising the block inside a startup script means parsing shell, and a
variable the shell itself exported is the more honest answer.

Exit status: 0 applied, already correct, or advice printed, 1 something went
wrong, 2 a usage error.
`

const (
	// The markers delimit the block so disable can find it, and are the reason
	// enable can be run twice without appending a second copy.
	bobShellMarkerStart = "# >>> cortex abctl (bobshell) >>>"
	bobShellMarkerEnd   = "# <<< cortex abctl (bobshell) <<<"
	bobShellEnvVar      = "CORTEX_BOBSHELL"
)

// bobShellBlock is what enable writes and what disable removes — ONE constant, so
// the two cannot disagree. A previous attempt at this feature carried a version
// string inside the block and built the two ends separately; they drifted, and
// disable stopped matching what enable had written.
//
// A function rather than an alias: bash does not expand aliases in
// non-interactive shells unless expand_aliases is set, and "$@" forwards
// arguments explicitly rather than relying on textual substitution.
//
// It needs no guard against recursing into itself. "abctl exec" runs the bob
// binary as a child process, and that process does not read this file, so the
// function does not exist on the other side of it — the "bob" inside the body
// resolves to the PATH binary. Verified by hand under zsh, bash and dash: the
// binary is reached exactly once. Deliberately no "whence -p" / "type -P" to
// find that binary past the function: those are shell-specific builtins, the
// portable-looking fallbacks are not portable, and the whole apparatus guards a
// loop that cannot happen.
//
// Every byte here is fixed — no interpolation, nothing machine-specific — which
// is what lets disable be a string comparison rather than a parser.
//
// It OPENS with a newline, so the block is self-separating: appended to a file
// whose last line has no newline of its own, it still starts on a line of its
// own, and the separator is inside the one string disable removes. Keeping the
// separator outside — appending it to the user's content — is what made the round
// trip lossy: once written, "vim\n" + block and "vim" + "\n" + block are the same
// bytes, so disable could not tell whose newline it was, and either left ours
// behind or ate theirs. Inside the constant there is nothing to tell apart.
const bobShellBlock = "\n" + bobShellMarkerStart + `
bob() {
  abctl exec -- bob "$@"
}
export ` + bobShellEnvVar + `=1
` + bobShellMarkerEnd + "\n"

// rcFileMode is the permission an rc file gets when enable has to create one.
// 0644, not 0600: rc files are conventionally world-readable, and silently
// tightening the mode of a file the user may share across a dotfiles setup is
// not something they asked for. An existing file keeps its own mode.
const rcFileMode os.FileMode = 0o644

// maxRCSymlinkHops is how many links deep enable and disable will follow.
//
// One hop is the dotfiles case — ~/.zshrc symlinked into a tracked repo — and it
// must be followed rather than written over: os.Rename replaces the LINK, which
// would turn it into a regular file and silently detach it from the repo, the
// tracked copy keeping the old contents with nothing in "git status" to show it.
// Past one hop, the chain is someone's deliberate arrangement and a write is more
// likely to surprise them than to help, so we print instead.
const maxRCSymlinkHops = 1

func runBobShell(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, bobShellUsage)
		return 2
	}
	action := args[0]
	// Answered before anything else, and before any path resolution: --help asks
	// for this command's usage, and reading it as an action name would send
	// someone looking for the command list to the one branch that refuses to
	// print it. Same split, and same reason, as claude-code's.
	switch action {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, bobShellUsage)
		return 0
	}

	// The verb is validated HERE, before any environment or filesystem work,
	// because three of the steps below answer successfully on their own: an
	// unrecognised $SHELL, a dangling rc symlink, and a chain past the hop limit
	// each print the block and return 0. Validating the action last meant
	// `bobshell enabel` under fish printed the block and exited 0 — reporting
	// success for a verb that does not exist. Nothing downstream can reach this
	// check, so it has to come first.
	switch action {
	case "enable", "disable", "status":
	default:
		fmt.Fprintf(stderr, "abctl: unknown bobshell action %q (enable, disable, status)\n", action)
		return 2
	}

	// These verbs take no operands, and anything after them is a misunderstanding
	// that must not be silently dropped. A FlagSet with ContinueOnError, the same
	// shape claude-code builds, both rejects an unrecognised flag and gives -h its
	// own usage — so `disable --help` prints help instead of deleting the block,
	// and `enable --dry-run` is refused instead of enabling for real.
	fs := flag.NewFlagSet("configure bobshell "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	// --yes for parity with `configure claude-code`, whose enable and disable both
	// prompt before writing and take --yes to skip it. enable and disable here edit
	// a startup file, which is the same class of change, and review found the
	// asymmetry: this command had no flags at all, so there was no way to say yes
	// in advance and nothing to pass from a script or an installer.
	yes := fs.Bool("yes", false, "do not prompt for confirmation")
	// The FlagSet's own usage would print a bare header and a one-flag list.
	// Printing this command's usage instead is the useful answer, and suppressing
	// it here keeps -h's single copy on stdout below.
	fs.Usage = func() {}
	if err := fs.Parse(args[1:]); err != nil {
		// -h and --help arrive here as flag.ErrHelp, and asking for help is not a
		// usage error: answer on stdout and exit 0, the same split the action-level
		// help above uses. Any other parse failure is a real usage error, and
		// Parse has already named it on stderr.
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, bobShellUsage)
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "abctl: bobshell %s takes no arguments (got %q)\n", action, fs.Arg(0))
		return 2
	}

	// status answers from the environment alone, so it needs no home directory,
	// no rc path and no file. Dispatched before all of that rather than after, so
	// a user whose $SHELL is unrecognised can still ask.
	if action == "status" {
		return bobShellStatus(stdout)
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		fmt.Fprintf(stderr, "abctl: cannot determine your home directory: %v\n", err)
		return 1
	}

	path, err := bobShellRCPath(os.Getenv("SHELL"), home)
	if err != nil {
		// Not an error exit: we know what the user wanted and we can still tell
		// them how to get it. What "it" is depends on the verb, which is why the
		// advice is a function taking the action rather than three inlined copies
		// of enable's answer — see bobShellAdviseManual.
		fmt.Fprintf(stdout, "abctl: %v\n\n", err)
		return bobShellAdviseManual(action, "whichever file your shell reads at startup", stdout)
	}

	// Resolved ONCE, here, above both verbs — so the path that gets checked is
	// the path that gets written. Doing this inside each verb is how the previous
	// attempt ended up guarding the pre-resolution path while writing the
	// post-resolution one.
	target, hops, err := rcTarget(path)
	// An absent file at hop 0 is the normal first-run case: enable creates it. The
	// same os.ErrNotExist one hop in is a DANGLING LINK, which is not normal and
	// must not be followed — creating the missing target would quietly repair
	// someone's broken link into a file their shell may not read, and writing
	// through it is exactly the surprise the hop limit exists to avoid. The error
	// value is identical in both cases; only the hop count separates them.
	if err != nil && !(hops == 0 && errors.Is(err, os.ErrNotExist)) {
		fmt.Fprintf(stdout, "abctl: cannot follow %s: %v\n\n", path, err)
		fmt.Fprint(stdout, "Sort the link out, or do it by hand.\n\n")
		return bobShellAdviseManual(action, "the real file", stdout)
	}
	if hops > maxRCSymlinkHops {
		fmt.Fprintf(stdout, "abctl: %s is %d symlinks deep (ending at %s).\n\n", path, hops, target)
		fmt.Fprint(stdout, "That is deliberate enough that abctl will not write through it.\n\n")
		return bobShellAdviseManual(action, "the real file", stdout)
	}

	// Only enable and disable reach here: help and status returned above, and any
	// other verb was refused before the home directory was read.
	if action == "enable" {
		return bobShellEnable(target, *yes, stdout, stderr)
	}
	return bobShellDisable(target, *yes, stdout, stderr)
}

// bobShellRCPath maps the shell's basename to the rc file to edit.
//
// shell and home are parameters rather than read from the environment so the
// mapping can be tested without a process environment, and so runBobShell reads
// each of them exactly once.
//
// Deliberately two cases and an error. Not consulted: whether the shell will be
// a login or a non-login shell (which decides between .bashrc and .bash_profile,
// differently on macOS and Linux), .zshenv / .zprofile / .zlogin, or /etc/passwd
// when $SHELL is unset. Every one of those is a claim about what the user's shell
// does at startup, and being wrong about it writes a block into a file nothing
// reads. An unrecognised shell gets the block printed, which is correct however
// their startup is arranged.
func bobShellRCPath(shell, home string) (string, error) {
	if shell == "" {
		return "", errors.New("$SHELL is not set, so I cannot tell which startup file to edit")
	}
	switch filepath.Base(shell) {
	case "zsh":
		return filepath.Join(home, ".zshrc"), nil
	case "bash":
		return filepath.Join(home, ".bashrc"), nil
	default:
		return "", fmt.Errorf("I only know where zsh and bash read their startup files, and $SHELL is %s", shell)
	}
}

// rcTarget follows a symlink chain and reports where it ends and how many hops it
// took. A path that is not a link is 0 hops and itself.
//
// os.Readlink in a loop rather than filepath.EvalSymlinks, which answers a
// different question: it returns only the destination, so the caller cannot tell
// one hop from five, and it fails outright on a dangling link instead of
// reporting the link it could not follow. Both distinctions are decisions this
// command makes.
//
// A missing file is not an error for the caller's purposes — enable creates one —
// so os.ErrNotExist comes back distinguishable via errors.Is. The HOP COUNT is what
// separates the two ways that error arrives: 0 hops means the rc file itself is
// absent (normal), while 1 or more means a link pointed at something missing (a
// dangling link, which the caller refuses). Returning both is the reason this
// reports hops even alongside an error.
func rcTarget(path string) (string, int, error) {
	current := path
	for hops := 0; ; hops++ {
		fi, err := os.Lstat(current)
		if err != nil {
			return current, hops, err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			return current, hops, nil
		}
		// Stop counting once past the limit. The caller only needs to know the
		// chain is too long, and a loop of links would otherwise spin here.
		if hops >= maxRCSymlinkHops {
			return current, hops + 1, nil
		}
		dest, err := os.Readlink(current)
		if err != nil {
			return current, hops, err
		}
		// A relative link resolves against the directory holding the link, not
		// the working directory.
		if !filepath.IsAbs(dest) {
			dest = filepath.Join(filepath.Dir(current), dest)
		}
		current = dest
	}
}

func bobShellEnable(path string, yes bool, stdout, stderr io.Writer) int {
	content, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stderr, "abctl: read %s: %v\n", path, err)
		return 1
	}

	if strings.Contains(string(content), bobShellBlock) {
		fmt.Fprintf(stdout, "Already enabled in %s. Nothing to do.\n", path)
		return 0
	}
	// Our markers are there but the block is not what we write. Hand-edited, or
	// written by a different version. Appending would leave two competing
	// definitions with the last one winning, which is worse than declining.
	if strings.Contains(string(content), bobShellMarkerStart) {
		fmt.Fprintf(stdout, "%s already has a cortex bobshell block, but not one matching what this abctl writes.\n", path)
		fmt.Fprint(stdout, "Left alone. Remove it by hand and re-run enable, or keep what you have.\n")
		return 0
	}

	// A plain append, with no newline fix-up of the user's content: the block opens
	// with its own newline, so it separates itself from whatever precedes it and
	// disable removes that byte along with the rest. Adjusting `out` here instead
	// put the separator outside the string disable searches for, and it survived
	// every disable — see bobShellBlock's comment and
	// TestBobShellRoundTripIsByteIdentical's no-trailing-newline rows.
	out := string(content) + bobShellBlock

	// Prompted HERE, not at the top of the verb: the two branches above answer
	// "already enabled" and "there is a block I do not recognise" without writing
	// anything, and asking permission to do nothing trains people to stop reading
	// the question. Past this point a write is certain, so this is the last moment
	// that is still honest.
	if !yes && !bobShellConfirm(path, "Add the cortex bobshell block to", stdout) {
		return 0
	}

	if err := writeRCFile(path, out); err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Enabled in %s.\n", path)
	// shellQuote, because this line is meant to be COPIED AND RUN, unlike the
	// prose above that merely names the path. An unquoted "source /Users/a b/.zshrc"
	// splits at the space and sources /Users/a, so the rc file is written correctly
	// and the instructions for loading it are broken — the worse of the two failures
	// to have, since the user sees "Enabled" and then a command that does not work.
	// cmd_exec.go's helper already handles the embedded-single-quote case.
	fmt.Fprintf(stdout, "\nOpen a new terminal, or run:\n  source %s\n", shellQuote(path))
	fmt.Fprint(stdout, "\nThen \"bob\" runs through Cortex. \"abctl configure bobshell disable\" is the off switch.\n")
	return 0
}

func bobShellDisable(path string, yes bool, stdout, stderr io.Writer) int {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stdout, "Not enabled in %s (no such file). Nothing to do.\n", path)
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "abctl: read %s: %v\n", path, err)
		return 1
	}

	// Counting rather than testing presence: every count but 1 is a case where
	// removing one copy would leave the file in a state nobody chose.
	switch n := strings.Count(string(content), bobShellBlock); {
	case n == 1:
		// One Replace of the whole block, leading newline included, so it is the
		// exact inverse of enable's append and the file comes back byte-identical.
		out := strings.Replace(string(content), bobShellBlock, "", 1)
		// Only this branch writes. The other three report and return 0, so the
		// prompt lives here rather than above the switch.
		if !yes && !bobShellConfirm(path, "Remove the cortex bobshell block from", stdout) {
			return 0
		}
		if err := writeRCFile(path, out); err != nil {
			fmt.Fprintf(stderr, "abctl: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "Disabled: removed the cortex bobshell block from %s.\n", path)
		fmt.Fprintf(stdout, "\n\"bob\" keeps working in shells already open. Open a new terminal to be rid of it.\n")
		return 0
	case n > 1:
		// Removing one of N is a silent half-measure, and removing all N assumes
		// the extras are ours to delete.
		fmt.Fprintf(stdout, "%s has %d copies of the cortex bobshell block.\n", path, n)
		fmt.Fprint(stdout, "Left alone — remove them by hand, between the marker lines:\n\n")
		fmt.Fprintf(stdout, "  %s\n  ...\n  %s\n", bobShellMarkerStart, bobShellMarkerEnd)
		return 0
	case strings.Contains(string(content), bobShellMarkerStart):
		// The markers are there but the content between them is not ours.
		// Deleting a hand-edited block is not what "remove exactly what enable
		// added" means.
		fmt.Fprintf(stdout, "%s has a cortex bobshell block that has been edited since it was written.\n", path)
		fmt.Fprint(stdout, "Left alone — remove it by hand if you want it gone, between the marker lines:\n\n")
		fmt.Fprintf(stdout, "  %s\n  ...\n  %s\n", bobShellMarkerStart, bobShellMarkerEnd)
		return 0
	default:
		fmt.Fprintf(stdout, "Not enabled in %s. Nothing to do.\n", path)
		return 0
	}
}

// bobShellAdviseManual prints what the user has to do by hand when abctl will not
// touch the file itself — an unrecognised $SHELL, a dangling rc symlink, or a chain
// past the hop limit. Returns 0: we could not do it for them, but we answered the
// question they asked, and that is a success.
//
// It takes the action because the three callers sit ABOVE the enable/disable
// dispatch — they run before either verb does, so they cannot tell the verbs apart
// on their own. That is exactly what went wrong when each of them inlined
// enable's answer: `configure bobshell disable` under fish printed "add this to
// your startup file" and the whole block, telling someone trying to REMOVE the
// integration to paste it in. Branching here rather than duplicating the arms
// keeps the two answers next to each other, where a future third fallback picks
// both up for free.
//
// where names the file in the caller's own words ("the real file", "whichever file
// your shell reads at startup"), because only the caller knows why it is giving up.
func bobShellAdviseManual(action, where string, stdout io.Writer) int {
	if action == "disable" {
		// No block printed: a user removing the integration has the text already —
		// it is in their file — and printing it again is at best noise and at worst
		// read as an instruction to add it. The markers are what they need, since
		// that is what they are searching the file for.
		fmt.Fprintf(stdout, "To remove it by hand, delete the block between %s\nand %s from %s.\n",
			bobShellMarkerStart, bobShellMarkerEnd, where)
		return 0
	}
	fmt.Fprintf(stdout, "Add this to %s:\n\n", where)
	fmt.Fprint(stdout, bobShellBlock)
	return 0
}

// bobShellStatus reports whether this shell routes bob through Cortex.
//
// The environment, and nothing else. Exits 0 either way: "not enabled" is a
// successful report, not a failure, which is how claudeCodeStatus behaves and
// what makes this usable in a script that only wants the text.
func bobShellStatus(stdout io.Writer) int {
	if v, ok := os.LookupEnv(bobShellEnvVar); ok {
		fmt.Fprintf(stdout, "  %s=%s\n", bobShellEnvVar, v)
		// Two lines, because these are two different facts and the variable only
		// establishes the first. It is exported, so it is inherited by every child
		// process — including a NON-INTERACTIVE subshell, which does not read the rc
		// file and therefore has no bob function at all. There, `bob` is the PATH
		// binary and Cortex is not in the path of the call, while the variable still
		// says 1. Claiming "bob runs through Cortex" on the strength of an inherited
		// variable is a claim this command cannot check: the function table lives in
		// the shell's own memory and is never exported, so abctl, as a child process,
		// cannot see it. mise reports the same split as separate `activated:` and
		// `shims_on_path:` lines for the same reason.
		fmt.Fprint(stdout, "configured — a shell that reads your startup file defines \"bob\"\n")
		// "type bob", NOT "which bob". In bash `which` is /usr/bin/which, a separate
		// process, and a child cannot see its parent's function table — so with the
		// function live and a bob binary on PATH, bash's `which bob` prints the
		// BINARY'S PATH. Read by the old rule ("a path means no") that is a false
		// negative in exactly the case this check exists to find, and it reads as
		// authoritative. zsh's `which` is a builtin and does report the function,
		// which is why this survived review: it is right in one of the two shells we
		// write a file for. `type` is a POSIX shell builtin — verified as a builtin in
		// sh, bash, zsh and dash — so it sees the function in all of them.
		fmt.Fprint(stdout, "\nIn an interactive shell that is the function, so \"bob\" runs through Cortex.\nTo confirm it in THIS shell: run \"type bob\" — it says \"bob is a function\" if Cortex\nis in the path of the call, and names a file if it is not (a script or\nnon-interactive subshell inherits the variable but not the function).\n")
		return 0
	}
	fmt.Fprintf(stdout, "  %s (unset)\n", bobShellEnvVar)
	fmt.Fprint(stdout, "not enabled in this shell\n")
	fmt.Fprint(stdout, "\nIf you have just run enable, this shell has not read the file yet — open a new\nterminal or source it. Otherwise: abctl configure bobshell enable\n")
	return 0
}

// bobShellConfirm names the file, then asks, reusing claude-code's confirm.
//
// The file is named on its own line first because the prompt itself cannot say
// which file it means: confirm's question is the fixed "Apply? [y/N]", shared
// with claude-code, and the whole point of the question here is WHICH startup
// file is about to change — $SHELL picked it, not the user.
//
// confirm reads /dev/tty rather than stdin and declines when there is no
// terminal, printing "re-run with --yes". That is the behaviour we want: in CI or
// a container this writes nothing and exits 0, the same "advice printed, nothing
// applied" outcome as an unrecognised $SHELL.
func bobShellConfirm(path, what string, stdout io.Writer) bool {
	fmt.Fprintf(stdout, "%s %s\n", what, path)
	return confirm(stdout)
}

// writeRCFile replaces path's contents atomically, keeping its permissions.
//
// Temp-then-rename, the same discipline as edit/local.go's Apply: a truncate and
// rewrite that fails partway leaves the user with a half-written startup file,
// and a broken .zshrc is a shell that does not start properly. rename(2) is
// atomic within a filesystem, so the file is either the old one or the new one —
// which requires the temp to be a sibling, since a cross-filesystem rename fails
// EXDEV.
//
// os.CreateTemp rather than a fixed path+".tmp" with O_EXCL: debris from a
// crashed run would wedge every later write, and the failure would look like a
// permissions problem rather than a leftover file (userconfig.go:116-127 makes
// the same argument).
//
// path is expected to be already symlink-resolved by the caller.
func writeRCFile(path, content string) error {
	dir := filepath.Dir(path)

	// Inherit the existing mode rather than assuming one, so a user who
	// deliberately locked their rc file down keeps it that way.
	mode := rcFileMode
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}

	tmp, err := os.CreateTemp(dir, ".abctl-bobshell-*")
	if err != nil {
		return fmt.Errorf("create temp beside %s: %w", path, err)
	}
	tmpName := tmp.Name()
	// Every failure below leaves the original untouched and takes the temp with
	// it. A stray dotfile in the user's home directory is litter we created.
	defer func() {
		if tmpName != "" {
			os.Remove(tmpName)
		}
	}()

	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp: %w", err)
	}
	// Sync before rename: rename orders the directory entry, not the data, so a
	// crash between the two could publish a file whose contents never landed.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	tmpName = "" // renamed away; nothing to clean up
	return nil
}
