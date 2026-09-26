package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHome points os.UserHomeDir at a scratch directory. USERPROFILE as well as
// HOME, matching userconfig_e2e_test.go — os.UserHomeDir reads whichever the
// platform uses, and a test that sets only one passes on the author's machine.
func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

// The whole of the file-selection policy, pinned as a table. No file need exist:
// this maps a shell name to a path and nothing more.
//
// The two error rows are the load-bearing ones. An unrecognised shell must NOT
// fall back to a guess — writing a block into .bashrc for a fish user puts it in
// a file nothing reads, and the user has no reason to look there.
func TestBobShellRCPath(t *testing.T) {
	for _, tc := range []struct {
		name    string
		shell   string
		want    string // relative to home; "" means expect an error
		wantErr string // substring the error must carry
	}{
		{name: "zsh", shell: "/bin/zsh", want: ".zshrc"},
		{name: "bash", shell: "/bin/bash", want: ".bashrc"},
		// The basename is what decides, so a Homebrew or nix path works.
		{name: "bash from a prefix", shell: "/usr/local/bin/bash", want: ".bashrc"},
		{name: "zsh from a prefix", shell: "/opt/homebrew/bin/zsh", want: ".zshrc"},
		{name: "another shell", shell: "/usr/bin/fish", wantErr: "/usr/bin/fish"},
		{name: "unset", shell: "", wantErr: "$SHELL is not set"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := "/home/someone"
			got, err := bobShellRCPath(tc.shell, home)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("path = %q, want an error", got)
				}
				// The message must name what it could not handle, or the user
				// cannot tell which of their settings to change.
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not mention %q", err, tc.wantErr)
				}
				// An error must not also hand back a path a caller might use.
				if got != "" {
					t.Errorf("error case returned a path anyway: %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if want := filepath.Join(home, tc.want); got != want {
				t.Errorf("path = %q, want %q", got, want)
			}
		})
	}
}

// enable appends. The user's existing content must survive byte-for-byte as a
// prefix — an rc file holds things whose order matters, and rewriting or
// reordering any of it is not what "append a block" means.
func TestBobShellEnableAppends(t *testing.T) {
	home := fakeHome(t)
	t.Setenv("SHELL", "/bin/zsh")
	rc := filepath.Join(home, ".zshrc")
	original := "export EDITOR=vim\nalias ll='ls -l'\n"
	if err := os.WriteFile(rc, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := runBobShell([]string{"enable", "--yes"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, errb.String())
	}
	if errb.Len() != 0 {
		t.Errorf("stderr not empty: %q", errb.String())
	}

	got, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), original) {
		t.Errorf("existing content not preserved as a prefix:\n%s", got)
	}
	if !strings.Contains(string(got), bobShellBlock) {
		t.Errorf("block not written:\n%s", got)
	}
	// Without this the user enables it, sees nothing happen in the current
	// shell, and concludes it is broken.
	if !strings.Contains(out.String(), "source") {
		t.Errorf("stdout does not tell the user to source the file:\n%s", out.String())
	}
	// The path is the only way a user finds out WHERE it went, which matters most
	// in the macOS-bash case where the file may not be the one their terminal
	// reads.
	if !strings.Contains(out.String(), rc) {
		t.Errorf("stdout does not name the file it wrote:\n%s", out.String())
	}
}

// A file with no trailing newline is the case that silently corrupts: the block's
// first line fuses onto the user's last line, which both breaks that line and
// makes the block unmatchable, so disable could never remove it again.
func TestBobShellEnableAddsMissingNewline(t *testing.T) {
	home := fakeHome(t)
	t.Setenv("SHELL", "/bin/bash")
	rc := filepath.Join(home, ".bashrc")
	if err := os.WriteFile(rc, []byte("export EDITOR=vim"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := runBobShell([]string{"enable", "--yes"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, errb.String())
	}
	got, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), "export EDITOR=vim\n"+bobShellMarkerStart) {
		t.Errorf("block did not start on its own line:\n%q", got)
	}
	// The property that fusing would have destroyed.
	if !strings.Contains(string(got), bobShellBlock) {
		t.Errorf("block not matchable after the newline fix:\n%q", got)
	}
}

// enable on a file that does not exist creates it. This is the common case on a
// fresh machine, and the one where a missing-file error would be wrong.
func TestBobShellEnableCreatesTheFile(t *testing.T) {
	home := fakeHome(t)
	t.Setenv("SHELL", "/bin/zsh")
	rc := filepath.Join(home, ".zshrc")

	var out, errb bytes.Buffer
	if code := runBobShell([]string{"enable", "--yes"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, errb.String())
	}
	got, err := os.ReadFile(rc)
	if err != nil {
		t.Fatalf("file not created: %v", err)
	}
	if string(got) != bobShellBlock {
		t.Errorf("content = %q, want just the block", got)
	}
	st, err := os.Stat(rc)
	if err != nil {
		t.Fatal(err)
	}
	// 0644, not 0600: an rc file the user may share across machines should not be
	// created more restrictively than the convention.
	//
	// The literal, not rcFileMode. writeRCFile sets the mode FROM that constant, so
	// comparing against it compares the implementation with itself — both sides move
	// together and changing 0644 to anything else stays green. README.md commits to
	// 0644 by name, so 0644 is what the test says.
	if perm := st.Mode().Perm(); perm != 0o644 {
		t.Errorf("mode = %v, want %v", perm, os.FileMode(0o644))
	}
}

