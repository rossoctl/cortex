package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// olderEnable leaves settings and the record as an agentop from before #1729 did: the
// six keys plus CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1, recorded as Cortex's, with
// prior as the value the key had before (nil: absent).
func olderEnable(t *testing.T, prior *string) (settings, cfg, state string) {
	t.Helper()
	before := `{}`
	if prior != nil {
		before = `{"env":{"` + envNoTelem + `":"` + *prior + `"}}`
	}
	settings, cfg = fixture(t, before)
	state = filepath.Join(t.TempDir(), "state.json")
	var out, errb bytes.Buffer
	if code := claudeCodeEnable2(settings, cfg, state, true, &out, &errb); code != 0 {
		t.Fatalf("enable: %s", errb.String())
	}
	handEdit(t, settings, map[string]string{envNoTelem: "1"})
	st, err := readState(state)
	if err != nil || st == nil {
		t.Fatalf("no record: %v", err)
	}
	st.Written[envNoTelem], st.Prior[envNoTelem] = "1", prior
	b, _ := json.Marshal(st)
	if err := os.WriteFile(state, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return settings, cfg, state
}

func TestClaudeCodeEnable_RetiresTheKeyAnOlderEnableWrote(t *testing.T) {
	settings, cfg, state := olderEnable(t, nil)
	var out, errb bytes.Buffer
	if code := claudeCodeEnable2(settings, cfg, state, true, &out, &errb); code != 0 {
		t.Fatalf("enable: %s", errb.String())
	}
	if _, ok := readEnv(t, settings)[envNoTelem]; ok {
		t.Errorf("%s survived the enable that retires it", envNoTelem)
	}
	if !strings.Contains(out.String(), "Removes from") || strings.Contains(out.String(), "Adds to") {
		t.Errorf("the prompt does not say only what it removes:\n%s", out.String())
	}
	st, _ := readState(state)
	if _, ok := st.Written[envNoTelem]; ok {
		t.Errorf("the record still claims %s, so a later value of the user's would be removed", envNoTelem)
	}
	out.Reset()
	if code := claudeCodeEnable2(settings, cfg, state, true, &out, &errb); code != 0 || !strings.Contains(out.String(), "Already enabled") {
		t.Errorf("second enable exit %d:\n%s", code, out.String())
	}
}

// The user had the key before the older enable: it is theirs, so it stays.
func TestClaudeCodeEnable_KeepsARetiredKeyTheUserHadFirst(t *testing.T) {
	one := "1"
	settings, cfg, state := olderEnable(t, &one)
	var out, errb bytes.Buffer
	if code := claudeCodeEnable2(settings, cfg, state, true, &out, &errb); code != 0 {
		t.Fatalf("enable: %s", errb.String())
	}
	if got := readEnv(t, settings)[envNoTelem]; got != "1" {
		t.Errorf("%s = %q, want the user's own \"1\"", envNoTelem, got)
	}
	if !strings.Contains(out.String(), "Already enabled") {
		t.Errorf("enable changed settings to put back the value they already hold:\n%s", out.String())
	}
	if st, _ := readState(state); st != nil {
		if _, ok := st.Written[envNoTelem]; ok {
			t.Errorf("the record still claims %s", envNoTelem)
		}
	}
}

// With no record the "1" cannot be told from the user's own, so nothing removes it.
func TestClaudeCode_RetiredKeyWithoutARecordIsLeft(t *testing.T) {
	settings, cfg := fixture(t, `{"env":{"`+envNoTelem+`":"1"}}`)
	var out, errb bytes.Buffer
	if code := claudeCodeEnable2(settings, cfg, "", true, &out, &errb); code != 0 {
		t.Fatalf("enable: %s", errb.String())
	}
	if code := claudeCodeDisable2(settings, "", true, &out, &errb); code != 0 {
		t.Fatalf("disable: %s", errb.String())
	}
	if env := readEnv(t, settings); len(env) != 1 || env[envNoTelem] != "1" {
		t.Errorf("env = %v, want only the user's %s=1", env, envNoTelem)
	}
}

// Disable on an install the older enable left: the key goes with the other six.
func TestClaudeCodeDisable_RemovesTheRetiredKeyAnOlderEnableWrote(t *testing.T) {
	settings, _, state := olderEnable(t, nil)
	var out, errb bytes.Buffer
	if code := claudeCodeDisable2(settings, state, true, &out, &errb); code != 0 {
		t.Fatalf("disable: %s", errb.String())
	}
	if env := readEnv(t, settings); len(env) != 0 {
		t.Errorf("env = %v, want every key Cortex wrote removed", env)
	}
}

// After the retiring enable, a value the user sets by hand is theirs.
func TestClaudeCodeDisable_LeavesARetiredKeySetAfterRetirement(t *testing.T) {
	settings, cfg, state := olderEnable(t, nil)
	var out, errb bytes.Buffer
	if code := claudeCodeEnable2(settings, cfg, state, true, &out, &errb); code != 0 {
		t.Fatalf("enable: %s", errb.String())
	}
	handEdit(t, settings, map[string]string{envNoTelem: "1"})
	if code := claudeCodeDisable2(settings, state, true, &out, &errb); code != 0 {
		t.Fatalf("disable: %s", errb.String())
	}
	if got := readEnv(t, settings)[envNoTelem]; got != "1" {
		t.Errorf("%s = %q, want the user's later \"1\" left alone", envNoTelem, got)
	}
}
