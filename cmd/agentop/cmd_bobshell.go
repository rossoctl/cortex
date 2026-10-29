package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// The shell half of `agentop configure bob`: the "bob" function block in the
// shell's rc file. Until 2026-10-15 this was its own subcommand with its own
// flag parsing and its own prompt; it is now a PLAN the merged command computes
// and shows under its single confirmation (see runBob), plus the by-hand advice
// arms for the arrangements agentop will not write through. The one caller that
// still wants the half on its own is `agentop uninstall`, which reaches
// bobShellDisable with --yes and captures the output.

const (
	// The markers delimit the block so disable can find it, and are the reason
	// enable can be run twice without appending a second copy.
	bobShellMarkerStart = "# >>> cortex agentop (bobshell) >>>"
	bobShellMarkerEnd   = "# <<< cortex agentop (bobshell) <<<"
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
// It needs no guard against recursing into itself. "agentop exec" runs the bob
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
  agentop exec -- bob "$@"
}
export ` + bobShellEnvVar + `=1
` + bobShellMarkerEnd + "\n"

// rcBlock is one fixed block agentop owns in a shell's rc file: its text and the
// marker lines that delimit it. name is how messages refer to it ("bobshell", "pi").
// The plan, apply, disable and advice functions below take one, so the bob shell
// function and the pi one share every rule about the file instead of copying them.
type rcBlock struct{ name, start, end, text string }

var bobShellRC = rcBlock{"bobshell", bobShellMarkerStart, bobShellMarkerEnd, bobShellBlock}

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

// rcResolve picks the rc file for $SHELL and applies the link rules both
// halves of `configure bob` share, so they always pick the same file and refuse
// the same arrangements.
//
// Returns the symlink-resolved path to write; an "advise by hand" reason (an
// unrecognised $SHELL, a dangling rc symlink, a chain past the hop limit) which
// is advice and not failure — the caller prints the block for the user to place;
// and a hard readErr when agentop cannot so much as look (EACCES, EIO), which
// the callers exit 1 on.
//
// An absent file at hop 0 is the normal first-run case and NOT an error of
// either kind: enable creates it. The same os.ErrNotExist one hop in is a
// DANGLING LINK, which is not normal and must not be followed — creating the
// missing target would quietly repair someone's broken link into a file their
// shell may not read, and writing through it is exactly the surprise the hop
// limit exists to avoid. The error value is identical in both cases; only the
// hop count separates them, which is why rcTarget reports hops even alongside
// an error.
func rcResolve(home, shell string) (path string, advise, readErr error) {
	path, err := rcPathForShell(shell, home)
	if err != nil {
		return "", err, nil
	}
	target, hops, err := rcTarget(path)
	switch {
	case err != nil && !(hops == 0 && errors.Is(err, os.ErrNotExist)):
		// A DANGLING LINK is advice: the arrangement is the user's, agentop can
		// describe it exactly, and printing the block is a useful answer. Anything
		// else here is agentop failing to look, not a link needing sorting out.
		// Separated by the error value, not the hop count, because a dangling link
		// at hop 1 and an unreadable file at hop 0 both arrive here.
		//
		// ELOOP is deliberately NOT in this branch and cannot reach it: rcTarget
		// caps its walk at maxRCSymlinkHops, so a link cycle returns hops > limit
		// with a nil error and takes the "deeper than" path below. Pinned by
		// TestBobShellDeclinesExactlyTwoHops/self-referential_link, which reaches
		// that path and not this one.
		if !errors.Is(err, os.ErrNotExist) {
			return path, nil, fmt.Errorf("cannot read %s: %w", path, err)
		}
		return path, fmt.Errorf("cannot follow %s: %w", path, err), nil
	case hops > maxRCSymlinkHops:
		// "deeper than", and no endpoint named: rcTarget stops walking at the
		// limit, so hops is a floor and target is the link it stopped ON, not the
		// end of the chain. The limit is stated instead of the count, because the
		// limit is the fact agentop actually knows.
		return path, fmt.Errorf("%s is a symlink chain deeper than the %d agentop will follow", path, maxRCSymlinkHops), nil
	}
	return target, nil, nil
}

// rcPathForShell maps the shell's basename to the rc file to edit.
//
// shell and home are parameters rather than read from the environment so the
// mapping can be tested without a process environment, and so the callers read
// each of them exactly once.
//
// Deliberately two cases and an error. Not consulted: whether the shell will be
// a login or a non-login shell (which decides between .bashrc and .bash_profile,
// differently on macOS and Linux), .zshenv / .zprofile / .zlogin, or /etc/passwd
// when $SHELL is unset. Every one of those is a claim about what the user's shell
// does at startup, and being wrong about it writes a block into a file nothing
// reads. An unrecognised shell gets the block printed, which is correct however
// their startup is arranged.
func rcPathForShell(shell, home string) (string, error) {
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

// rcTarget follows a symlink chain and reports where it ends and how many hops
// it took. A path that is not a link is 0 hops and itself.
//
// os.Readlink in a loop rather than filepath.EvalSymlinks, which answers a
// different question: it returns only the destination, so the caller cannot tell
// one hop from five, and it fails outright on a dangling link instead of
// reporting the link it could not follow. Both distinctions are decisions this
// command makes.
//
// A missing file is not an error for the caller's purposes — enable creates one —
// so os.ErrNotExist comes back distinguishable via errors.Is. The HOP COUNT is
// what separates the two ways that error arrives: 0 hops means the rc file itself
// is absent (normal), while 1 or more means a link pointed at something missing
// (a dangling link, which the caller refuses). Returning both is the reason this
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
		// chain is too long, and a loop of links would otherwise spin here — which
		// is the half of this line that no assertion can catch, since its failure
		// mode is a hang rather than a wrong answer. Pinned by
		// TestBobShellDeclinesExactlyTwoHops/self-referential_link: delete these
		// three lines and that subtest stops the package at its deadline.
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

// bobShellPlan is the shell half's whole answer to "what would change", computed
// BEFORE anything is written so the merged command can show it under one
// confirmation.
type rcPlan struct {
	// blk is the block this plan adds or removes.
	blk rcBlock
	// path is the rc file, symlink-resolved.
	path string
	// write is whether there is a change to make at all.
	write bool
	// remove is the verb: false appends the block, true removes it.
	remove bool
	// already is the requested state already holding — nothing to do, and not a
	// finding either.
	already bool
	// note is what the half says when it will not write. Two kinds, told apart
	// by already: a FINDING (already false) — an unrecognised or duplicated
	// block, left alone with the reason, which the merged command must show
	// beside any prompt, because it is the one line that says the rc file was
	// left alone — and PLAIN ABSENCE (already true) — "not enabled, nothing to
	// do", which belongs in the nothing-to-do arm alone: with the other half
	// changing the same file, printing "nothing to do" above an Apply? prompt
	// contradicts the preview under it.
	note string
}

// leftAlone is whether the note is a finding: a block agentop declined to
// touch, as opposed to the requested state simply holding already. The merged
// command's preview and report key on this, not on note alone, because both
// kinds of note exist and only one of them narrows a headline.
func (p rcPlan) leftAlone() bool {
	return p.note != "" && !p.already
}

func bobShellEnablePlan(path string) (rcPlan, error) { return rcEnablePlan(bobShellRC, path) }

// rcEnablePlan computes what enable would do to the rc file: append the
// block, or nothing. The error is a failure to read the file, which is exit 1 —
// nothing below can say whether the block is already there.
func rcEnablePlan(blk rcBlock, path string) (rcPlan, error) {
	content, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return rcPlan{}, err
	}

	if strings.Contains(string(content), blk.text) {
		return rcPlan{blk: blk, path: path, already: true}, nil
	}
	// Our markers are there but the block is not what we write. Hand-edited, or
	// written by a different version. Appending would leave two competing
	// definitions with the last one winning, which is worse than declining.
	if strings.Contains(string(content), blk.start) {
		return rcPlan{blk: blk, path: path, note: fmt.Sprintf(
			"%s already has a cortex %s block, but not one matching what this agentop\n"+
				"  writes. Left alone — remove it by hand and re-run enable, or keep what you have.",
			path, blk.name)}, nil
	}

	// A plain append, with no newline fix-up of the user's content: the block opens
	// with its own newline, so it separates itself from whatever precedes it and
	// disable removes that byte along with the rest. Adjusting the content here
	// instead put the separator outside the string disable searches for, and it
	// survived every disable — see bobShellBlock's comment and
	// TestBobShellRoundTripIsByteIdentical's no-trailing-newline rows.
	//
	// The append itself happens in apply, against the file as it is THEN: both
	// halves of the merged command write this same file on macOS, and a
	// whole-file write from this pre-prompt read would drop whatever the other
	// half wrote since.
	return rcPlan{blk: blk, path: path, write: true}, nil
}

func bobShellDisablePlan(path string) (rcPlan, error) { return rcDisablePlan(bobShellRC, path) }

// rcDisablePlan computes what disable would do to the rc file: remove the
// block, or nothing. The error is a failure to read the file; a file that does
// not exist holds nothing of ours, which is the already state — reported, not
// failed, and not a finding either.
func rcDisablePlan(blk rcBlock, path string) (rcPlan, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return rcPlan{blk: blk, path: path, already: true, note: fmt.Sprintf("Not enabled in %s (no such file). Nothing to do.", path)}, nil
	}
	if err != nil {
		return rcPlan{}, err
	}

	// Counting rather than testing presence: every count but 1 is a case where
	// removing one copy would leave the file in a state nobody chose.
	switch n := strings.Count(string(content), blk.text); {
	case n == 1:
		// One Replace of the whole block, leading newline included, so it is the
		// exact inverse of enable's append and the file comes back byte-identical.
		// The replace itself happens in apply, against the file as it is then —
		// see bobShellEnablePlan for why.
		return rcPlan{blk: blk, path: path, write: true, remove: true}, nil
	case n > 1:
		// Removing one of N is a silent half-measure, and removing all N assumes
		// the extras are ours to delete.
		return rcPlan{blk: blk, path: path, note: fmt.Sprintf(
			"%s has %d copies of the cortex %s block. Left alone — remove them\n"+
				"  by hand, between the marker lines:\n\n  %s\n  ...\n  %s",
			path, n, blk.name, blk.start, blk.end)}, nil
	case strings.Contains(string(content), blk.start):
		// The markers are there but the content between them is not ours.
		// Deleting a hand-edited block is not what "remove exactly what enable
		// added" means.
		return rcPlan{blk: blk, path: path, note: fmt.Sprintf(
			"%s has a cortex %s block that has been edited since it was written.\n"+
				"  Left alone — remove it by hand if you want it gone, between the marker lines:\n"+
				"\n  %s\n  ...\n  %s",
			path, blk.name, blk.start, blk.end)}, nil
	}
	// Plain absence, same as no file at all: the requested state holds, so the
	// note is for the nothing-to-do arm — not a finding (see bobShellPlan.note).
	return rcPlan{blk: blk, path: path, already: true, note: fmt.Sprintf("Not enabled in %s. Nothing to do.", path)}, nil
}

// apply writes the plan's change to the file as it is NOW, not as the plan read
// it. Both halves of the merged command write the same rc file on macOS, so a
// whole-file write from the pre-prompt read would drop whatever the other half
// wrote since; the transforms are append-or-remove of one fixed block, so a
// fresh read keeps both halves' changes and stays the exact inverse of the
// other verb. Returns 0 or 1; the prompt is the caller's, so a decline never
// reaches here.
func (p rcPlan) apply(stderr io.Writer) int {
	if !p.write {
		return 0
	}
	content, err := os.ReadFile(p.path)
	if err != nil && !(errors.Is(err, os.ErrNotExist) && !p.remove) {
		fmt.Fprintf(stderr, "agentop: read %s: %v\n", p.path, err)
		return 1
	}
	out := string(content) + p.blk.text
	if p.remove {
		out = strings.Replace(string(content), p.blk.text, "", 1)
	}
	if err := writeRCFile(p.path, out); err != nil {
		fmt.Fprintf(stderr, "agentop: %v\n", err)
		return 1
	}
	return 0
}

// bobShellDisable removes the block, prompting unless yes. The merged
// `configure bob disable` does NOT go through here — it shows the shell half's
// plan under its own single prompt — but `agentop uninstall` does, with yes set
// and the output captured, so the half stays callable on its own terms.
func bobShellDisable(path string, yes bool, stdout, stderr io.Writer) int {
	return rcDisable(bobShellRC, path, yes, stdout, stderr)
}

// rcDisable is bobShellDisable for any block: what `agentop uninstall` runs for
// each shell function it removes.
func rcDisable(blk rcBlock, path string, yes bool, stdout, stderr io.Writer) int {
	p, err := rcDisablePlan(blk, path)
	if err != nil {
		fmt.Fprintf(stderr, "agentop: read %s: %v\n", path, err)
		return 1
	}
	if !p.write {
		fmt.Fprintln(stdout, p.note)
		return 0
	}
	// exitDeclined on a refusal, and this is the ONLY arm of this function that
	// returns it: "already enabled" and "there is a block I do not recognise" are
	// agentop's own answers, not the user's refusal. That is exactly the
	// distinction exitDeclined was added to carry — see its comment in
	// cmd_claudecode.go.
	if !yes && !bobShellConfirm(path, "Remove the cortex "+blk.name+" block from", stdout) {
		return exitDeclined
	}
	if code := p.apply(stderr); code != 0 {
		return code
	}
	fmt.Fprintf(stdout, "Disabled: removed the cortex %s block from %s.\n", blk.name, path)
	return 0
}

// bobShellConfirm names the file, then asks, reusing the shared confirm.
//
// The file is named on its own line first because the prompt itself cannot say
// which file it means: confirm's question is the fixed "Apply? [y/N]", and the
// whole point of the question here is WHICH startup file is about to change —
// $SHELL picked it, not the user.
//
// confirm reads /dev/tty rather than stdin, so it declines only where there is
// no terminal to open. Where a terminal DOES exist it blocks for an answer, and
// redirecting stdin cannot change that; that is why this is a var rather than a
// plain func. A test that wants the declined path has to substitute it, because
// `go test` inherits the terminal it was launched from and would otherwise hang
// waiting on a human.
var bobShellConfirm = func(path, what string, stdout io.Writer) bool {
	fmt.Fprintf(stdout, "%s %s\n", what, path)
	return confirm(stdout)
}

// bobShellAdviseManual prints what the user has to do by hand when agentop will not
// touch the file itself — an unrecognised $SHELL, a dangling rc symlink, or a chain
// past the hop limit. Returns 0: we could not do it for them, but we answered the
// question they asked, and that is a success.
//
// It takes the action because the callers sit ABOVE the halves' plans — they run
// before either half's changes are computed, so they cannot tell the verbs apart
// on their own. That is exactly what went wrong when each of them inlined
// enable's answer: a disable under fish printed "add this to your startup file"
// and the whole block, telling someone trying to REMOVE the integration to paste
// it in. Branching here rather than duplicating the arms keeps the two answers
// next to each other, where a future third fallback picks both up for free.
//
// where names the file in the caller's own words ("the real file", "whichever file
// your shell reads at startup"), because only the caller knows why it is giving up.
func bobShellAdviseManual(action, where string, stdout io.Writer) int {
	return rcAdviseManual(bobShellRC, action, where, stdout)
}

// rcAdviseManual is bobShellAdviseManual for any block.
func rcAdviseManual(blk rcBlock, action, where string, stdout io.Writer) int {
	if action == "disable" {
		// No block printed: a user removing the integration has the text already —
		// it is in their file — and printing it again is at best noise and at worst
		// read as an instruction to add it. The markers are what they need, since
		// that is what they are searching the file for.
		fmt.Fprintf(stdout, "To remove it by hand, delete the block between %s\nand %s from %s.\n",
			blk.start, blk.end, where)
		return 0
	}
	fmt.Fprintf(stdout, "Add this to %s:\n\n", where)
	fmt.Fprint(stdout, blk.text)
	return 0
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

	tmp, err := os.CreateTemp(dir, ".agentop-bobshell-*")
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
