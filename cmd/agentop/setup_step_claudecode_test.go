package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func claudeEnv(t *testing.T, settings string) (*setupEnv, string) {
	t.Helper()
	env := configEnv(t)
	path := filepath.Join(env.home, settingsRel)
	if settings != "" {
		writeExe(t, path, settings)
	}
	return env, path
}

func TestClaudeCodeStepFreshPreflight(t *testing.T) {
	env, _ := claudeEnv(t, `{"env":{"HTTPS_PROXY":"http://corp:3128"}}`)
	env.configFresh = true
	_, prob := claudeCodeStep{}.plan(env)
	if prob == nil || !strings.Contains(prob.reason, `HTTPS_PROXY is already set to "http://corp:3128"`) {
		t.Errorf("problem = %+v", prob)
	}
	env2, _ := claudeEnv(t, `{"env":{"HTTPS_PROXY":"http://127.0.0.1:47699"}}`)
	env2.configFresh = true
	if p, prob := (claudeCodeStep{}).plan(env2); prob != nil || p.verb != "route" {
		t.Errorf("a stale Cortex value refused on a fresh install: %+v %v", p, prob)
	}
}

func TestClaudeCodeStepAppliesAgainstTheConfigNowOnDiskAndUndoes(t *testing.T) {
	env, path := claudeEnv(t, `{"model":"opus"}`)
	env.configFresh = true
	if _, prob := (claudeCodeStep{}).plan(env); prob != nil {
		t.Fatal(prob)
	}
	if _, _, err := (configStep{}).apply(env, nil); err != nil { // what the config step does first
		t.Fatal(err)
	}
	_, u, err := claudeCodeStep{}.apply(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	if readEnv(t, path)[envProxy] != "http://127.0.0.1:47600" {
		t.Error("Claude Code was not routed")
	}
	if err := u.fn(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != `{"model":"opus"}` {
		t.Errorf("undo left %q", b)
	}
	for _, f := range []string{path + ".bak", filepath.Join(env.home, stateRel)} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("undo left %s", f)
		}
	}
}