// An rc file that already exists keeps its own permissions. README.md promises this,
// and someone who deliberately locked their rc file down is exactly the person who
// would not notice abctl widening it again.
//
// 0600 is the mode to test with, because it is the one that differs from the
// new-file default: with a 0644 fixture, deleting writeRCFile's mode-inheritance
// branch outright changes no observable behaviour and every test still passes.
func TestBobShellEnablePreservesAnExistingMode(t *testing.T) {
	home := fakeHome(t)
	t.Setenv("SHELL", "/bin/zsh")
	rc := filepath.Join(home, ".zshrc")
	if err := os.WriteFile(rc, []byte("export EDITOR=vim\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// os.WriteFile applies the umask, so the fixture is only 0600 if we say so.
	if err := os.Chmod(rc, 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := runBobShell([]string{"enable", "--yes"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, errb.String())
	}

	st, err := os.Stat(rc)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %v, want 0600 preserved — the write widened a locked-down file", perm)
	}
	// The edit still has to have happened; a no-op would also preserve the mode.
	got, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), bobShellBlock) {
		t.Errorf("block not written:\n%q", got)
	}
}

// enable twice must not append twice. Two definitions of the same function is a
// file where the last one silently wins, and disable would then refuse (>1 copy),
// leaving the user unable to undo what enable did.
func TestBobShellEnableIsIdempotent(t *testing.T) {
	home := fakeHome(t)
	t.Setenv("SHELL", "/bin/zsh")
	rc := filepath.Join(home, ".zshrc")

	var out1, errb bytes.Buffer
	if code := runBobShell([]string{"enable", "--yes"}, &out1, &errb); code != 0 {
		t.Fatalf("first enable: exit = %d; stderr: %s", code, errb.String())
	}
	first, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}

	var out2 bytes.Buffer
	errb.Reset()
	if code := runBobShell([]string{"enable", "--yes"}, &out2, &errb); code != 0 {
		t.Fatalf("second enable: exit = %d; stderr: %s", code, errb.String())
	}
	second, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}

	if n := strings.Count(string(second), bobShellBlock); n != 1 {
		t.Errorf("block appears %d times, want 1:\n%s", n, second)
	}
	// Byte-identical, not merely "still one copy": a second run must not rewrite
	// the file at all.
	if !bytes.Equal(first, second) {
		t.Errorf("second enable changed the file:\n%q\nvs\n%q", first, second)
	}
	if !strings.Contains(out2.String(), "Already enabled") {
		t.Errorf("second run did not say it was already enabled:\n%s", out2.String())
	}
}

// enable's twin of TestBobShellDisable/"hand-edited between the markers": our markers
// are present but the block between them is not what we write, so enable declines too.
//
// Both verbs have to refuse here, for the same reason and with the same outcome.
// Appending a second block would leave two `bob()` definitions in one file with the
// last one silently winning — a worse state than the one the user started in, and one
// disable would then refuse to clean up because it sees more than one copy.
//
// This is the enable half of a guard that had no test at all. Deleting the whole `if`
// from cmd_bobshell.go left the package green, because only disable's arm was covered.
func TestBobShellEnableDeclinesAHandEditedBlock(t *testing.T) {
	home := fakeHome(t)
	t.Setenv("SHELL", "/bin/zsh")
	rc := filepath.Join(home, ".zshrc")
	// Same fixture shape as the disable row, so the two refusals are visibly the
	// same case seen from either verb.
	content := bobShellMarkerStart + "\nbob() { echo my own version; }\n" + bobShellMarkerEnd + "\n"
	if err := os.WriteFile(rc, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	// Exit 0, like disable's refusal: declining with an explanation is a successful
	// outcome, not a failure to be scripted against.
	if code := runBobShell([]string{"enable", "--yes"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, errb.String())
	}
	if errb.Len() != 0 {
		t.Errorf("stderr not empty: %q", errb.String())
	}
	if !strings.Contains(out.String(), "not one matching") {
		t.Errorf("stdout does not explain the mismatch:\n%s", out.String())
	}

	// The whole point of the guard: the user's version survives untouched, and no
	// second block was appended beside it.
	got, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf("file changed:\n%q\nwant\n%q", got, content)
	}
	if strings.Contains(string(got), bobShellBlock) {
		t.Errorf("our block was appended alongside the user's:\n%q", got)
	}
}

