package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// handEdit rewrites env keys in a settings file the way a user with an editor
// would: after enable, behind agentop's back, with no record of having done it.
func handEdit(t *testing.T, path string, kv map[string]string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	env, ok := doc["env"].(map[string]any)
	if !ok {
		t.Fatalf("%s has no env block to edit", path)
	}
	for k, v := range kv {
		env[k] = v
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

// enable has to record the values it WROTE, not only the ones it displaced:
// without them disable cannot tell its own value from one changed since, which is
// the whole of the check below.
func TestClaudeCodeEnable_RecordsWhatItWrote(t *testing.T) {
	settings, cfg := fixture(t, settingsWithSecret)
	state := filepath.Join(t.TempDir(), "state.json")
	var out, errb bytes.Buffer
	if code := claudeCodeEnable2(settings, cfg, state, true, &out, &errb); code != 0 {
		t.Fatalf("enable failed: %s", errb.String())
	}
	st, err := readState(state)
	if err != nil || st == nil {
		t.Fatalf("no readable record at %s (err %v)", state, err)
	}
	env := readEnv(t, settings)
	for _, k := range managedKeys {
		got, recorded := st.Written[k]
		if !recorded {
			t.Errorf("written[%s] not recorded", k)
			continue
		}
		if got != env[k] {
			t.Errorf("written[%s] = %q, but the file now holds %q", k, got, env[k])
		}
	}
}

// Prior and Written have opposite freshness rules, and a re-enable is where they
// part company: Prior must stay the FIRST enable's (only that run saw what
// predated Cortex), Written must follow the LATEST one. A stale Written would make
// disable read Cortex's own value as somebody's edit and refuse to clear it.
func TestClaudeCodeEnable_ReEnableRefreshesWrittenKeepsPrior(t *testing.T) {
	// A stale Cortex port, as TestApplyClaudeCodeEnable_WritesThePlanAndRecordsPrior
	// uses: enable refuses to overwrite a value that does not look like ours, so a
	// plain corporate proxy here would never get past the first enable.
	const mine = "http://127.0.0.1:47699"
	settings, cfg := fixture(t, `{"env":{"HTTPS_PROXY":"`+mine+`"}}`)
	state := filepath.Join(t.TempDir(), "state.json")
	var out, errb bytes.Buffer
	if code := claudeCodeEnable2(settings, cfg, state, true, &out, &errb); code != 0 {
		t.Fatalf("first enable failed: %s", errb.String())
	}
	first := readEnv(t, settings)[envProxy]

	// The proxy moves, as it does when someone edits listener.forward_proxy_addr.
	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	moved := strings.Replace(string(body), "47600", "47650", 1)
	if moved == string(body) {
		t.Fatal("fixture: the config has no 47600 to move")
	}
	if err := os.WriteFile(cfg, []byte(moved), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errb.Reset()
	if code := claudeCodeEnable2(settings, cfg, state, true, &out, &errb); code != 0 {
		t.Fatalf("second enable failed: %s", errb.String())
	}
	second := readEnv(t, settings)[envProxy]
	if second == first {
		t.Fatalf("fixture: both enables wrote %q, so a stale record is unobservable", second)
	}

	st, err := readState(state)
	if err != nil || st == nil {
		t.Fatalf("no readable record at %s (err %v)", state, err)
	}
	if p := st.Prior[envProxy]; p == nil || *p != mine {
		t.Errorf("prior %s = %v, want the user's own %q kept from the first enable", envProxy, p, mine)
	}
	if st.Written[envProxy] != second {
		t.Errorf("written %s = %q, want the second enable's %q", envProxy, st.Written[envProxy], second)
	}

	// And the point of refreshing it: disable still recognises its own value.
	pl, err := planClaudeCodeDisable(settings, state)
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.left) != 0 {
		t.Errorf("disable would leave %q behind; a refreshed record should claim every key", pl.left)
	}
}

// An install enabled before Written existed is the whole population this fix has to
// reach, and it never runs an enable that changes settings again: `install.sh
// --claude-code` re-runs on every upgrade and stops at "Already enabled". So that
// re-run tops the record up, and a later hand edit is then safe.
func TestClaudeCodeEnable_AlreadyEnabledTopsUpAPreWrittenRecord(t *testing.T) {
	settings, cfg := fixture(t, settingsWithSecret)
	state := filepath.Join(t.TempDir(), "state.json")
	var out, errb bytes.Buffer
	if code := claudeCodeEnable2(settings, cfg, state, true, &out, &errb); code != 0 {
		t.Fatalf("enable failed: %s", errb.String())
	}
	// Age the record to what an agentop from before this fix wrote.
	st, err := readState(state)
	if err != nil || st == nil {
		t.Fatalf("no record (err %v)", err)
	}
	aged := managedState{Settings: st.Settings, Prior: st.Prior}
	b, err := json.MarshalIndent(aged, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errb.Reset()
	if code := claudeCodeEnable2(settings, cfg, state, true, &out, &errb); code != 0 {
		t.Fatalf("re-enable failed: %s", errb.String())
	}
	if !strings.Contains(out.String(), "Already enabled") {
		t.Fatalf("fixture: the re-run wrote to settings, so it is not the top-up path:\n%s", out.String())
	}
	st, err = readState(state)
	if err != nil || st == nil {
		t.Fatalf("no record after the re-run (err %v)", err)
	}
	env := readEnv(t, settings)
	for _, k := range managedKeys {
		if got, recorded := st.Written[k]; !recorded || got != env[k] {
			t.Errorf("written[%s] = %q (recorded %v), want the file's %q", k, got, recorded, env[k])
		}
	}
	// Prior survived the top-up: it is still the only note of what predated Cortex.
	if len(st.Prior) != len(aged.Prior) {
		t.Errorf("prior is %v, want the %v the aged record held", st.Prior, aged.Prior)
	}

	// And an unchanged re-run after that writes nothing at all.
	before, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(state)
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := claudeCodeEnable2(settings, cfg, state, true, &out, &errb); code != 0 {
		t.Fatalf("third enable failed: %s", errb.String())
	}
	after, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("the record changed on an unchanged re-run:\nwas %s\nnow %s", before, after)
	}
	if now, err := os.Stat(state); err != nil || !now.ModTime().Equal(info.ModTime()) {
		t.Errorf("the record was rewritten on an unchanged re-run (mtime %v → %v, %v)",
			info.ModTime(), now.ModTime(), err)
	}
}

// A machine with no record must not gain one from a re-run. Settings already hold
// Cortex's values there, so a Prior taken from them would be Cortex's own — and
// disable would put those back instead of removing the keys.
func TestClaudeCodeEnable_AlreadyEnabledCreatesNoRecord(t *testing.T) {
	settings, cfg := fixture(t, settingsWithSecret)
	var out, errb bytes.Buffer
	if code := claudeCodeEnable2(settings, cfg, "", true, &out, &errb); code != 0 {
		t.Fatalf("enable failed: %s", errb.String())
	}
	state := filepath.Join(t.TempDir(), "state.json")
	out.Reset()
	if code := claudeCodeEnable2(settings, cfg, state, true, &out, &errb); code != 0 {
		t.Fatalf("re-enable failed: %s", errb.String())
	}
	if !strings.Contains(out.String(), "Already enabled") {
		t.Fatalf("fixture: not the top-up path:\n%s", out.String())
	}
	if _, err := os.Stat(state); err == nil {
		body, _ := os.ReadFile(state)
		t.Errorf("a record was created where there was none: %s", body)
	}
}

// The bug: disable restored the recorded prior value over whatever the key held
// now, so an edit made after enable was thrown away without a word.
func TestClaudeCodeDisable_LeavesAKeyChangedSinceEnable(t *testing.T) {
	const edited = "http://127.0.0.1:9999"
	settings, cfg := fixture(t, settingsWithSecret)
	state := filepath.Join(t.TempDir(), "state.json")
	var out, errb bytes.Buffer
	if code := claudeCodeEnable2(settings, cfg, state, true, &out, &errb); code != 0 {
		t.Fatalf("enable failed: %s", errb.String())
	}
	handEdit(t, settings, map[string]string{envProxy: edited})

	out.Reset()
	errb.Reset()
	if code := claudeCodeDisable2(settings, state, true, &out, &errb); code != 0 {
		t.Fatalf("disable exit %d: %s", code, errb.String())
	}
	env := readEnv(t, settings)
	if env[envProxy] != edited {
		t.Errorf("%s = %q, want the hand edit %q left alone", envProxy, env[envProxy], edited)
	}
	for _, k := range managedKeys {
		if k == envProxy {
			continue
		}
		if v, ok := env[k]; ok {
			t.Errorf("%s = %q survived disable; only the edited key should", k, v)
		}
	}
	// Still the user's file otherwise.
	if env["ANTHROPIC_AUTH_TOKEN"] != "sk-do-not-touch" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN = %q", env["ANTHROPIC_AUTH_TOKEN"])
	}
	// Said out loud: a value silently kept is as confusing as one silently lost.
	for _, want := range []string{envProxy, edited, "changed since Cortex set them"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output does not mention %q:\n%s", want, out.String())
		}
	}
	// And the record stays: it is the only note of what that key held before
	// enable, and the key is still set.
	if _, err := os.Stat(state); err != nil {
		t.Errorf("the record was deleted with a managed key still in the file: %v", err)
	}
}

