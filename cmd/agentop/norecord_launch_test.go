package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
			err := addRecordOff(inject, home, dir, func(string) string { return "" })
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
	if err := addRecordOff(inject, root, filepath.Join(root, "missing"), getenv); err != nil {
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
	if err := addRecordOff(map[string]string{}, root, "", func(string) string { return "" }); err == nil {
		t.Fatal("a settings file that is not JSON was taken as setting nothing")
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
	t.Chdir(t.TempDir())
	t.Setenv(customHeadersEnv, "X-Team: a")
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
			args := append(append([]string{"--cortex-stats-url", stats}, tc.args...), "--", "/bin/sh", "-c", `printf '%s' "$`+customHeadersEnv+`"`)
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

// The refusal stops the launch: nothing runs.
func TestRunExec_NoRecordRefusesASettingsOverride(t *testing.T) {
	cfgPath, _ := execCfg(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Chdir(t.TempDir())
	writeSettingsFile(t, filepath.Join(home, ".claude", "settings.json"), `{"env":{"ANTHROPIC_CUSTOM_HEADERS":"X-Team: a"}}`)
	var stdout, stderr bytes.Buffer
	code := runExec([]string{"--cortex-stats-url", execStats(t, cfgPath), "--no-record", "--", "no-such-command-should-not-run"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), customHeadersEnv) {
		t.Fatalf("exit %d, stderr %q; want 1 and a refusal naming %s", code, stderr.String(), customHeadersEnv)
	}
}