// The load-bearing test: enable then disable must restore the file byte-for-byte.
//
// Asserted as byte equality rather than "the markers are gone", because that is
// the actual promise — disable removes exactly what enable added and nothing
// else. It is also what makes drift between the two unmergeable: any change to
// what enable writes that disable does not match fails here.
func TestBobShellRoundTripIsByteIdentical(t *testing.T) {
	// Every input that used to be here ended in a newline or was empty, which is
	// exactly why the table missed a real defect: enable appended a newline to a
	// file that lacked one and disable removed only the block, so that byte was
	// never removed and the round trip lost. The no-trailing-newline rows are the
	// point of the table now, not an edge case beside it.
	for _, tc := range []struct{ name, original string }{
		{"empty", ""},
		{"trailing newline", "export EDITOR=vim\n"},
		{"no trailing newline", "export EDITOR=vim"},
		{"no trailing newline, multi-line", "# leading comment\nalias g=git"},
		{"several trailing newlines", "export EDITOR=vim\n\n\n"},
		{"only a newline", "\n"},
		{"multi-line", "# leading comment\n\nexport PATH=$PATH:/opt/bin\nalias g=git\n"},
	} {
		original := tc.original
		t.Run(tc.name, func(t *testing.T) {
			home := fakeHome(t)
			t.Setenv("SHELL", "/bin/zsh")
			rc := filepath.Join(home, ".zshrc")
			if original != "" {
				if err := os.WriteFile(rc, []byte(original), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			var out, errb bytes.Buffer
			if code := runBobShell([]string{"enable", "--yes"}, &out, &errb); code != 0 {
				t.Fatalf("enable: exit = %d; stderr: %s", code, errb.String())
			}
			out.Reset()
			errb.Reset()
			if code := runBobShell([]string{"disable", "--yes"}, &out, &errb); code != 0 {
				t.Fatalf("disable: exit = %d; stderr: %s", code, errb.String())
			}

			got, err := os.ReadFile(rc)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != original {
				t.Errorf("round trip changed the file:\ngot  %q\nwant %q", got, original)
			}
		})
	}
}

func TestBobShellDisable(t *testing.T) {
	for _, tc := range []struct {
		name      string
		content   string // "" means do not create the file
		wantSays  string
		wantFinal string // "" means "expect the block gone"; "=" means unchanged
	}{
		{
			name:      "no file",
			wantSays:  "Not enabled",
			wantFinal: "=",
		},
		{
			name:      "file without the block",
			content:   "export EDITOR=vim\n",
			wantSays:  "Not enabled",
			wantFinal: "=",
		},
		{
			name:      "exactly one copy",
			content:   "export EDITOR=vim\n" + bobShellBlock,
			wantSays:  "Disabled",
			wantFinal: "export EDITOR=vim\n",
		},
		{
			// Removing one of two is a silent half-measure; removing both assumes
			// the extra is ours. Neither is right, so refuse and say so.
			name:      "two copies",
			content:   bobShellBlock + bobShellBlock,
			wantSays:  "2 copies",
			wantFinal: "=",
		},
		{
			// Our markers, someone else's content. Deleting it is not "remove
			// exactly what enable added".
			name:      "hand-edited between the markers",
			content:   bobShellMarkerStart + "\nbob() { echo my own version; }\n" + bobShellMarkerEnd + "\n",
			wantSays:  "edited since it was written",
			wantFinal: "=",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := fakeHome(t)
			t.Setenv("SHELL", "/bin/zsh")
			rc := filepath.Join(home, ".zshrc")
			if tc.content != "" {
				if err := os.WriteFile(rc, []byte(tc.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			var out, errb bytes.Buffer
			// Every one of these is exit 0: a refusal that explains itself is a
			// successful outcome for a command whose job is "or do nothing".
			if code := runBobShell([]string{"disable", "--yes"}, &out, &errb); code != 0 {
				t.Fatalf("exit = %d, want 0; stderr: %s", code, errb.String())
			}
			if errb.Len() != 0 {
				t.Errorf("stderr not empty: %q", errb.String())
			}
			if !strings.Contains(out.String(), tc.wantSays) {
				t.Errorf("stdout missing %q:\n%s", tc.wantSays, out.String())
			}

			want := tc.wantFinal
			if want == "=" {
				want = tc.content
			}
			got, err := os.ReadFile(rc)
			if err != nil && tc.content != "" {
				t.Fatal(err)
			}
			if string(got) != want {
				t.Errorf("file = %q, want %q", got, want)
			}
		})
	}
}

// status reads the environment and nothing else, and succeeds either way — "not
// enabled" is a report, not a failure, matching claudeCodeStatus.
func TestBobShellStatus(t *testing.T) {
	for _, tc := range []struct {
		name        string
		set         bool
		value       string
		wantSays    string
		wantNotSays string
	}{
		// wantSays / wantNotSays as a PAIR, because the two reports share a
		// substring: "not enabled in this shell" contains "enabled in this shell",
		// so a one-sided Contains on the shorter phrase passes in both states and
		// pins neither. Whatever these phrases become, each row has to name
		// something the other state does not print.
		{name: "enabled", set: true, value: "1", wantSays: "configured", wantNotSays: "not enabled"},
		{name: "not enabled", wantSays: "not enabled", wantNotSays: "configured"},
		// Any value counts as set. The block writes 1, but a user who exported it
		// by hand should not get a different answer for an equivalent setting.
		{name: "some other value", set: true, value: "yes", wantSays: "configured", wantNotSays: "not enabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(bobShellEnvVar, tc.value)
			} else {
				// t.Setenv restores on cleanup, so this cannot leak either way.
				t.Setenv(bobShellEnvVar, "")
				os.Unsetenv(bobShellEnvVar)
			}
			var out, errb bytes.Buffer
			if code := runBobShell([]string{"status"}, &out, &errb); code != 0 {
				t.Errorf("exit = %d, want 0", code)
			}
			if !strings.Contains(out.String(), tc.wantSays) {
				t.Errorf("stdout missing %q:\n%s", tc.wantSays, out.String())
			}
			if strings.Contains(out.String(), tc.wantNotSays) {
				t.Errorf("stdout carries the other state's report %q:\n%s", tc.wantNotSays, out.String())
			}
			// A status a script cannot pipe is half a status.
			if errb.Len() != 0 {
				t.Errorf("stderr not empty: %q", errb.String())
			}
		})
	}
}

// status must not need a home directory, an rc file, or a recognisable $SHELL:
// it answers from the environment, so it has to work where enable would refuse.
func TestBobShellStatusNeedsNothingButTheEnvironment(t *testing.T) {
	fakeHome(t)
	t.Setenv("SHELL", "/usr/bin/fish")
	t.Setenv(bobShellEnvVar, "1")

	var out, errb bytes.Buffer
	if code := runBobShell([]string{"status"}, &out, &errb); code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "configured") {
		t.Errorf("status did not answer under an unrecognised shell:\n%s", out.String())
	}
}

// rcTarget's own contract, tested directly because runBobShell can only observe
// it through outcomes. The hop count is the value the caller branches on, and
// EvalSymlinks cannot produce it — which is why this is hand-rolled.
func TestRCTarget(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	one := filepath.Join(dir, "one")
	if err := os.Symlink(plain, one); err != nil {
		t.Fatal(err)
	}
	two := filepath.Join(dir, "two")
	if err := os.Symlink(one, two); err != nil {
		t.Fatal(err)
	}
	// A relative link must resolve against the link's own directory, not the
	// process working directory.
	rel := filepath.Join(dir, "rel")
	if err := os.Symlink("plain", rel); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		path     string
		wantHops int
		wantErr  bool
	}{
		{name: "regular file", path: plain, wantHops: 0},
		{name: "one hop", path: one, wantHops: 1},
		// Counting stops once past the limit; the caller only needs "too deep".
		{name: "two hops", path: two, wantHops: 2},
		{name: "relative link", path: rel, wantHops: 1},
		// Absent is normal — enable creates the file — so it reports 0 hops and
		// an error the caller distinguishes with errors.Is(err, os.ErrNotExist).
		{name: "absent", path: filepath.Join(dir, "missing"), wantHops: 0, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, hops, err := rcTarget(tc.path)
			if hops != tc.wantHops {
				t.Errorf("hops = %d, want %d", hops, tc.wantHops)
			}
			if tc.wantErr != (err != nil) {
				t.Errorf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}

	t.Run("absent is os.ErrNotExist", func(t *testing.T) {
		// The distinction enable depends on: a missing file is a normal state, a
		// dangling link is not, and both arrive as an error from Lstat.
		_, _, err := rcTarget(filepath.Join(dir, "missing"))
		if !os.IsNotExist(err) {
			t.Errorf("err = %v, want a not-exist error", err)
		}
	})
}

// The block's shape, asserted against the specific mistakes a previous attempt
// made. Each of these was a real defect: dialect-specific builtins to resolve the
// binary past the function (dash has neither "whence" nor "type -P", and printed
// prose that the shell then executed), and a variable that leaked into the user's
// interactive shell. None is needed — "abctl exec" execve's into a process that
// never reads this file, so the function cannot recurse.
//
// A test rather than a review note, so reintroducing any of it fails a build.
func TestBobShellBlockShape(t *testing.T) {
	for _, banned := range []string{
		"whence",  // zsh-only
		"type -P", // bash-only
		"command -v",
		"local ", // leaked a variable into the user's shell
		"$(",     // command substitution: nothing here needs to run at source time
		"`",      // the other substitution spelling
		"if ",    // no conditionals: the block must be the same on every machine
	} {
		if strings.Contains(bobShellBlock, banned) {
			t.Errorf("block contains %q — see this test's comment for why not:\n%s", banned, bobShellBlock)
		}
	}

	// The properties disable depends on.
	//
	// A leading newline, then the marker: the block separates itself from whatever
	// it is appended to, which is what lets enable append it unchanged and disable
	// remove it with one Replace. A separator kept outside the constant is not
	// invertible — disable cannot tell our newline from the user's — so this
	// assertion is load-bearing, not cosmetic.
	if !strings.HasPrefix(bobShellBlock, "\n"+bobShellMarkerStart) {
		t.Errorf("block does not start with a newline and the start marker:\n%q", bobShellBlock)
	}
	if !strings.HasSuffix(bobShellBlock, bobShellMarkerEnd+"\n") {
		t.Errorf("block does not end with the end marker and a newline:\n%q", bobShellBlock)
	}
	// Removing the block must not leave a dangling blank region or join two
	// unrelated lines, which a block not ending in a newline would do.
	if !strings.HasSuffix(bobShellBlock, "\n") {
		t.Errorf("block does not end with a newline: %q", bobShellBlock)
	}
	// What the feature actually is. Asserted so a refactor cannot quietly change
	// which command the function runs.
	if !strings.Contains(bobShellBlock, `abctl exec -- bob "$@"`) {
		t.Errorf("block does not run abctl exec -- bob \"$@\":\n%s", bobShellBlock)
	}
	if !strings.Contains(bobShellBlock, "export "+bobShellEnvVar+"=1") {
		t.Errorf("block does not export %s, which is what status reads:\n%s", bobShellEnvVar, bobShellBlock)
	}
}

// An unrecognised shell must write NOTHING and say what to do by hand. Writing a
// guess puts the block in a file the user's shell never reads, and they have no
// reason to look there to find out why "bob" does not work.
//
// The advice has to match the VERB, and this test used to assert the block for both
// — codifying the bug rather than catching it. `disable` under fish printed "Add
// this to whichever file your shell reads at startup" and the whole block, handing
// someone who was removing the integration the text to paste in. wantBlock is the
// property that was missing.
func TestBobShellUnknownShell(t *testing.T) {
	home := fakeHome(t)
	t.Setenv("SHELL", "/usr/bin/fish")

	for _, tc := range []struct {
		action    string
		wantBlock bool
	}{
		// enable cannot do the edit, so the block is the whole answer.
		{"enable", true},
		// disable must NOT print it: the user already has the text — it is in their
		// file — and printing it here reads as an instruction to add what they asked
		// to have removed. They get the markers to search for instead.
		{"disable", false},
	} {
		t.Run(tc.action, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := runBobShell([]string{tc.action}, &out, &errb); code != 0 {
				t.Fatalf("exit = %d, want 0; stderr: %s", code, errb.String())
			}
			// Nothing at all in the home directory: not .bashrc, not .zshrc, and
			// no leftover temp file from a write that was attempted and undone.
			entries, err := os.ReadDir(home)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				names := make([]string, 0, len(entries))
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Errorf("created files under an unrecognised shell: %v", names)
			}
			if got := strings.Contains(out.String(), bobShellBlock); got != tc.wantBlock {
				t.Errorf("block printed = %v, want %v; stdout:\n%s", got, tc.wantBlock, out.String())
			}
			// Whichever half it printed, it has to name the markers — they are what
			// the user greps for, in a file abctl is not going to touch.
			if !strings.Contains(out.String(), bobShellMarkerStart) {
				t.Errorf("stdout does not name the start marker:\n%s", out.String())
			}
			// Name the shell, so the user can see which setting produced this.
			if !strings.Contains(out.String(), "/usr/bin/fish") {
				t.Errorf("stdout does not name $SHELL:\n%s", out.String())
			}
		})
	}
}

