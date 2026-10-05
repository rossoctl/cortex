package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// planClaudeCodeEnable is what `agentop setup` calls before it asks anything, so the
// property that matters is that it touches nothing — not the settings, not a backup,
// not the state record.
func TestPlanClaudeCodeEnable_TouchesNothing(t *testing.T) {
	settings, cfg := fixture(t, settingsWithSecret)
	before, _ := os.ReadFile(settings)
	pl, err := planClaudeCodeEnable(settings, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.changes) != len(managedKeys) {
		t.Errorf("changes = %d, want %d (none of the keys are set yet)", len(pl.changes), len(managedKeys))
	}
	if after, _ := os.ReadFile(settings); !bytes.Equal(before, after) {
		t.Error("planning changed the settings file")
	}
	if _, err := os.Stat(settings + ".bak"); err == nil {
		t.Error("planning wrote a backup")
	}
}

func TestPlanClaudeCodeEnable_NoChangesOnceEnabled(t *testing.T) {
	settings, cfg := fixture(t, settingsWithSecret)
	var out, errb bytes.Buffer
	if code := claudeCodeEnable2(settings, cfg, "", true, &out, &errb); code != 0 {
		t.Fatalf("enable failed: %s", errb.String())
	}
	pl, err := planClaudeCodeEnable(settings, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.changes) != 0 {
		t.Errorf("changes after enable = %q, want none", pl.changes)
	}
}

func TestPlanClaudeCodeEnable_RefusesAForeignValue(t *testing.T) {
	settings, cfg := fixture(t, `{"env":{"HTTPS_PROXY":"http://corp:3128"}}`)
	_, err := planClaudeCodeEnable(settings, cfg)
	if err == nil || !strings.Contains(err.Error(), "Refusing to overwrite") {
		t.Errorf("err = %v, want the overwrite refusal", err)
	}
}

func TestApplyClaudeCodeEnable_WritesThePlanAndRecordsPrior(t *testing.T) {
	// A stale Cortex port, which isCortexValue lets enable replace. The prior value
	// then differs from the one written, so recording either is distinguishable.
	const stale = "http://127.0.0.1:47699"
	settings, cfg := fixture(t, `{"env":{"HTTPS_PROXY":"`+stale+`"}}`)
	state := filepath.Join(t.TempDir(), "state.json")
	pl, err := planClaudeCodeEnable(settings, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var errb bytes.Buffer
	if err := applyClaudeCodeEnable(pl, state, &errb); err != nil {
		t.Fatalf("apply: %v (%s)", err, errb.String())
	}
	env := readEnv(t, settings)
	for k, v := range pl.want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	// 47600 is the fixture's forward_proxy_addr.
	if env[envProxy] != "http://127.0.0.1:47600" {
		t.Errorf("%s = %q, want the stale value replaced by http://127.0.0.1:47600", envProxy, env[envProxy])
	}
	st, err := readState(state)
	if err != nil || st == nil {
		t.Fatalf("no readable state record at %s (err %v)", state, err)
	}
	if p := st.Prior[envProxy]; p == nil || *p != stale {
		t.Errorf("prior %s not recorded as the user's %q: %v", envProxy, stale, p)
	}
	if p, ok := st.Prior[envNoTelem]; !ok || p != nil {
		t.Errorf("prior %s should be recorded as absent", envNoTelem)
	}
}

func TestPlanApplyClaudeCodeDisable_RestoresWhatTheUserHad(t *testing.T) {
	// A stale Cortex port that enable replaces, so the user's value and the one
	// enable leaves behind differ: a skipped restore cannot pass for a restore.
	const stale = "http://127.0.0.1:47699"
	settings, cfg := fixture(t, `{"env":{"HTTPS_PROXY":"`+stale+`"}}`)
	state := filepath.Join(t.TempDir(), "state.json")
	var out, errb bytes.Buffer
	if code := claudeCodeEnable2(settings, cfg, state, true, &out, &errb); code != 0 {
		t.Fatalf("enable failed: %s", errb.String())
	}
	if got := readEnv(t, settings)[envProxy]; got == stale {
		t.Fatalf("fixture: enable left %s at the stale %q, so a restore is unobservable", envProxy, got)
	}
	before, _ := os.ReadFile(settings)
	pl, err := planClaudeCodeDisable(settings, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(settings); !bytes.Equal(before, after) {
		t.Error("planning a disable changed the settings file")
	}
	if len(pl.present) != len(managedKeys) {
		t.Errorf("present = %q, want every managed key", pl.present)
	}
	restored, err := applyClaudeCodeDisable(pl, state, &errb)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(restored, ",") != envProxy {
		t.Errorf("restored = %q, want only %s", restored, envProxy)
	}
	env := readEnv(t, settings)
	if env[envProxy] != stale {
		t.Errorf("%s = %q after disable, want the user's own %q put back", envProxy, env[envProxy], stale)
	}
	if _, ok := env[envNoTelem]; ok {
		t.Errorf("%s survived disable", envNoTelem)
	}
	if _, err := os.Stat(state); err == nil {
		t.Error("the state record survived disable")
	}
}

func TestApplyClaudeCodeDisable_KeepsManualEdits(t *testing.T) {
	// After enable, a hand-edited managed key must survive disable (#1289).
	settings, cfg := fixture(t, `{"env":{}}`)
	state := filepath.Join(t.TempDir(), "state.json")
	var out, errb bytes.Buffer
	if code := claudeCodeEnable2(settings, cfg, state, true, &out, &errb); code != 0 {
		t.Fatalf("enable failed: %s", errb.String())
	}

	const userProxy = "http://proxy.example:8080"
	doc, err := readSettings(settings)
	if err != nil {
		t.Fatal(err)
	}
	envRaw(doc)[envProxy] = userProxy
	if err := writeSettings(settings, doc); err != nil {
		t.Fatal(err)
	}

	errb.Reset()
	pl, err := planClaudeCodeDisable(settings, cfg)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := applyClaudeCodeDisable(pl, state, &errb)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range restored {
		if k == envProxy {
			t.Fatalf("disable restored %s despite a manual edit", envProxy)
		}
	}
	env := readEnv(t, settings)
	if env[envProxy] != userProxy {
		t.Fatalf("%s = %q, want manual edit %q", envProxy, env[envProxy], userProxy)
	}
	if !strings.Contains(errb.String(), "keeping your edit to "+envProxy) {
		t.Fatalf("stderr missing keep-edit notice: %q", errb.String())
	}
}
