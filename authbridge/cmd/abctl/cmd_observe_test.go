package main

import (
	"bytes"
	"flag"
	"os"
	"os/exec"
	"strings"
	"testing"
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
			"on 'abctl service' and 'abctl claude-code':\n%s", out)
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