// The other two fallbacks, which sit at the same spot above the verb dispatch and
// had the same bug: a dangling rc symlink and a chain past the hop limit. Covered
// here rather than left to the unrecognised-$SHELL case because all three inlined
// enable's answer independently, so one test passing proves nothing about the other
// two — and the symlink arms are the ones no test reached at all.
func TestBobShellUnwritableLinkAdvisesPerVerb(t *testing.T) {
	for _, shape := range []struct {
		name string
		// setup arranges home so that .zshrc cannot be written through, and
		// returns nothing: the rc path is always $home/.zshrc.
		setup func(t *testing.T, home string)
	}{
		{"dangling link", func(t *testing.T, home string) {
			if err := os.Symlink(filepath.Join(home, "nowhere"), filepath.Join(home, ".zshrc")); err != nil {
				t.Fatal(err)
			}
		}},
		{"past the hop limit", func(t *testing.T, home string) {
			real := filepath.Join(home, "real")
			if err := os.WriteFile(real, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			mid := filepath.Join(home, "mid")
			if err := os.Symlink(real, mid); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(mid, filepath.Join(home, ".zshrc")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		for _, tc := range []struct {
			action    string
			wantBlock bool
		}{
			{"enable", true},
			{"disable", false},
		} {
			t.Run(shape.name+"/"+tc.action, func(t *testing.T) {
				home := fakeHome(t)
				t.Setenv("SHELL", "/bin/zsh")
				shape.setup(t, home)

				var out, errb bytes.Buffer
				if code := runBobShell([]string{tc.action}, &out, &errb); code != 0 {
					t.Fatalf("exit = %d, want 0; stderr: %s", code, errb.String())
				}
				if got := strings.Contains(out.String(), bobShellBlock); got != tc.wantBlock {
					t.Errorf("block printed = %v, want %v; stdout:\n%s", got, tc.wantBlock, out.String())
				}
				if !strings.Contains(out.String(), bobShellMarkerStart) {
					t.Errorf("stdout does not name the start marker:\n%s", out.String())
				}
				// The refusal must not have written anything through the link: the
				// dangling case would create the missing target, and the deep case
				// would replace a link the user built on purpose.
				if _, err := os.Stat(filepath.Join(home, "nowhere")); err == nil {
					t.Error("created the dangling link's missing target")
				}
				st, err := os.Lstat(filepath.Join(home, ".zshrc"))
				if err != nil {
					t.Fatal(err)
				}
				if st.Mode()&os.ModeSymlink == 0 {
					t.Error(".zshrc is no longer a symlink: the refusal wrote over it")
				}
			})
		}
	}
}

// Usage errors, and the stdout/stderr split they turn on: explicit help is a
// successful answer (stdout, 0); a missing or misspelled action is an error
// (stderr, 2). Same shape as TestClaudeCodeHelp_PrintsUsageOnStdout, and for the
// same reason — `--help` read as an action name sends someone looking for the
// command list to the one branch that refuses to print it.
// Nothing follows the verb, and anything that does is a misunderstanding that
// must not be silently dropped.
//
// Both of these did real damage before the verbs went through a FlagSet:
// runBobShell read args[0] and never looked at the rest, so `disable --help`
// DELETED the block instead of printing help, and `enable --dry-run` enabled for
// real. Each row therefore asserts the file too, not just the exit code — a
// refusal that still wrote is the failure this is guarding.
func TestBobShellRejectsStrayArguments(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"disable --help", []string{"disable", "--help"}},
		{"enable --dry-run", []string{"enable", "--dry-run"}},
		{"enable -n", []string{"enable", "-n"}},
		{"status extra", []string{"status", "extra"}},
		{"enable operand", []string{"enable", "~/.bashrc"}},
		{"disable operand", []string{"disable", "~/.bashrc"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := fakeHome(t)
			t.Setenv("SHELL", "/bin/zsh")
			rc := filepath.Join(home, ".zshrc")
			const original = "export EDITOR=vim\n"
			if err := os.WriteFile(rc, []byte(original), 0o644); err != nil {
				t.Fatal(err)
			}
			// Enabled first, so a `disable` that wrongly went ahead has something to
			// remove and shows up as a changed file rather than a no-op.
			var out, errb bytes.Buffer
			if code := runBobShell([]string{"enable", "--yes"}, &out, &errb); code != 0 {
				t.Fatalf("setup enable: exit = %d; stderr: %s", code, errb.String())
			}
			before, err := os.ReadFile(rc)
			if err != nil {
				t.Fatal(err)
			}

			out.Reset()
			errb.Reset()
			code := runBobShell(tc.args, &out, &errb)

			// --help is a successful answer even here: the FlagSet reports it as
			// flag.ErrHelp, and the usage goes to stdout with exit 0. Every other
			// row is a usage error on stderr with exit 2. What both share, and what
			// this test exists for, is that neither touches the file.
			if tc.args[1] == "--help" {
				if code != 0 {
					t.Errorf("exit = %d, want 0; stderr: %s", code, errb.String())
				}
				if !strings.Contains(out.String(), "abctl configure bobshell —") {
					t.Errorf("usage not on stdout:\n%s", out.String())
				}
			} else {
				if code != 2 {
					t.Errorf("exit = %d, want 2; stdout: %s", code, out.String())
				}
				if errb.Len() == 0 {
					t.Errorf("nothing on stderr to say what was wrong")
				}
			}

			after, err := os.ReadFile(rc)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Errorf("the rc file was written anyway:\ngot  %q\nwant %q", after, before)
			}
		})
	}
}

func TestBobShellHelpAndUsageErrors(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "help"} {
		t.Run("help "+arg, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := runBobShell([]string{arg}, &out, &errb); code != 0 {
				t.Errorf("exit = %d, want 0", code)
			}
			if !strings.Contains(out.String(), "abctl configure bobshell —") {
				t.Errorf("usage not on stdout:\n%s", out.String())
			}
			if strings.Contains(out.String()+errb.String(), "unknown bobshell action") {
				t.Errorf("help was read as an action name:\n%s%s", out.String(), errb.String())
			}
			// Explicit help is a successful answer, so it must be pipeable.
			if errb.Len() != 0 {
				t.Errorf("stderr not empty: %q", errb.String())
			}
		})
	}

	t.Run("no action", func(t *testing.T) {
		var out, errb bytes.Buffer
		if code := runBobShell(nil, &out, &errb); code != 2 {
			t.Errorf("exit = %d, want 2", code)
		}
		if !strings.Contains(errb.String(), "abctl configure bobshell —") {
			t.Errorf("usage not on stderr:\n%s", errb.String())
		}
		if out.Len() != 0 {
			t.Errorf("stdout not empty: %q", out.String())
		}
	})

	t.Run("misspelled action", func(t *testing.T) {
		fakeHome(t)
		t.Setenv("SHELL", "/bin/zsh")
		var out, errb bytes.Buffer
		// A genuine typo must still be an error — the help case above must not
		// have widened into swallowing every unrecognised action.
		if code := runBobShell([]string{"enabel"}, &out, &errb); code != 2 {
			t.Errorf("exit = %d, want 2", code)
		}
		// Quoted, so an action carrying a stray shell character is legible.
		if !strings.Contains(errb.String(), `"enabel"`) {
			t.Errorf("stderr does not quote the input: %q", errb.String())
		}
		// Naming the valid set is the difference between a refusal and a dead end.
		if !strings.Contains(errb.String(), "enable, disable, status") {
			t.Errorf("stderr does not name the valid actions: %q", errb.String())
		}
	})

	// A bad verb must be refused whatever the environment looks like.
	//
	// The subtest above pins SHELL=/bin/zsh, and that is exactly why the bug this
	// covers survived: three steps between the help switch and the old
	// action check answer successfully on their own — an unrecognised $SHELL, a
	// dangling rc symlink, and a chain past the hop limit each print the block and
	// return 0. With the check at the bottom, `bobshell enabel` under fish printed
	// the block and exited 0, reporting success for a verb that does not exist.
	//
	// Each row below reaches a different one of those early returns, so a future
	// early return that forgets to validate first fails here rather than shipping.
	t.Run("misspelled action beats every early return", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			setup func(t *testing.T, home string)
			shell string
		}{
			{name: "unrecognised shell", shell: "/usr/bin/fish"},
			{name: "shell unset", shell: ""},
			{
				name:  "dangling rc symlink",
				shell: "/bin/zsh",
				setup: func(t *testing.T, home string) {
					if err := os.Symlink(filepath.Join(home, "nowhere"), filepath.Join(home, ".zshrc")); err != nil {
						t.Fatal(err)
					}
				},
			},
			{
				name:  "rc symlink past the hop limit",
				shell: "/bin/zsh",
				setup: func(t *testing.T, home string) {
					dir := t.TempDir()
					real := filepath.Join(dir, "zshrc")
					if err := os.WriteFile(real, []byte("export EDITOR=vim\n"), 0o644); err != nil {
						t.Fatal(err)
					}
					mid := filepath.Join(dir, "middle")
					if err := os.Symlink(real, mid); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(mid, filepath.Join(home, ".zshrc")); err != nil {
						t.Fatal(err)
					}
				},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				home := fakeHome(t)
				t.Setenv("SHELL", tc.shell)
				if tc.setup != nil {
					tc.setup(t, home)
				}

				var out, errb bytes.Buffer
				if code := runBobShell([]string{"enabel"}, &out, &errb); code != 2 {
					t.Errorf("exit = %d, want 2", code)
				}
				if !strings.Contains(errb.String(), `"enabel"`) {
					t.Errorf("stderr does not name the bad action: %q", errb.String())
				}
				// The signature of the bug: advice printed for a verb that does not
				// exist. A refusal must not look like a successful answer.
				if strings.Contains(out.String(), bobShellBlock) {
					t.Errorf("printed the block for an unknown action:\n%s", out.String())
				}
			})
		}
	})
}

