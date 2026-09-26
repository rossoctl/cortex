package clientstate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// write lays down a state record and the settings file it points at. currentCA == ""
// writes an env block with no CA variable at all.
func write(t *testing.T, dir, priorCA, currentCA string) string {
	t.Helper()
	settingsPath := filepath.Join(dir, "settings.json")
	env := map[string]any{"HTTPS_PROXY": "http://127.0.0.1:47600"}
	if currentCA != "" {
		env[CAEnvVar] = currentCA
	}
	b, err := json.Marshal(map[string]any{"env": env})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	prior := map[string]any{CAEnvVar: nil}
	if priorCA != "" {
		prior[CAEnvVar] = priorCA
	}
	sb, err := json.Marshal(State{Settings: settingsPath, Prior: nil})
	if err != nil {
		t.Fatal(err)
	}
	// Marshal Prior by hand so a nil entry survives as JSON null.
	var doc map[string]any
	if err := json.Unmarshal(sb, &doc); err != nil {
		t.Fatal(err)
	}
	doc["prior"] = prior
	sb, err = json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, RelPath)
	if err := os.WriteFile(statePath, sb, 0o600); err != nil {
		t.Fatal(err)
	}
	return statePath
}

// TestCurrentCA_IsTheLiveValueNotPrior is the whole reason this package exists. Prior
// is what abctl DISPLACED; reading it answers a different question and gets the
// important cases backwards — it flags clients that are already correct, and because
// abctl freezes the record on first write, it can never go quiet afterwards.
func TestCurrentCA_IsTheLiveValueNotPrior(t *testing.T) {
	dir := t.TempDir()
	statePath := write(t, dir, "/displaced/old.crt", "/live/current.crt")

	st, err := Load(statePath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := st.CurrentCA(); got != "/live/current.crt" {
		t.Errorf("CurrentCA() = %q, want the live value; reading Prior here would warn "+
			"about clients that are already configured correctly", got)
	}
	// Prior is still readable for its actual purpose (disable restoring a value).
	if p := st.Prior[CAEnvVar]; p == nil || *p != "/displaced/old.crt" {
		t.Error("Prior no longer carries the displaced value that disable needs")
	}
}

// TestLoad_MissingIsNotAnError: no record is a normal state — a first install, a
// machine where abctl never ran — and callers must be able to tell it apart from a
// record that exists and is broken.
func TestLoad_MissingIsNotAnError(t *testing.T) {
	st, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Errorf("Load of a missing record returned an error: %v", err)
	}
	if st != nil {
		t.Errorf("Load of a missing record returned %+v, want nil", st)
	}
}

// TestLoad_MalformedIsAnError: a truncated or hand-mangled record must be
// distinguishable from an absent one, so a caller can say so rather than silently
// behaving as though nothing was configured.
func TestLoad_MalformedIsAnError(t *testing.T) {
	p := filepath.Join(t.TempDir(), RelPath)
	if err := os.WriteFile(p, []byte("{{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Error("Load of malformed JSON returned no error")
	}
}

// TestCurrentCA_EmptyOnEveryFailure: this feeds diagnostics, so every broken shape
// answers "" rather than erroring or panicking. A nil receiver is included because a
// caller that ignores Load's nil-for-absent gets one.
func TestCurrentCA_EmptyOnEveryFailure(t *testing.T) {
	dir := t.TempDir()

	var nilState *State
	if got := nilState.CurrentCA(); got != "" {
		t.Errorf("nil receiver: %q, want \"\"", got)
	}

	if got := (&State{}).CurrentCA(); got != "" {
		t.Errorf("no settings path: %q, want \"\"", got)
	}
	if got := (&State{Settings: filepath.Join(dir, "nope.json")}).CurrentCA(); got != "" {
		t.Errorf("settings file absent: %q, want \"\"", got)
	}

	// Settings present but no CA variable in it.
	noKey, err := Load(write(t, dir, "", ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ca := noKey.CurrentCA(); ca != "" {
		t.Errorf("no %s in env: %q, want \"\"", CAEnvVar, ca)
	}

	// Settings that is not JSON, and a CA value that is not a string.
	for name, body := range map[string]string{
		"not json":       "not json at all",
		"env not object": `{"env":42}`,
		"ca not string":  `{"env":{"` + CAEnvVar + `":42}}`,
	} {
		t.Run(name, func(t *testing.T) {
			sp := filepath.Join(dir, name+".json")
			if err := os.WriteFile(sp, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := (&State{Settings: sp}).CurrentCA(); got != "" {
				t.Errorf("CurrentCA() = %q, want \"\"", got)
			}
		})
	}
}