// Every managed key edited: there is nothing of ours left to take out, so disable
// must change nothing at all rather than write the file to no effect.
func TestClaudeCodeDisable_EveryKeyChangedTouchesNothing(t *testing.T) {
	settings, cfg := fixture(t, settingsWithSecret)
	state := filepath.Join(t.TempDir(), "state.json")
	var out, errb bytes.Buffer
	if code := claudeCodeEnable2(settings, cfg, state, true, &out, &errb); code != 0 {
		t.Fatalf("enable failed: %s", errb.String())
	}
	edits := map[string]string{}
	for i, k := range managedKeys {
		edits[k] = "mine-" + string(rune('a'+i))
	}
	handEdit(t, settings, edits)
	before, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	stateBefore, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errb.Reset()
	if code := claudeCodeDisable2(settings, state, true, &out, &errb); code != 0 {
		t.Fatalf("disable exit %d: %s", code, errb.String())
	}
	if after, _ := os.ReadFile(settings); !bytes.Equal(before, after) {
		t.Errorf("settings.json changed:\nwas  %s\nnow  %s", before, after)
	}
	if after, err := os.ReadFile(state); err != nil || !bytes.Equal(stateBefore, after) {
		t.Errorf("the record changed: %q (%v)", after, err)
	}
	if !strings.Contains(out.String(), "Nothing to remove") {
		t.Errorf("output does not say there was nothing to remove:\n%s", out.String())
	}
	// Not the plain "Nothing to do": seven keys are set, they are just not ours.
	if strings.Contains(out.String(), "none of the Cortex variables are set") {
		t.Errorf("output claims no variables are set, but every managed key is:\n%s", out.String())
	}
}

