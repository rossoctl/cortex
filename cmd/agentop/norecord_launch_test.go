package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/observe/claude"
)

func TestNoRecordWanted(t *testing.T) {
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == recordEnv {
				return v
			}
			return ""
		}
	}
	for _, tc := range []struct {
		flag bool
		val  string
		want bool
	}{
		{true, "", true},
		{false, "off", true},
		{false, " OFF ", true},
		{false, "on", false},
		{false, "0", false},
		{false, "", false},
	} {
		if got := noRecordWanted(tc.flag, env(tc.val)); got != tc.want {
			t.Errorf("flag %v, %s=%q: %v, want %v", tc.flag, recordEnv, tc.val, got, tc.want)
		}
	}
}

// The headers already set are kept in order; an X-Cortex-Record line already there is
// replaced, so exactly one goes out, set to off.
func TestWithRecordOff(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "X-Cortex-Record: off"},
		{"X-Team: a", "X-Team: a\nX-Cortex-Record: off"},
		{"X-Team: a\r\nX-Other: b\n", "X-Team: a\nX-Other: b\nX-Cortex-Record: off"},
		{"x-cortex-record: on\nX-Team: a", "X-Team: a\nX-Cortex-Record: off"},
	} {
		if got := withRecordOff(tc.in); got != tc.want {
			t.Errorf("withRecordOff(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// writeSettingsFile writes a Claude Code settings file at p.
func writeSettingsFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A settings env block that sets ANTHROPIC_CUSTOM_HEADERS would replace the child's value,
// so the opt-out refuses and names the file: the user's, and the project's two.
func TestAddRecordOff_RefusesASettingsOverride(t *testing.T) {
	for _, rel := range []string{"home/.claude/settings.json", "proj/.claude/settings.json", "proj/.claude/settings.local.json"} {
		t.Run(rel, func(t *testing.T) {
			root := t.TempDir()
			home, dir := filepath.Join(root, "home"), filepath.Join(root, "proj")
			// Another settings file with an env block but no override, to show only the key counts.
			writeSettingsFile(t, filepath.Join(home, ".claude", "settings.json"), `{"env":{"DEBUG":"1"}}`)
			p := filepath.Join(root, filepath.FromSlash(rel))
			writeSettingsFile(t, p, `{"env":{"ANTHROPIC_CUSTOM_HEADERS":"X-Team: a"}}`)
			inject := map[string]string{}
			err := addRecordOff(inject, "claude", home, dir, func(string) string { return "" })
			if err == nil || !strings.Contains(err.Error(), p) {
				t.Fatalf("addRecordOff = %v, want a refusal naming %s", err, p)
			}
			if _, set := inject[customHeadersEnv]; set {
				t.Fatalf("the child got %s despite the refusal", customHeadersEnv)
			}
		})
	}
}

// With no override, the child's value keeps what agentop's environment had.
func TestAddRecordOff_AppendsToTheEnvironment(t *testing.T) {
	root := t.TempDir()
	writeSettingsFile(t, filepath.Join(root, ".claude", "settings.json"), `{"env":{"DEBUG":"1"}}`)
	inject := map[string]string{}
	getenv := func(k string) string {
		if k == customHeadersEnv {
			return "X-Team: a"
		}
		return ""
	}
	if err := addRecordOff(inject, "claude", root, filepath.Join(root, "missing"), getenv); err != nil {
		t.Fatal(err)
	}
	if got, want := inject[customHeadersEnv], "X-Team: a\nX-Cortex-Record: off"; got != want {
		t.Fatalf("%s = %q, want %q", customHeadersEnv, got, want)
	}
}

// An unreadable settings file refuses: whether it overrides the value is unknown.
func TestAddRecordOff_RefusesAnUnreadableSettingsFile(t *testing.T) {
	root := t.TempDir()
	writeSettingsFile(t, filepath.Join(root, ".claude", "settings.json"), `{not json`)
	if err := addRecordOff(map[string]string{}, "claude", root, "", func(string) string { return "" }); err == nil {
		t.Fatal("a settings file that is not JSON was taken as setting nothing")
	}
}

// Claude Code reads its user settings from CLAUDE_CONFIG_DIR when that is set, so a file there
// that overrides the variable refuses too.
func TestAddRecordOff_ChecksClaudeConfigDir(t *testing.T) {
	root := t.TempDir()
	cfgDir := filepath.Join(root, "cfg")
	p := filepath.Join(cfgDir, "settings.json")
	writeSettingsFile(t, p, `{"env":{"ANTHROPIC_CUSTOM_HEADERS":"X-Team: a"}}`)
	getenv := func(k string) string {
		if k == claude.ConfigDirEnv {
			return cfgDir
		}
		return ""
	}
	err := addRecordOff(map[string]string{}, "claude", filepath.Join(root, "home"), "", getenv)
	if err == nil || !strings.Contains(err.Error(), p) {
		t.Fatalf("addRecordOff = %v, want a refusal naming %s", err, p)
	}
}

// Any child but Claude Code would ignore the header and be recorded, so it is refused. A path
// to claude, and --print (no child), are accepted.
func TestAddRecordOff_RefusesAChildThatIsNotClaudeCode(t *testing.T) {
	none := func(string) string { return "" }
	for _, cmd := range []string{"bob", "opencode", "curl", "/usr/bin/codex", "claude-wrapper"} {
		inject := map[string]string{}
		err := addRecordOff(inject, cmd, t.TempDir(), "", none)
		if err == nil || !strings.Contains(err.Error(), "would be recorded") {
			t.Errorf("%s: addRecordOff = %v, want a refusal", cmd, err)
		}
		if _, set := inject[customHeadersEnv]; set {
			t.Errorf("%s: the child got %s despite the refusal", cmd, customHeadersEnv)
		}
	}
	for _, cmd := range []string{"claude", "/opt/bin/claude", "claude.exe", ""} {
		if err := addRecordOff(map[string]string{}, cmd, t.TempDir(), "", none); err != nil {
			t.Errorf("%q: addRecordOff = %v, want it accepted", cmd, err)
		}
	}
}

// End to end through runExec: the child sees the header list with the opt-out appended, from
// the flag and from CORTEX_RECORD=off; without either, it sees its own value untouched.
func TestRunExec_NoRecordReachesTheChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	cfgPath, _ := execCfg(t)
	stats := execStats(t, cfgPath)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(claude.ConfigDirEnv, "")
	t.Chdir(t.TempDir())
	t.Setenv(customHeadersEnv, "X-Team: a")
	// A stand-in claude on PATH that prints what it was given: --no-record runs only Claude Code.
	bin := t.TempDir()
	writeSettingsFile(t, filepath.Join(bin, "claude"), "#!/bin/sh\nprintf '%s' \"$"+customHeadersEnv+"\"\n")
	if err := os.Chmod(filepath.Join(bin, "claude"), 0o755); err != nil { //nolint:gosec // test stub
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, tc := range []struct {
		name string
		args []string
		env  string
		want string
	}{
		{"flag", []string{"--no-record"}, "", "X-Team: a\nX-Cortex-Record: off"},
		{"env", nil, "off", "X-Team: a\nX-Cortex-Record: off"},
		{"neither", nil, "", "X-Team: a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(recordEnv, tc.env)
			args := append(append([]string{"--cortex-stats-url", stats}, tc.args...), "--", "claude")
			var stdout, stderr bytes.Buffer
			if code := runExec(args, &stdout, &stderr); code != 0 {
				t.Fatalf("exit %d: %s", code, stderr.String())
			}
			if got := stdout.String(); got != tc.want {
				t.Fatalf("child saw %s = %q, want %q", customHeadersEnv, got, tc.want)
			}
		})
	}
}

// Through runExec, a child that is not Claude Code is refused before it runs.
func TestRunExec_NoRecordRefusesOtherAgents(t *testing.T) {
	cfgPath, _ := execCfg(t)
	var stdout, stderr bytes.Buffer
	code := runExec([]string{"--cortex-stats-url", execStats(t, cfgPath), "--no-record", "--", "bob"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "bob would be recorded") {
		t.Fatalf("exit %d, stderr %q; want 1 and a refusal naming bob", code, stderr.String())
	}
}

// The refusal stops the launch: nothing runs.
func TestRunExec_NoRecordRefusesASettingsOverride(t *testing.T) {
	cfgPath, _ := execCfg(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(claude.ConfigDirEnv, "")
	t.Chdir(t.TempDir())
	writeSettingsFile(t, filepath.Join(home, ".claude", "settings.json"), `{"env":{"ANTHROPIC_CUSTOM_HEADERS":"X-Team: a"}}`)
	var stdout, stderr bytes.Buffer
	// claude need not exist: the refusal comes before the launch.
	code := runExec([]string{"--cortex-stats-url", execStats(t, cfgPath), "--no-record", "--", "claude"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), customHeadersEnv) {
		t.Fatalf("exit %d, stderr %q; want 1 and a refusal naming %s", code, stderr.String(), customHeadersEnv)
	}
}
