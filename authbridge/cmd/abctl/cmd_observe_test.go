package main

import (
	"bytes"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/authbridge/cmd/abctl/tui"
)

// usageText renders the root usage block the way --help does, so the assertions
// below read the same text a user does.
func usageText(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	fs := flag.NewFlagSet("abctl", flag.ContinueOnError)
	fs.SetOutput(&buf)
	// Mirror the flags runObserve registers, so PrintDefaults has something to
	// print and the block is shaped as it is in practice.
	fs.String("endpoint", "", "endpoint")
	writeRootUsage(fs)
	return buf.String()
}

// The viewer must be reachable by name. Subcommands were discoverable only by
// typing --help, so a user who never did saw a TUI and concluded that was all
// abctl offered — the service commands least visible of all, and those are what
// you need when Cortex is down.
func TestRootUsage_ListsObserveFirst(t *testing.T) {
	out := usageText(t)
	if !strings.Contains(out, "abctl observe") {
		t.Errorf("usage does not mention the observe subcommand:\n%s", out)
	}
	// Listed above the deprecated bare form, so the reader meets the supported
	// spelling first.
	obs := strings.Index(out, "abctl observe")
	bare := strings.Index(out, "deprecated")
	if obs < 0 || bare < 0 || obs > bare {
		t.Errorf("observe should be listed before the deprecated bare form:\n%s", out)
	}
}

// Bare abctl still works, but the usage says it is going away — otherwise the
// deprecation exists only in a commit message.
func TestRootUsage_MarksBareInvocationDeprecated(t *testing.T) {
	out := usageText(t)
	if !strings.Contains(out, "deprecated") {
		t.Errorf("usage does not mark bare abctl deprecated:\n%s", out)
	}
	if !strings.Contains(out, "future release") {
		t.Errorf("usage does not say the behaviour will change:\n%s", out)
	}
}

// Every subcommand main dispatches must appear in the usage, or the discovery
// problem this change fixes reappears for the next one added.
func TestRootUsage_ListsEverySubcommand(t *testing.T) {
	out := usageText(t)
	for _, name := range dispatchableSubcommands {
		if !strings.Contains(out, "abctl "+name) {
			t.Errorf("usage omits the %q subcommand:\n%s", name, out)
		}
	}
}

// The supported spelling must be the one a reader meets first: `configure` is
// where new agents are added, and `claude-code` survives only for muscle memory and
// for the installer, which still types it.
func TestRootUsage_MarksClaudeCodeDeprecated(t *testing.T) {
	out := usageText(t)
	if !strings.Contains(out, "abctl configure") {
		t.Errorf("usage does not mention configure:\n%s", out)
	}
	ca := strings.Index(out, "abctl configure")
	cc := strings.Index(out, "abctl claude-code")
	if ca < 0 || cc < 0 || ca > cc {
		t.Errorf("configure should be listed before the deprecated claude-code:\n%s", out)
	}
	// The old spelling stays listed — it still dispatches, and a user who types it
	// needs to find it here — but must not read as the recommended way in.
	if !strings.Contains(out, `deprecated: same as "abctl configure`) {
		t.Errorf("usage does not mark claude-code deprecated:\n%s", out)
	}
}

// The unknown-subcommand error must name the same set main dispatches, so a typo
// gets a complete list rather than a stale one.
func TestUnknownSubcommandMessage_NamesEverySubcommand(t *testing.T) {
	msg := unknownSubcommandMessage("bogus")
	if !strings.Contains(msg, `"bogus"`) {
		t.Errorf("message does not quote the input: %q", msg)
	}
	for _, name := range dispatchableSubcommands {
		if !strings.Contains(msg, name) {
			t.Errorf("message omits %q: %q", name, msg)
		}
	}
}