// A record written before Written existed, or by an agentop that could not record
// it, must keep the old behaviour exactly: remove outright. Leaving a key behind on
// an unknown value would strand the client on a Cortex the user is turning off, and
// the next enable records Written and self-heals.
func TestClaudeCodeDisable_RecordWithoutWrittenStillRemoves(t *testing.T) {
	settings, _ := fixture(t, `{"env":{"HTTPS_PROXY":"http://127.0.0.1:47600","CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC":"1"}}`)
	state := filepath.Join(t.TempDir(), "state.json")
	if err := writeState(state, managedState{
		Settings: settings,
		Prior:    map[string]*string{envProxy: nil, envNoTelem: nil},
	}); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := claudeCodeDisable2(settings, state, true, &out, &errb); code != 0 {
		t.Fatalf("disable exit %d: %s", code, errb.String())
	}
	if env := readEnv(t, settings); len(env) != 0 {
		t.Errorf("env = %v, want every managed key removed", env)
	}
	if _, err := os.Stat(state); err == nil {
		t.Error("the record survived a disable that left nothing behind")
	}
}

// changedSinceEnable is the ownership check, and its no-record answer is the one
// worth pinning: "unknown" must read as "not changed", never as "changed".
func TestChangedSinceEnable(t *testing.T) {
	recorded := &managedState{
		Settings: "s",
		Prior:    map[string]*string{},
		Written:  map[string]string{envProxy: "http://127.0.0.1:47600"},
	}
	for _, tc := range []struct {
		name string
		st   *managedState
		key  string
		cur  string
		want bool
	}{
		{"no record at all", nil, envProxy, "http://elsewhere", false},
		{"key not in the record", recorded, envCACerts, "/some/ca.crt", false},
		{"still ours", recorded, envProxy, "http://127.0.0.1:47600", false},
		{"changed", recorded, envProxy, "http://127.0.0.1:9999", true},
		{"emptied", recorded, envProxy, "", true},
	} {
		if got := changedSinceEnable(tc.st, tc.key, tc.cur); got != tc.want {
			t.Errorf("%s: changedSinceEnable(%s, %q) = %v, want %v", tc.name, tc.key, tc.cur, got, tc.want)
		}
	}
}