// The "run:" line enable prints is the one thing in its output meant to be COPIED
// AND EXECUTED, so it has to survive a $HOME with a space in it — which is not
// exotic: a macOS account named "Ed Snible" produces one, as does every Windows
// "Documents and Settings" descendant.
//
// The break this pins is nastier than a cosmetic one. The rc file is written to the
// right path, so enable is not wrong about what it did; only the instructions for
// loading it are wrong, and they fail in the user's own shell minutes later with
// "no such file or directory: /Users/Ed" — a path the user never typed and cannot
// map back to this command.
//
// Verified by actually running it, rather than by inspecting the string. A
// field-count check cannot express this: strings.Fields knows nothing about
// quoting, so a correctly quoted path with a space in it still splits into three
// fields and the assertion fails on working code. What matters is what a shell
// does with the line, so the test hands the line to a shell. The quoted-form check
// alongside it names the fix, so a regression that half-quotes — double quotes,
// say, which leave $ and backtick live — is caught as well.
func TestBobShellEnableQuotesThePathItTellsYouToSource(t *testing.T) {
	home := filepath.Join(t.TempDir(), "Ed Snible")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("SHELL", "/bin/zsh")

	var out, errb bytes.Buffer
	if code := runBobShell([]string{"enable", "--yes"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr: %s", code, errb.String())
	}

	rc := filepath.Join(home, ".zshrc")
	// The write itself was never the broken half; assert it anyway, so a future
	// change that stops writing cannot pass this test on its output alone.
	if _, err := os.Stat(rc); err != nil {
		t.Fatalf("enable did not write the rc file: %v", err)
	}

	var sourceLine string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "source ") {
			sourceLine = strings.TrimSpace(line)
			break
		}
	}
	if sourceLine == "" {
		t.Fatalf("no source line in stdout:\n%s", out.String())
	}
	if !strings.Contains(sourceLine, "'"+rc+"'") {
		t.Errorf("the path is not single-quoted: %q", sourceLine)
	}

	// Ground truth: run it. A shell resolves the quoting exactly as the user's shell
	// will, so this fails on an unquoted path (sourcing ".../Ed", which does not
	// exist) and on a broken quoting scheme, without the test needing to model
	// either. The rc file holds only a function definition and an export, which any
	// of these shells parses.
	//
	// The VERB is swapped to "." before running, and only the verb. `source` is a
	// bash/zsh builtin; POSIX specifies `.`, and dash implements only that — so
	// `sh -c "source ..."` exits 127 "source: not found" wherever /bin/sh is dash,
	// which is every Ubuntu CI runner. It passes on macOS, where /bin/sh is bash in
	// POSIX mode and keeps the builtin, so this is a defect that hides locally and
	// fails only in CI. What enable PRINTS stays `source`: its audience is an
	// interactive zsh or bash, where `source` is idiomatic and correct, and the
	// advice is not being tested here — the quoting of the path is. Substituting the
	// verb keeps the path under test byte-for-byte as enable emitted it, which
	// rebuilding the line from rc would not.
	runnable := ". " + strings.TrimPrefix(sourceLine, "source ")
	for _, sh := range []string{"/bin/sh", "/bin/bash", "/bin/zsh"} {
		if _, err := os.Stat(sh); err != nil {
			continue // not every runner has all three
		}
		if outBytes, err := exec.Command(sh, "-c", runnable).CombinedOutput(); err != nil {
			t.Errorf("%s could not run the printed command %q (as %q): %v\n%s", sh, sourceLine, runnable, err, outBytes)
		}
	}
}