// --help and --version must not carry the deprecation notice: --help is where the
// replacement is documented, so a warning above it pushes the answer further up
// the scrollback.
func TestWantsInfoFlagOnly(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"--help"}, true},
		{[]string{"-h"}, true},
		{[]string{"-help"}, true},
		{[]string{"--version"}, true},
		{[]string{"-version"}, true},
		{[]string{"--help", "--version"}, true},
		{nil, false},                                // bare abctl does warn
		{[]string{"--endpoint", "http://x"}, false}, // real work does warn
		{[]string{"--help", "--endpoint"}, false},   // mixed: opens the viewer
	} {
		if got := wantsInfoFlagOnly(tc.args); got != tc.want {
			t.Errorf("wantsInfoFlagOnly(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// --version describes the binary, not the viewer, so it is answered at the root
// and is not a flag on the subcommand. `abctl observe --version` is now the
// unknown flag it should be.
func TestIsVersionFlag(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"--version"}, true},
		{[]string{"-version"}, true},
		{nil, false},
		{[]string{"observe"}, false},
		// Exactly a version request, not merely containing one: printing a version
		// while silently discarding the rest would hide a confused invocation.
		{[]string{"--endpoint", "http://x", "--version"}, false},
		{[]string{"--version", "--endpoint"}, false},
	} {
		if got := isVersionFlag(tc.args); got != tc.want {
			t.Errorf("isVersionFlag(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// The root usage must document --version, since it is a root flag with no
// FlagSet entry to print it — removing it from the viewer's flag set took it out
// of PrintDefaults, so the usage text is now its only home.
func TestRootUsage_DocumentsVersion(t *testing.T) {
	out := usageText(t)
	if !strings.Contains(out, "--version") {
		t.Errorf("usage does not document --version:\n%s", out)
	}
	// And the flag list is labelled as the viewer's, not the binary's, so a
	// reader does not expect --version to appear among them.
	if !strings.Contains(out, "abctl observe") || !strings.Contains(out, "Viewer flags") {
		t.Errorf("usage does not scope the flag list to the viewer:\n%s", out)
	}
}

// TestObserveFlags_DocumentThePrefsFile renders the flag set runObserve actually
// builds, rather than usageText's hand-mirrored copy — the point is to catch the
// real flag drifting, which a mirror cannot do.
//
// Also asserts the value renders as "string": flag.PrintDefaults reads the first
// backquoted word in a usage string as the value's NAME, so a stray `abctl service`
// in the description silently rendered the flag as "-prefs abctl service".
func TestObserveFlags_DocumentThePrefsFile(t *testing.T) {
	// runObserve's flag set is built inline and it opens the TUI, so it cannot be
	// rendered directly here without extracting a helper this PR has no other reason
	// to add. `--help` on the real binary is the honest substitute: it exercises the
	// registration exactly as a user meets it.
	out := runObserveHelp(t)

	if !strings.Contains(out, "-prefs string") {
		t.Errorf("--prefs is missing or mis-rendered:\n%s", out)
	}
	if !strings.Contains(out, "abctl-config.yaml") {
		t.Errorf("--prefs does not name the default file:\n%s", out)
	}
	// The whole reason the flag is not called --config. Matched as a flag-list entry
	// ("  -config") rather than as a bare substring, because --prefs's own description
	// mentions --config in order to point at the proxy config.
	if strings.Contains(out, "  -config") {
		t.Errorf("the viewer grew a -config flag; --config already means the proxy config "+
			"on 'abctl service' and 'abctl configure claude-code':\n%s", out)
	}
}

// runObserveHelp runs `abctl observe --help` in a subprocess and returns its
// output. A subprocess because the flag set is ExitOnError, so -h exits the
// process — which is fine for a child and fatal for a test binary.
func runObserveHelp(t *testing.T) string {
	t.Helper()
	out, err := runAbctlChild(t, nil, "observe", "--help")
	// -h exits 0 through ExitOnError, but the test binary wrapping it may report
	// otherwise; the output is what matters.
	if len(out) == 0 && err != nil {
		t.Fatalf("child produced no output: %v", err)
	}
	return out
}

// --skip-claude-metadata must default to false, so a bare `abctl observe` names its
// sessions with no flag at all.
//
// Read off registerObserveFlags rather than a flag set of this test's own, for the reason
// TestKubernetesFlag_DefaultsToFalse spells out: declaring the flag here with a default of
// this test's choosing would assert that false equals false, and go on passing with
// production flipped to true — the one thing this test exists to catch.
func TestSkipClaudeMetadataFlag_DefaultsToFalse(t *testing.T) {
	fs := flag.NewFlagSet("abctl", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f := registerObserveFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if *f.skipClaudeMetadata {
		t.Error("--skip-claude-metadata must default to false, so `abctl observe` names sessions without a flag")
	}
	// The flag package's own record of the default, which is what --help prints.
	if got := fs.Lookup("skip-claude-metadata").DefValue; got != "false" {
		t.Errorf("registered default = %q, want \"false\"", got)
	}
}

// The background harvester writes the metadata file and returns the merged map, saying nothing
// on the way through.
//
// The silence matters more now than it did: the harvest runs while the viewer is up, so anything
// printed from it would land on the alt screen and corrupt the frame. Only failures knowable
// BEFORE the TUI starts get a word, and claudeHarvester checks those itself.
func TestClaudeHarvester_WritesTheFileQuietly(t *testing.T) {
	home := prefsHome(t)
	cfg := filepath.Join(t.TempDir(), "claude")
	writeSessionTranscript(t, filepath.Join(cfg, "projects", "-p"), "s1.jsonl",
		`{"type":"ai-title","aiTitle":"named by the harvest"}`)
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)

	var warn bytes.Buffer
	h := claudeHarvester(&warn)
	if h == nil {
		t.Fatal("claudeHarvester returned nil on a healthy tree")
	}
	if warn.Len() != 0 {
		t.Errorf("claudeHarvester complained before running: %q", warn.String())
	}

	meta, err := h()
	if err != nil {
		t.Fatal(err)
	}
	// The map it RETURNS is what names sessions in the running viewer, so the title has to be
	// in there and not only in the file.
	if meta["s1"].Title != "named by the harvest" {
		t.Errorf("returned title = %q, want %q", meta["s1"].Title, "named by the harvest")
	}
	if got := readMetadataFile(t, filepath.Join(home, tui.SessionMetadataRel)); got["s1"].Title != "named by the harvest" {
		t.Errorf("file title = %q, want %q", got["s1"].Title, "named by the harvest")
	}
}

// An incremental pass still returns every title, not just the ones it re-read.
//
// The harvest parses only changed transcripts, so its own result map is nearly empty on a warm
// tree — returning that would have blanked the TITLE column for every session it skipped, which
// is most of them. It returns the merged file instead. This is the regression that would have
// made the whole async change look like it broke titles.
func TestClaudeHarvester_ReturnsEveryTitleNotJustTheReparsedOnes(t *testing.T) {
	prefsHome(t)
	cfg := filepath.Join(t.TempDir(), "claude")
	proj := filepath.Join(cfg, "projects", "-p")
	writeSessionTranscript(t, proj, "old.jsonl", `{"type":"ai-title","aiTitle":"harvested earlier"}`)
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)

	// First pass records it.
	if _, err := claudeHarvester(io.Discard)(); err != nil {
		t.Fatal(err)
	}
	// Second pass skips it as unchanged, and must still name it.
	meta, err := claudeHarvester(io.Discard)()
	if err != nil {
		t.Fatal(err)
	}
	if meta["old"].Title != "harvested earlier" {
		t.Errorf("a skipped session lost its title: got %q", meta["old"].Title)
	}
}

// A harvest that cannot work must not stop the viewer: no panic, no exit, and an empty result
// rather than a refusal.
//
// The viewer is the tool you reach for when everything else is broken, so a missing ~/.claude
// costs a TITLE column and nothing more.
func TestClaudeHarvester_SurvivesAnUnreadableConfigDir(t *testing.T) {
	prefsHome(t)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "nonexistent"))

	var warn bytes.Buffer
	h := claudeHarvester(&warn)
	if h == nil {
		t.Fatal("a missing config dir must not disable the harvest outright")
	}
	if _, err := h(); err != nil {
		t.Errorf("a missing config dir was reported as an error: %v", err)
	}
	// Not worth a word: a machine that has never run Claude Code legitimately has none.
	if warn.Len() != 0 {
		t.Errorf("a missing config dir was reported as a problem: %q", warn.String())
	}
}

// A corrupt metadata file names the repair, and disables the harvest rather than running it.
//
// Checked BEFORE the TUI starts precisely so the message can be printed at all: the harvest now
// runs after the alt screen goes up, where a warning would corrupt the frame. It is also the one
// failure that cannot clear itself, since every launch reads the same bad file. observe has to
// name a different repair from the subcommand's --merge=false, which is not a flag it has.
//
// (This test existed before the async change and was lost in an edit; restored here in the
// factory's shape, which is also where it belongs now that the check moved.)
func TestClaudeHarvester_CorruptFileNamesTheRepair(t *testing.T) {
	home := prefsHome(t)
	cfg := filepath.Join(t.TempDir(), "claude")
	writeSessionTranscript(t, filepath.Join(cfg, "projects", "-p"), "s1.jsonl",
		`{"type":"ai-title","aiTitle":"t"}`)
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)

	path := filepath.Join(home, tui.SessionMetadataRel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	var warn bytes.Buffer
	if h := claudeHarvester(&warn); h != nil {
		t.Error("a corrupt file must disable the harvest, not be silently merged over")
	}
	got := warn.String()
	if !strings.Contains(got, "read-claude-sessions --merge=false") {
		t.Errorf("the warning does not name the repair:\n%s", got)
	}
	// The command it names must be runnable as printed, on its own line.
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "abctl experimental") && !strings.HasPrefix(strings.TrimSpace(line), "abctl experimental") {
			t.Errorf("the repair command is not on a line of its own: %q", line)
		}
	}
}

