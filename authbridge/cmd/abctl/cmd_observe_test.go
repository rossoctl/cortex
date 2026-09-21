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
	if os.Getenv("ABCTL_HELP_CHILD") == "1" {
		// Re-exec'd child: be abctl.
		os.Args = []string{"abctl", "observe", "--help"}
		main()
		return ""
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestObserveFlags_DocumentThePrefsFile")
	cmd.Env = append(os.Environ(), "ABCTL_HELP_CHILD=1")
	out, err := cmd.CombinedOutput()
	// -h exits 0 through ExitOnError, but the test binary wrapping it may report
	// otherwise; the output is what matters.
	if len(out) == 0 && err != nil {
		t.Fatalf("child produced no output: %v", err)
	}
	return string(out)
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

// harvestClaudeMetadata writes the metadata file, and says nothing when it worked.
//
// The silence is half the point: this runs on every `abctl observe`, immediately above a
// viewer that is about to take the alt screen, so a success line would be noise on every
// single launch.
func TestHarvestClaudeMetadata_WritesTheFileQuietly(t *testing.T) {
	home := prefsHome(t)
	cfg := filepath.Join(t.TempDir(), "claude")
	writeSessionTranscript(t, filepath.Join(cfg, "projects", "-p"), "s1.jsonl",
		`{"type":"ai-title","aiTitle":"named by the harvest"}`)
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)

	var warn bytes.Buffer
	harvestClaudeMetadata(&warn)

	if warn.Len() != 0 {
		t.Errorf("a successful harvest wrote to stderr: %q", warn.String())
	}
	got := readMetadataFile(t, filepath.Join(home, tui.SessionMetadataRel))
	if got["s1"].Title != "named by the harvest" {
		t.Errorf("title = %q, want %q", got["s1"].Title, "named by the harvest")
	}
}

// A harvest that cannot work must not stop the viewer: no panic, no exit, and the
// caller gets on with opening the TUI.
//
// The viewer is the tool you reach for when everything else is broken, so a missing or
// unreadable ~/.claude costs a TITLE column and nothing more.
func TestHarvestClaudeMetadata_SurvivesAnUnreadableConfigDir(t *testing.T) {
	prefsHome(t)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "nonexistent"))

	var warn bytes.Buffer
	harvestClaudeMetadata(&warn)

	// A config dir that simply is not there is not a failure worth a word: a machine that
	// has never run Claude Code legitimately has none.
	if warn.Len() != 0 {
		t.Errorf("a missing config dir was reported as a problem: %q", warn.String())
	}
}

// `abctl observe` harvests implicitly, and --skip-claude-metadata stops it.
//
// The only test that pins the CALL rather than the function: the two above would both keep
// passing if the harvest were deleted from runObserve. A subprocess because runObserve
// opens a TUI — the same reason and the same mechanism as runObserveHelp.
//
// --endpoint names a port nothing listens on, so the child fails to connect and exits
// instead of taking the alt screen. That is harmless here: the harvest runs before the
// endpoint is ever dialled, so the file is already written by the time connecting fails.
func TestObserve_HarvestsClaudeMetadataUnlessSkipped(t *testing.T) {
	for _, tc := range []struct {
		name        string
		extraArg    string
		wantHarvest bool
	}{
		{"implicit by default", "", true},
		{"suppressed by the flag", "--skip-claude-metadata", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := prefsHome(t)
			cfg := filepath.Join(t.TempDir(), "claude")
			writeSessionTranscript(t, filepath.Join(cfg, "projects", "-p"), "s1.jsonl",
				`{"type":"ai-title","aiTitle":"from the observe path"}`)

			args := []string{"observe", "--endpoint", "http://127.0.0.1:1"}
			if tc.extraArg != "" {
				args = append(args, tc.extraArg)
			}
			cmd := exec.Command(os.Args[0], "-test.run=TestObserve_HarvestsClaudeMetadataUnlessSkipped")
			cmd.Env = append(os.Environ(),
				"ABCTL_OBSERVE_CHILD=1",
				"ABCTL_OBSERVE_ARGS="+strings.Join(args, " "),
				"HOME="+home,
				"USERPROFILE="+home,
				"CLAUDE_CONFIG_DIR="+cfg,
			)
			out, _ := cmd.CombinedOutput()

			path := filepath.Join(home, tui.SessionMetadataRel)
			_, err := os.Stat(path)
			if tc.wantHarvest {
				if err != nil {
					t.Fatalf("observe did not harvest: %v\nchild output:\n%s", err, out)
				}
				if got := readMetadataFile(t, path)["s1"].Title; got != "from the observe path" {
					t.Errorf("title = %q, want %q", got, "from the observe path")
				}
				return
			}
			if err == nil {
				t.Errorf("--skip-claude-metadata still harvested: %s exists\nchild output:\n%s", path, out)
			}
		})
	}
}

// TestMain lets the subprocess above re-enter this binary as abctl.
//
// Keyed on an env var rather than on an argument, so the parent can pass the child a
// -test.run filter and its own abctl argv independently.
func TestMain(m *testing.M) {
	if os.Getenv("ABCTL_OBSERVE_CHILD") == "1" {
		os.Args = append([]string{"abctl"}, strings.Fields(os.Getenv("ABCTL_OBSERVE_ARGS"))...)
		main()
		return
	}
	os.Exit(m.Run())
}