// Without --yes, and with no terminal to prompt on, both verbs must write
// NOTHING. `go test` has no controlling terminal, so confirm's /dev/tty open
// fails and it declines — which is the CI and container case, and the reason
// --yes exists at all.
//
// This is the guard on a real hazard in the confirm/--yes pair: the prompt sits
// between "decided to write" and "wrote", so a mistake there does not fail
// loudly, it just silently stops applying — enable reporting success while the
// rc file is untouched. Every other test in this file passes --yes, so without
// this one nothing exercises the unconfirmed path and deleting the prompt
// entirely would keep the suite green.
func TestBobShellWithoutYesAndWithoutATerminalWritesNothing(t *testing.T) {
	for _, verb := range []string{"enable", "disable"} {
		t.Run(verb, func(t *testing.T) {
			home := fakeHome(t)
			t.Setenv("SHELL", "/bin/zsh")
			rc := filepath.Join(home, ".zshrc")

			// disable needs a block present, or it answers "not enabled" and returns
			// before ever reaching the prompt — which would pass this test for the
			// wrong reason.
			var setup bytes.Buffer
			if code := runBobShell([]string{"enable", "--yes"}, &setup, &setup); code != 0 {
				t.Fatalf("setup enable: exit = %d: %s", code, setup.String())
			}
			before, err := os.ReadFile(rc)
			if err != nil {
				t.Fatal(err)
			}
			if verb == "enable" {
				// For enable the interesting file is one with no block yet, so the
				// write is the thing the prompt is gating.
				if err := os.WriteFile(rc, []byte("export EDITOR=vim\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if before, err = os.ReadFile(rc); err != nil {
					t.Fatal(err)
				}
			}

			var out, errb bytes.Buffer
			code := runBobShell([]string{verb}, &out, &errb)

			// Declining is not a failure: nothing was asked for that could not be
			// done, so this is the same "advice printed, nothing applied" exit 0 as
			// an unrecognised $SHELL.
			if code != 0 {
				t.Errorf("exit = %d, want 0; stderr: %s", code, errb.String())
			}
			after, err := os.ReadFile(rc)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Errorf("%s wrote without confirmation:\nbefore: %q\nafter:  %q", verb, before, after)
			}
			// The user has to be told how to get past it, or the command looks broken.
			if !strings.Contains(out.String(), "--yes") {
				t.Errorf("stdout does not mention --yes:\n%s", out.String())
			}
			// And which file was at stake, since $SHELL chose it rather than the user.
			if !strings.Contains(out.String(), rc) {
				t.Errorf("stdout does not name the file %q:\n%s", rc, out.String())
			}
		})
	}
}

// The advice status prints must name "type", not "which". In bash, `which` is
// /usr/bin/which — a separate process that cannot see its parent shell's
// function table — so with the function live it prints the bob BINARY's path,
// which under the old wording ("a path means no") reads as a definitive "not
// routed" in exactly the case the check exists to detect. zsh's `which` is a
// builtin and does report the function, which is why the wrong advice survived:
// it works in one of the two shells this command writes a file for.
func TestBobShellStatusAdvisesTypeNotWhich(t *testing.T) {
	t.Setenv(bobShellEnvVar, "1")
	var out bytes.Buffer
	if code := bobShellStatus(&out); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out.String(), "type bob") {
		t.Errorf("status does not advise \"type bob\":\n%s", out.String())
	}
	if strings.Contains(out.String(), "which bob") {
		t.Errorf("status still advises \"which bob\", which is wrong in bash:\n%s", out.String())
	}
}