// `abctl observe` wires the harvest to the flag: on by default, off with
// --skip-claude-metadata.
//
// Asserted through observeHarvester, the function runObserve actually calls, rather than by watching
// a child process write a file. That used to work, when the harvest blocked startup. It cannot
// now: the harvest is a tea.Cmd, so it runs on bubbletea's goroutine after the program starts,
// and a headless child dies on "could not open a new TTY" before the scan finishes. Waiting on a
// race in a subprocess would be a flaky test of the wrong thing; the decision this test exists
// to pin is whether a harvester is HANDED OVER, and that is synchronous and local.
//
// The harvester's own behaviour is covered by the TestClaudeHarvester_* tests above, and that
// the TUI runs what it is handed by TestHarvestedMsg_* in the tui package.
func TestObserve_WiresTheHarvestToTheFlag(t *testing.T) {
	for _, tc := range []struct {
		name        string
		args        []string
		wantHarvest bool
	}{
		{"implicit by default", nil, true},
		{"suppressed by the flag", []string{"--skip-claude-metadata"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefsHome(t)
			cfg := filepath.Join(t.TempDir(), "claude")
			writeSessionTranscript(t, filepath.Join(cfg, "projects", "-p"), "s1.jsonl",
				`{"type":"ai-title","aiTitle":"from the observe path"}`)
			t.Setenv("CLAUDE_CONFIG_DIR", cfg)

			fs := flag.NewFlagSet("abctl", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			f := registerObserveFlags(fs)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}

			// The SAME function runObserve calls, not a copy of its condition: a test that
			// restated the `if` passed with the real wiring deleted.
			h := observeHarvester(f, io.Discard)

			if tc.wantHarvest {
				if h == nil {
					t.Fatal("observe handed the viewer no harvester, so sessions would never be named")
				}
				// And what it hands over really does name sessions.
				meta, err := h()
				if err != nil {
					t.Fatal(err)
				}
				if meta["s1"].Title != "from the observe path" {
					t.Errorf("title = %q, want %q", meta["s1"].Title, "from the observe path")
				}
				return
			}
			if h != nil {
				t.Error("--skip-claude-metadata still handed over a harvester")
			}
		})
	}
}