func TestClaudeCodeStepDoneWhenRouted(t *testing.T) {
	env, _ := claudeEnv(t, "")
	env.configFresh = true
	if _, _, err := (configStep{}).apply(env, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (claudeCodeStep{}).apply(env, nil); err != nil {
		t.Fatal(err)
	}
	env.configFresh = false // a later run: the config is on disk now
	if p, prob := (claudeCodeStep{}).plan(env); prob != nil || !p.done {
		t.Errorf("a routed Claude Code planned a change: %+v %v", p, prob)
	}
}

func TestClaudeCodeStepUndoKeepsASymlinkedSettingsFile(t *testing.T) {
	env, path := claudeEnv(t, "")
	real := filepath.Join(env.home, "dotfiles", "claude.json")
	writeExe(t, real, `{}`)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, path); err != nil {
		t.Fatal(err)
	}
	env.configFresh = true
	if _, _, err := (configStep{}).apply(env, nil); err != nil {
		t.Fatal(err)
	}
	_, u, err := claudeCodeStep{}.apply(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := u.fn(); err != nil {
		t.Fatal(err)
	}
	if l, err := os.Readlink(path); err != nil || l != real {
		t.Errorf("settings.json is no longer the link: %q %v", l, err)
	}
}

// A fresh install's plan cannot tell that settings.json already holds the values
// the built-in config yields, as after a reinstall with ~/.cortex removed. Apply's
// re-plan finds nothing to change, so the file keeps its bytes and gets no .bak.
func TestClaudeCodeStepApplyLeavesAnAlreadyRoutedFileAlone(t *testing.T) {
	env, path := claudeEnv(t, "")
	env.configFresh = true
	if _, _, err := (configStep{}).apply(env, nil); err != nil {
		t.Fatal(err)
	}
	want, err := wanted(env.configPath())
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(map[string]any{"env": want})
	if err != nil {
		t.Fatal(err)
	}
	writeExe(t, path, string(b))
	detail, u, err := claudeCodeStep{}.apply(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(b) || detail != "Claude Code already routed" {
		t.Errorf("apply with nothing to change rewrote the file: %q (detail %q)", got, detail)
	}
	if u.fn != nil { // a rollback would rewrite the file, over any edit made since
		t.Error("apply with nothing to change returned an undo")
	}
	if _, err := os.Stat(path + ".bak"); err == nil {
		t.Error("apply with nothing to change wrote a .bak")
	}
}

// With a config on disk the plan is planClaudeCodeEnable's, and apply plans again
// rather than writing that plan's copy of the file, so an edit made to
// settings.json after the plan, as Claude Code makes to it itself, survives.
func TestClaudeCodeStepApplyKeepsAnEditMadeAfterThePlan(t *testing.T) {
	env, path := claudeEnv(t, `{"model":"opus"}`)
	env.configFresh = true
	if _, _, err := (configStep{}).apply(env, nil); err != nil {
		t.Fatal(err)
	}
	env.configFresh = false // a later run: the config is on disk now
	if p, prob := (claudeCodeStep{}).plan(env); prob != nil || p.verb != "route" {
		t.Fatalf("plan = %+v %v", p, prob)
	}
	writeExe(t, path, `{"model":"opus","theme":"dark"}`)
	if _, _, err := (claudeCodeStep{}).apply(env, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil || doc["theme"] != "dark" {
		t.Errorf("apply lost the edit made after the plan: %s", b)
	}
}

// onDiskConfig writes the built-in config, as the config step does, and makes env
// a later run's: one with a config to plan against.
func onDiskConfig(t *testing.T, env *setupEnv) {
	t.Helper()
	env.configFresh = true
	if _, _, err := (configStep{}).apply(env, nil); err != nil {
		t.Fatal(err)
	}
	env.configFresh = false
}

// The undo takes away a ~/.claude the write made, if nothing else has used it.
func TestClaudeCodeStepUndoRemovesTheClaudeDirItMade(t *testing.T) {
	env, path := claudeEnv(t, "")
	onDiskConfig(t, env)
	_, u, err := claudeCodeStep{}.apply(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := u.fn(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(path)); err == nil {
		t.Error("the undo left the ~/.claude the step made")
	}
}

// The consent row promises a .bak only when writeSettings will write one: there is
// a file to copy, and no .bak from before, which it never overwrites.
func TestClaudeCodeStepPromisesABakOnlyWhenOneIsWritten(t *testing.T) {
	for _, tc := range []struct {
		name, settings string
		bak, want      bool
	}{{"no settings.json", "", false, false}, {"settings.json", `{}`, false, true}, {"and a .bak", `{}`, true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			env, path := claudeEnv(t, tc.settings)
			if tc.bak {
				writeExe(t, path+".bak", `{}`)
			}
			env.configFresh = true
			p, prob := claudeCodeStep{}.plan(env)
			if prob != nil || strings.HasSuffix(p.where, " (.bak kept)") != tc.want {
				t.Errorf("consent row %q (%v), want a .bak named %v", p.where, prob, tc.want)
			}
		})
	}
}

// A value set between the plan and the apply is refused when the step applies,
// HOME as ~, with nothing changed and so nothing to undo.
func TestClaudeCodeStepApplyRefusesAValueSetSinceThePlan(t *testing.T) {
	env, path := claudeEnv(t, `{}`)
	onDiskConfig(t, env)
	if p, prob := (claudeCodeStep{}).plan(env); prob != nil || p.verb != "route" {
		t.Fatalf("plan = %+v %v", p, prob)
	}
	writeExe(t, path, `{"env":{"HTTPS_PROXY":"http://corp:3128"}}`)
	_, u, err := claudeCodeStep{}.apply(env, nil)
	var se stepError
	if !errors.As(err, &se) || se.reason != `HTTPS_PROXY is already set to "http://corp:3128" in ~/.claude/settings.json.` || u.fn != nil {
		t.Errorf("apply = %v (undo %v), want the refusal and no undo", err, u.fn != nil)
	}
}

// Refusals show HOME as ~, on a fresh install's plan and on one against a config.
func TestClaudeCodeStepRefusalsShowHomeAsTilde(t *testing.T) {
	env, _ := claudeEnv(t, `{not json`)
	env.configFresh = true
	if _, prob := (claudeCodeStep{}).plan(env); prob == nil || !strings.HasPrefix(prob.reason, "~/.claude/settings.json is not valid JSON") {
		t.Errorf("fresh problem = %+v", prob)
	}
	env2, _ := claudeEnv(t, `{"env":{"HTTPS_PROXY":"http://corp:3128"}}`)
	onDiskConfig(t, env2)
	if _, prob := (claudeCodeStep{}).plan(env2); prob == nil || !strings.HasSuffix(prob.reason, " in ~/.claude/settings.json.") {
		t.Errorf("problem = %+v", prob)
	}
}

// A config that will not load is reported once, by the config step.
func TestClaudeCodeStepLeavesAConfigThatWillNotLoadToTheConfigStep(t *testing.T) {
	env, _ := claudeEnv(t, `{}`)
	writeExe(t, env.configPath(), "listener: [\n")
	_, probs := planSteps(env, []step{configStep{}, claudeCodeStep{}})
	if len(probs) != 1 || probs[0].label != "config" {
		t.Errorf("problems = %+v, want the config step's alone", probs)
	}
}

// A prior-settings record enable could not write is its warning, passed on as a
// row under the routed line: disable will then delete the keys, not restore them.
func TestClaudeCodeStepPassesOnTheStateRecordWarning(t *testing.T) {
	env, _ := claudeEnv(t, `{}`)
	onDiskConfig(t, env)
	_, state := claudeCodeStep{}.paths(env)
	writeExe(t, state, "{not json")
	detail, _, err := claudeCodeStep{}.apply(env, nil)
	first, notes, _ := strings.Cut(detail, "\n")
	if err != nil || first != "Claude Code → Cortex" || !strings.HasPrefix(notes, "could not record prior settings (") ||
		strings.Contains(notes, env.home) {
		t.Errorf("apply = %q %v, want the warning, HOME as ~, under the first line", detail, err)
	}
}

// ageRecord rewrites the record at path as an agentop from before Written existed
// wrote it, keeping its Settings and Prior. It returns the record as it now reads.
func ageRecord(t *testing.T, path string) managedState {
	t.Helper()
	st, err := readState(path)
	if err != nil || st == nil {
		t.Fatalf("no record at %s (err %v)", path, err)
	}
	aged := managedState{Settings: st.Settings, Prior: st.Prior}
	b, err := json.MarshalIndent(aged, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return aged
}

// The installed population this fix has to reach was enabled before Written
// existed, and `install.sh --claude-code` is the only thing it runs again: that
// hands off to `agentop setup --claude-code`, which finds settings already routed,
// so the step is done and its apply never runs. already is the hook on that path,
// and without it the fix would reach almost nobody.
func TestClaudeCodeStepAlreadyTopsUpAPreWrittenRecord(t *testing.T) {
	env, path := claudeEnv(t, `{}`)
	onDiskConfig(t, env)
	if _, _, err := (claudeCodeStep{}).apply(env, nil); err != nil {
		t.Fatal(err)
	}
	_, state := claudeCodeStep{}.paths(env)
	aged := ageRecord(t, state)

	// The plan is "already routed" now, which is the path already serves.
	p, prob := claudeCodeStep{}.plan(env)
	if prob != nil || !p.done {
		t.Fatalf("plan = %+v %v, want done — this is not the already path", p, prob)
	}
	if notes := (claudeCodeStep{}).already(env); notes != "" {
		t.Errorf("already warned: %q", notes)
	}
	st, err := readState(state)
	if err != nil || st == nil {
		t.Fatalf("no record after the top-up (err %v)", err)
	}
	vals := readEnv(t, path)
	for _, k := range managedKeys {
		if got, recorded := st.Written[k]; !recorded || got != vals[k] {
			t.Errorf("written[%s] = %q (recorded %v), want the file's %q", k, got, recorded, vals[k])
		}
	}
	// Prior survives: it is still the only note of what predated Cortex.
	if len(st.Prior) != len(aged.Prior) {
		t.Errorf("prior is %v, want the %v the aged record held", st.Prior, aged.Prior)
	}
	// And the top-up reached disable, which is the whole point of it.
	pl, err := planClaudeCodeDisable(path, state)
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.left) != 0 {
		t.Errorf("disable would leave %q behind; a topped-up record should claim every key", pl.left)
	}
}

// already is reachable from setup only. plan must change nothing on disk, and
// `agentop doctor` plans every step to report on it — a top-up from there would
// have doctor writing files.
func TestClaudeCodeStepPlanWritesNoRecord(t *testing.T) {
	env, _ := claudeEnv(t, `{}`)
	onDiskConfig(t, env)
	if _, _, err := (claudeCodeStep{}).apply(env, nil); err != nil {
		t.Fatal(err)
	}
	_, state := claudeCodeStep{}.paths(env)
	ageRecord(t, state)
	before, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 { // as doctor plans it, twice for good measure
		if _, prob := (claudeCodeStep{}).plan(env); prob != nil {
			t.Fatal(prob)
		}
	}
	after, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("plan wrote to the record:\nwas %s\nnow %s", before, after)
	}
}

// A fresh config means this machine has never been enabled from here, so there is
// no record to top up and nothing to read the wanted values from either.
func TestClaudeCodeStepAlreadyIsSilentOnAFreshConfig(t *testing.T) {
	env, _ := claudeEnv(t, `{}`)
	env.configFresh = true
	if notes := (claudeCodeStep{}).already(env); notes != "" {
		t.Errorf("already = %q on a fresh config, want nothing", notes)
	}
	if _, state := (claudeCodeStep{}).paths(env); fileExists(state) {
		t.Error("already created a record on a fresh config")
	}
}