// childSentinel marks a re-exec of this test binary that should behave as abctl instead of
// running tests, and childArgs carries the argv to hand it.
//
// ONE sentinel for every subprocess test here, checked in one place. There used to be two at
// different layers — runObserveHelp's own, read inside the test, and this one, read in
// TestMain — and TestMain runs BEFORE any test-name filtering, so whichever is checked there
// wins for every test in the package. Two of them is a trap: the outer one silently decides
// what the inner one was trying to control.
const (
	childSentinel = "ABCTL_TEST_CHILD"
	childArgs     = "ABCTL_TEST_CHILD_ARGS"
)

// argSep separates argv entries in childArgs.
//
// A unit separator (0x1F), not a space: splitting on whitespace could not carry an argument
// that CONTAINS one — a --prefs path with a space in it, say — and would have silently torn it
// into two. Nothing here passes such an argument today, which is exactly why the limit was
// worth removing rather than documenting.
//
// Not NUL, which would be the obvious choice and is rejected outright: exec refuses an
// environment variable containing one ("environment variable contains NUL"). 0x1F is legal in
// an env var, is what it means, and cannot appear in a flag or path anyone would type.
const argSep = "\x1f"

// runAbctlChild re-execs this test binary as abctl with the given argv, returning its combined
// output. extraEnv entries are appended to the parent's environment as "K=V".
//
// The child runs no tests: TestMain hands control to main() before the test framework starts,
// so no -test.run filter is passed. Passing one would be inert — and looked meaningful, which
// is worse.
func runAbctlChild(t *testing.T, extraEnv []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		childSentinel+"=1",
		childArgs+"="+strings.Join(args, argSep),
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestMain lets runAbctlChild re-enter this binary as abctl.
//
// Keyed on an env var rather than on an argument, so the child's argv is abctl's own and
// carries no test-framework flags.
func TestMain(m *testing.M) {
	if os.Getenv(childSentinel) == "1" {
		argv := []string{"abctl"}
		if raw := os.Getenv(childArgs); raw != "" {
			argv = append(argv, strings.Split(raw, argSep)...)
		}
		os.Args = argv
		main()
		return
	}
	os.Exit(m.Run())
}

// The child argv round-trips an argument containing a space.
//
// The scheme this replaced joined on spaces and split with strings.Fields, so a --prefs path
// with a space in it would have arrived as two arguments and the flag would have taken the
// first half. Nothing in the suite passes such an argument today; this keeps the separator
// honest, since a regression would otherwise surface only in whatever future test first needs
// one.
//
// Asserted on the ENCODING, not on abctl's behaviour: a torn --prefs is observably silent —
// `observe` ignores stray positionals and an unreadable prefs path degrades to defaults by
// design — so a test driven through the child cannot tell the two apart and would pass either
// way (confirmed by reverting the separator and watching it still pass). The encoding is the
// thing that changed, so the encoding is what gets pinned.
func TestChildArgs_RoundTripsAnArgumentWithASpace(t *testing.T) {
	want := []string{"observe", "--prefs", "/tmp/a directory with spaces/abctl-config.yaml"}

	// Exactly what runAbctlChild puts in the env, and what TestMain takes back out.
	encoded := strings.Join(want, argSep)
	got := strings.Split(encoded, argSep)

	if len(got) != len(want) {
		t.Fatalf("round-tripped %d args, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg %d = %q, want %q", i, got[i], want[i])
		}
	}
	// And the separator has to be legal in an environment variable, which NUL is not.
	if strings.Contains(argSep, "\x00") {
		t.Error("argSep contains NUL, which exec rejects outright")
	}
}
