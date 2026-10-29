package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// codexFixture writes a Cortex config under its own temp dir and returns the
// paths a test needs: the dotenv file to edit (not created unless content is
// non-empty) and the config to read addresses from. Mirrors fixture() in
// cmd_claudecode_test.go, which this reuses the cortexCfg template from.
func codexFixture(t *testing.T, env string) (envPath, cfgPath string) {
	t.Helper()
	dir := t.TempDir()
	envPath = filepath.Join(dir, ".env")
	if env != "" {
		if err := os.WriteFile(envPath, []byte(env), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath = filepath.Join(dir, "config.yaml")
	body := strings.Replace(cortexCfg, "CADIR", filepath.Join(dir, "ca"), 1)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return envPath, cfgPath
}

// readCodexEnv reads path's content into a plain map, for assertions. Returns an
// empty map for a missing file, matching parseCodexEnv's own convention.
func readCodexEnv(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := parseCodexEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, k := range codexKeys {
		if v, ok := f.get(k); ok {
			out[k] = v
		}
	}
	return out
}

// TestCodexEnable_PreservesEverythingElse is the property that matters most: this
// file is Codex's own, and the whole risk of this command is collateral damage to
// lines it did not ask to touch.
func TestCodexEnable_PreservesEverythingElse(t *testing.T) {
	const existing = "# a comment Codex's own loader ignores\n" +
		"\n" +
		"OPENAI_API_KEY=sk-do-not-touch\n" +
		"SOME_OTHER_TOOLS_VAR=1\n"
	envPath, cfgPath := codexFixture(t, existing)

	var out, errb bytes.Buffer
	if code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}

	b, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(b)
	for _, want := range []string{
		"# a comment Codex's own loader ignores",
		"OPENAI_API_KEY=sk-do-not-touch",
		"SOME_OTHER_TOOLS_VAR=1",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("lost %q from the file:\n%s", want, content)
		}
	}

	env := readCodexEnv(t, envPath)
	if env["HTTP_PROXY"] != "http://127.0.0.1:47600" || env["HTTPS_PROXY"] != "http://127.0.0.1:47600" ||
		env["http_proxy"] != "http://127.0.0.1:47600" || env["https_proxy"] != "http://127.0.0.1:47600" {
		t.Errorf("proxy spellings = %+v", env)
	}
	if !strings.HasSuffix(env[envSSLCert], filepath.Join("ca", "bundle.crt")) {
		t.Errorf("%s = %q, want it under the config's ca_dir, naming the bundle not ca.crt", envSSLCert, env[envSSLCert])
	}
	if _, err := os.Stat(envPath + ".bak"); err != nil {
		t.Errorf("no backup written: %v", err)
	}
	if !strings.Contains(content, "# Added by agentop configure codex enable") {
		t.Error("no explanatory comment for the newly-appended keys")
	}
}

// TestCodexEnable_ReadsAddressesFromConfig: hardcoding the port would point Codex
// at nothing the moment someone edited their Cortex config.
func TestCodexEnable_ReadsAddressesFromConfig(t *testing.T) {
	envPath, cfgPath := codexFixture(t, "")
	body, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	moved := strings.Replace(string(body), "127.0.0.1:47600", "127.0.0.1:19999", 1)
	if err := os.WriteFile(cfgPath, []byte(moved), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	env := readCodexEnv(t, envPath)
	if env["HTTP_PROXY"] != "http://127.0.0.1:19999" {
		t.Errorf("HTTP_PROXY = %q, want the moved address", env["HTTP_PROXY"])
	}
}

// A second enable with nothing changed must be a no-op: no prompt needed (there is
// nothing to confirm), no rewritten file, no fresh backup.
func TestCodexEnable_Idempotent(t *testing.T) {
	envPath, cfgPath := codexFixture(t, "")
	statePath := filepath.Join(filepath.Dir(envPath), "state.json")

	var out1, errb1 bytes.Buffer
	if code := codexEnable(envPath, cfgPath, statePath, true, &out1, &errb1); code != 0 {
		t.Fatalf("first enable: exit %d: %s", code, errb1.String())
	}
	before, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}

	var out2, errb2 bytes.Buffer
	if code := codexEnable(envPath, cfgPath, statePath, false, &out2, &errb2); code != 0 {
		t.Fatalf("second enable: exit %d: %s", code, errb2.String())
	}
	if !strings.Contains(out2.String(), "Already enabled") {
		t.Errorf("second enable did not report already-enabled:\n%s", out2.String())
	}
	after, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("file changed on a no-op enable:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// A value the user set to something real — a corporate proxy, say — must block
// enable outright rather than being silently overwritten.
func TestCodexEnable_RefusesForeignValue(t *testing.T) {
	envPath, cfgPath := codexFixture(t, "HTTP_PROXY=http://corporate-proxy.example.com:3128\n")
	var out, errb bytes.Buffer
	code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (refusal)", code)
	}
	if !strings.Contains(errb.String(), "Refusing to overwrite") {
		t.Errorf("stderr does not explain the refusal: %q", errb.String())
	}
	env := readCodexEnv(t, envPath)
	if env["HTTP_PROXY"] != "http://corporate-proxy.example.com:3128" {
		t.Errorf("the foreign value was touched: %q", env["HTTP_PROXY"])
	}
}

// THE GUARD HAS TO SEE EVERY SHAPE DOTENVY CALLS A DEFINITION, not just the plain
// one. dotenvy — which Codex uses — accepts `export KEY=V`, leading whitespace, and
// spaces around `=`. A regex that missed those let enable exit 0 and APPEND a second
// definition; Codex's precedence is last-wins, so the appended Cortex line beat a
// corporate proxy someone set on purpose. Each row is a value enable must refuse to
// overwrite, and must leave byte-for-byte intact.
func TestCodexEnable_RefusesForeignValueInEveryDotenvySpelling(t *testing.T) {
	for _, line := range []string{
		"export HTTPS_PROXY=http://corp.example.com:3128\n",
		"  HTTPS_PROXY=http://corp.example.com:3128\n",
		"HTTPS_PROXY = http://corp.example.com:3128\n",
		"\texport  HTTPS_PROXY  =  http://corp.example.com:3128\n",
	} {
		t.Run(strings.TrimSpace(line), func(t *testing.T) {
			envPath, cfgPath := codexFixture(t, line)
			var out, errb bytes.Buffer
			code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb)
			if code != 1 {
				t.Fatalf("exit = %d, want 1 (refusal); stdout=%q", code, out.String())
			}
			b, err := os.ReadFile(envPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != line {
				t.Errorf("file changed.\n got: %q\nwant: %q", string(b), line)
			}
		})
	}
}

// SSL_CERT_FILE IS A SHARED NAME, which is the one real cost of writing the CA
// there instead of under Codex's own CODEX_-prefixed name: anything could already
// have put its own bundle in this file. Refusing rather than overwriting is what
// makes that trade safe, so the CA key gets its own collision test rather than
// relying on the proxy keys' — it is the key whose name changed.
func TestCodexEnable_RefusesAForeignSSLCertFile(t *testing.T) {
	const corporate = "/etc/ssl/corporate-bundle.crt"
	envPath, cfgPath := codexFixture(t, envSSLCert+"="+corporate+"\n")
	var out, errb bytes.Buffer
	code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (refusal); stdout=%q", code, out.String())
	}
	if !strings.Contains(errb.String(), envSSLCert) || !strings.Contains(errb.String(), corporate) {
		t.Errorf("the refusal should name the key and the value it refused to touch: %q", errb.String())
	}
	if got := readCodexEnv(t, envPath)[envSSLCert]; got != corporate {
		t.Errorf("%s = %q, want the untouched %q", envSSLCert, got, corporate)
	}
}

// A commented-out assignment is not a definition, and the widened regex must not
// have started treating it as one — that would make enable refuse on a file whose
// only mention of a key is a note to self.
func TestCodexEnable_IgnoresCommentedAssignments(t *testing.T) {
	envPath, cfgPath := codexFixture(t, "# HTTPS_PROXY=http://corp.example.com:3128\n#export HTTP_PROXY=http://x\n")
	var out, errb bytes.Buffer
	code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if got := readCodexEnv(t, envPath)["HTTPS_PROXY"]; !strings.Contains(got, "47600") {
		t.Errorf("HTTPS_PROXY = %q, want Cortex's proxy — a comment should not have blocked it", got)
	}
}

// A key defined on two lines is ambiguous: Codex's loader resolves a duplicate
// last-wins, so appending would quietly beat the earlier definition. enable refuses
// rather than pick a side.
func TestCodexEnable_RefusesAmbiguousDuplicateKey(t *testing.T) {
	envPath, cfgPath := codexFixture(t, "HTTP_PROXY=http://one.example.com\nHTTP_PROXY=http://two.example.com\n")
	var out, errb bytes.Buffer
	code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (refusal)", code)
	}
	if !strings.Contains(errb.String(), "HTTP_PROXY") || !strings.Contains(errb.String(), "more than once") {
		t.Errorf("stderr does not name the ambiguous key: %q", errb.String())
	}
}

// A value that is Cortex's own but stale — e.g. an earlier enable's address, now
// changed — must be captured as absent (Prior = nil) rather than as something to
// restore. Unfiltered capture here is the exact #1289-adjacent staleness bug this
// design avoids: disable must delete the stale value, not resurrect it.
func TestCodexEnable_FiltersStaleCortexValueFromPrior(t *testing.T) {
	// "127.0.0.1:4761" matches isCortexValue's loose shape check for envProxy
	// (it contains "127.0.0.1:476") while differing from the fixture's actual
	// configured port, 47600 — a stale-but-Cortex-shaped value.
	envPath, cfgPath := codexFixture(t, "HTTP_PROXY=http://127.0.0.1:4761\n")
	statePath := filepath.Join(filepath.Dir(envPath), "state.json")

	var out, errb bytes.Buffer
	if code := codexEnable(envPath, cfgPath, statePath, true, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	b, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var st managedState
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	if prior, ok := st.Prior["HTTP_PROXY"]; !ok || prior != nil {
		t.Errorf("Prior[HTTP_PROXY] = %v, want nil (recorded absent, not the stale value)", prior)
	}

	// Disable must now DELETE the key, not restore the stale address.
	var dOut, dErr bytes.Buffer
	if code := codexDisable(envPath, statePath, true, &dOut, &dErr); code != 0 {
		t.Fatalf("disable: exit %d: %s", code, dErr.String())
	}
	if strings.Contains(dOut.String(), "Restored") {
		t.Errorf("disable claimed to restore a stale Cortex value:\n%s", dOut.String())
	}
	env := readCodexEnv(t, envPath)
	if _, ok := env["HTTP_PROXY"]; ok {
		t.Errorf("HTTP_PROXY still set after disable: %q", env["HTTP_PROXY"])
	}
}

// applyCodexDisable's restore branch, exercised directly against a hand-written
// state file: this is the only way a non-nil Prior entry can occur given enable's
// own filtered capture (see TestCodexEnable_FiltersStaleCortexValueFromPrior), but
// the branch is real defensive code — a future relaxation of that filter, or a
// hand-edited record, must still restore correctly rather than only ever delete.
func TestCodexDisable_RestoresRecordedPriorValue(t *testing.T) {
	envPath, cfgPath := codexFixture(t, "")
	statePath := filepath.Join(filepath.Dir(envPath), "state.json")

	// Simulate a successful enable: the file holds the five managed keys at
	// their wanted values.
	var enOut, enErr bytes.Buffer
	if code := codexEnable(envPath, cfgPath, statePath, true, &enOut, &enErr); code != 0 {
		t.Fatalf("enable: exit %d: %s", code, enErr.String())
	}

	// Hand-craft a record claiming HTTP_PROXY held a real value before enable.
	prior := "http://real-corporate-proxy.example.com:3128"
	st := managedState{Settings: envPath, Prior: map[string]*string{
		"HTTP_PROXY":  &prior,
		"HTTPS_PROXY": nil,
		"http_proxy":  nil,
		"https_proxy": nil,
		envSSLCert:    nil,
	}}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, b, 0o600); err != nil {
		t.Fatal(err)
	}

	var dOut, dErr bytes.Buffer
	if code := codexDisable(envPath, statePath, true, &dOut, &dErr); code != 0 {
		t.Fatalf("disable: exit %d: %s", code, dErr.String())
	}
	if !strings.Contains(dOut.String(), "HTTP_PROXY") || !strings.Contains(dOut.String(), "Restored") {
		t.Errorf("disable did not report the restore: %q", dOut.String())
	}
	env := readCodexEnv(t, envPath)
	if env["HTTP_PROXY"] != prior {
		t.Errorf("HTTP_PROXY = %q, want the recorded prior value %q", env["HTTP_PROXY"], prior)
	}
	for _, k := range []string{"HTTPS_PROXY", "http_proxy", "https_proxy", envSSLCert} {
		if _, ok := env[k]; ok {
			t.Errorf("%s still set after disable: %q", k, env[k])
		}
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("state file not removed after a full disable")
	}
}

func TestCodexDisable_NothingToDo(t *testing.T) {
	envPath, _ := codexFixture(t, "")
	var out, errb bytes.Buffer
	if code := codexDisable(envPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "Nothing to do") {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestCodexStatus_ReportsSetAndUnset(t *testing.T) {
	envPath, cfgPath := codexFixture(t, "")
	var enOut, enErr bytes.Buffer
	if code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &enOut, &enErr); code != 0 {
		t.Fatalf("enable: exit %d: %s", code, enErr.String())
	}

	var out bytes.Buffer
	if code := codexStatus(envPath, cfgPath, &out); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "enabled in "+envPath) {
		t.Errorf("status does not report enabled:\n%s", out.String())
	}
}

// A bridge that is off means Cortex terminates no TLS, so there is nothing for Codex to
// trust and every https request would fail verification. enable must refuse before it
// writes anything, as claude-code's and OpenCode's do.
//
// codexWanted's OTHER refusal — a bridge with no ca_dir — has no test because it cannot
// be reached through a config that loads: config.Validate rejects
// "tls_bridge.mode=enabled requires ca_dir", and a bridge that is absent or disabled is
// caught by the check above it. It stays as defence against a caller that builds a
// Config in memory.
func TestCodexEnable_RefusesADisabledBridge(t *testing.T) {
	envPath, cfgPath := codexFixture(t, "")
	body, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	off := strings.Replace(string(body), "mode: enabled", "mode: disabled", 1)
	if err := os.WriteFile(cfgPath, []byte(off), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (refusal)", code)
	}
	if want := "agentop: " + errBridgeDisabled(cfgPath).Error() + "\n"; errb.String() != want {
		t.Errorf("stderr =\n%s\nwant\n%s", errb.String(), want)
	}
	// Refused before any write: the file it would have created must not exist.
	if _, serr := os.Stat(envPath); !os.IsNotExist(serr) {
		t.Errorf("a refused enable created %s", envPath)
	}
}

// Declining at the prompt changes nothing and exits 3 — distinct from 1, so a caller can
// tell a refusal (a normal outcome) from a failure. Both verbs prompt, so both are pinned.
func TestCodexDeclining(t *testing.T) {
	// codexConfirm is a var precisely so a test can answer it; `go test` inherits the
	// terminal it was launched from, so an unstubbed prompt would block on a human.
	declineAll := func(t *testing.T) *int {
		t.Helper()
		prompts := 0
		prev := codexConfirm
		t.Cleanup(func() { codexConfirm = prev })
		codexConfirm = func(io.Writer) bool { prompts++; return false }
		return &prompts
	}

	t.Run("enable", func(t *testing.T) {
		envPath, cfgPath := codexFixture(t, "")
		statePath := filepath.Join(filepath.Dir(envPath), "state.json")
		prompts := declineAll(t)

		var out, errb bytes.Buffer
		if code := codexEnable(envPath, cfgPath, statePath, false, &out, &errb); code != exitDeclined {
			t.Errorf("exit = %d, want %d", code, exitDeclined)
		}
		if !strings.HasSuffix(out.String(), "Not changed.\n") {
			t.Errorf("stdout = %q, want it to end with Not changed.", out.String())
		}
		if *prompts != 1 {
			t.Errorf("prompted %d times, want once", *prompts)
		}
		if _, serr := os.Stat(envPath); !os.IsNotExist(serr) {
			t.Errorf("a declined enable wrote %s", envPath)
		}
		if _, serr := os.Stat(statePath); !os.IsNotExist(serr) {
			t.Error("a declined enable recorded state")
		}
	})

	t.Run("disable", func(t *testing.T) {
		envPath, cfgPath := codexFixture(t, "")
		statePath := filepath.Join(filepath.Dir(envPath), "state.json")

		var enOut, enErr bytes.Buffer
		if code := codexEnable(envPath, cfgPath, statePath, true, &enOut, &enErr); code != 0 {
			t.Fatalf("enable: exit %d: %s", code, enErr.String())
		}
		before, err := os.ReadFile(envPath)
		if err != nil {
			t.Fatal(err)
		}
		prompts := declineAll(t)

		var out, errb bytes.Buffer
		if code := codexDisable(envPath, statePath, false, &out, &errb); code != exitDeclined {
			t.Errorf("exit = %d, want %d", code, exitDeclined)
		}
		if !strings.HasSuffix(out.String(), "Not changed.\n") {
			t.Errorf("stdout = %q, want it to end with Not changed.", out.String())
		}
		if *prompts != 1 {
			t.Errorf("prompted %d times, want once", *prompts)
		}
		after, err := os.ReadFile(envPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) {
			t.Errorf("a declined disable changed the file:\nbefore:\n%s\nafter:\n%s", before, after)
		}
		if _, serr := os.Stat(statePath); serr != nil {
			t.Errorf("a declined disable removed the record: %v", serr)
		}
	})
}

// Usage errors and the stdout/stderr split they turn on: an explicit --help is a successful
// answer (stdout, 0), anything malformed is an error (stderr, 2).
//
// Every case here returns before runCodex resolves a home directory, which is what keeps
// the real ~/.codex/.env out of reach. HOME is pointed at a temp dir anyway, so a case
// added later that does reach the dispatch cannot touch the real file either.
func TestCodex_Usage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct {
		name     string
		args     []string
		code     int
		onStdout bool
	}{
		{name: "no args", args: nil, code: 2},
		{name: "--help", args: []string{"--help"}, code: 0, onStdout: true},
		{name: "-h", args: []string{"-h"}, code: 0, onStdout: true},
		{name: "help after the action", args: []string{"status", "--help"}, code: 0, onStdout: true},
		{name: "unknown action", args: []string{"enabel"}, code: 2},
		// status writes nothing, so there is nothing for --yes to skip; registering it
		// there would be a flag that parses and does nothing.
		{name: "status --yes", args: []string{"status", "--yes"}, code: 2},
		{name: "a stray argument", args: []string{"enable", "now"}, code: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := runCodex(tc.args, &out, &errb)
			if code != tc.code {
				t.Errorf("exit %d, want %d; stderr %q", code, tc.code, errb.String())
			}
			if got := strings.Contains(out.String(), "agentop configure codex —"); got != tc.onStdout {
				t.Errorf("usage on stdout = %v, want %v:\n%s", got, tc.onStdout, out.String())
			}
			// An unknown action must say so rather than only print usage, so a typo
			// names itself.
			if tc.name == "unknown action" && !strings.Contains(errb.String(), "unknown codex action") {
				t.Errorf("stderr does not report the unknown action: %q", errb.String())
			}
		})
	}
}

// `configure codex` must be an alternate spelling, not a reimplementation —
// same property TestConfigure_ClaudeCodeReachesTheSameLogic pins for claude-code.
// enable/disable must be a ROUND TRIP, including the marker comment. It used to
// leak one line per cycle, so three cycles on a file holding one unrelated key left
// three markers behind — contradicting disable's promise to remove only what it
// added and leave everything else exactly as it was.
func TestCodexEnableDisable_RoundTripsWithoutLeakingTheMarker(t *testing.T) {
	const original = "OPENAI_API_KEY=sk-not-a-real-key\n"
	envPath, cfgPath := codexFixture(t, original)
	statePath := filepath.Join(filepath.Dir(envPath), "state.json")
	for i := range 3 {
		var out, errb bytes.Buffer
		if code := codexEnable(envPath, cfgPath, statePath, true, &out, &errb); code != 0 {
			t.Fatalf("cycle %d: enable exit = %d, stderr=%q", i, code, errb.String())
		}
		out.Reset()
		errb.Reset()
		if code := codexDisable(envPath, statePath, true, &out, &errb); code != 0 {
			t.Fatalf("cycle %d: disable exit = %d, stderr=%q", i, code, errb.String())
		}
	}
	b, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), codexMarker); n != 0 {
		t.Errorf("%d marker comment(s) left after 3 enable/disable cycles, want 0:\n%s", n, string(b))
	}
	if string(b) != original {
		t.Errorf("file did not round-trip.\n got: %q\nwant: %q", string(b), original)
	}
}

// A marker a user edited is theirs, not agentop's, so removeComment's exact match
// must leave it alone rather than reclaim anything that merely looks similar.
func TestCodexDisable_LeavesAnEditedMarkerAlone(t *testing.T) {
	edited := "# " + codexMarker + " (do not delete)\n"
	envPath, cfgPath := codexFixture(t, edited)
	statePath := filepath.Join(filepath.Dir(envPath), "state.json")
	var out, errb bytes.Buffer
	if code := codexEnable(envPath, cfgPath, statePath, true, &out, &errb); code != 0 {
		t.Fatalf("enable exit = %d, stderr=%q", code, errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := codexDisable(envPath, statePath, true, &out, &errb); code != 0 {
		t.Fatalf("disable exit = %d, stderr=%q", code, errb.String())
	}
	b, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "(do not delete)") {
		t.Errorf("the user's edited comment was removed:\n%s", string(b))
	}
}

// enable writes SSL_CERT_FILE and NOT CODEX_CA_CERTIFICATE. Codex's dotenv loader
// drops every CODEX_-prefixed key on purpose, so a CA written under that name is one
// Codex never sees — and the resulting failure is silent, because Codex falls back
// to tunnelling the request opaquely and still answers.
func TestCodexEnable_WritesSSLCertFileNotCodexPrefixedCA(t *testing.T) {
	envPath, cfgPath := codexFixture(t, "")
	var out, errb bytes.Buffer
	if code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, errb.String())
	}
	b, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "CODEX_") {
		t.Errorf("wrote a CODEX_-prefixed key, which Codex's dotenv loader discards:\n%s", string(b))
	}
	if got := readCodexEnv(t, envPath)[envSSLCert]; got == "" {
		t.Errorf("%s is unset, so Codex has no CA it can actually read", envSSLCert)
	}
}

// CODEX_HOME moves the file Codex reads, so it must move the file agentop writes.
// Ignoring it meant enable wrote a file Codex never loads while status read that
// same unused file and reported `enabled` — two halves agreeing with each other and
// disagreeing with Codex.
func TestDefaultCodexEnvPath_HonoursCodexHome(t *testing.T) {
	t.Setenv(codexHomeEnv, "/tmp/elsewhere")
	if got, want := defaultCodexEnvPath("/home/someone"), filepath.Join("/tmp/elsewhere", ".env"); got != want {
		t.Errorf("with %s set: got %q, want %q", codexHomeEnv, got, want)
	}
	// Blank is not set: fall back rather than write to "/.env".
	t.Setenv(codexHomeEnv, "   ")
	if got, want := defaultCodexEnvPath("/home/someone"), filepath.Join("/home/someone", codexEnvRel); got != want {
		t.Errorf("with %s blank: got %q, want %q", codexHomeEnv, got, want)
	}
	t.Setenv(codexHomeEnv, "")
	if got, want := defaultCodexEnvPath("/home/someone"), filepath.Join("/home/someone", codexEnvRel); got != want {
		t.Errorf("with %s unset: got %q, want %q", codexHomeEnv, got, want)
	}
}

func TestConfigure_CodexReachesTheSameLogic(t *testing.T) {
	envPath, _ := codexFixture(t, "")

	var newOut, newErr bytes.Buffer
	newCode := runConfigure([]string{"codex", "status", "--env", envPath}, &newOut, &newErr)

	var oldOut, oldErr bytes.Buffer
	oldCode := runCodex([]string{"status", "--env", envPath}, &oldOut, &oldErr)

	if newCode != oldCode {
		t.Errorf("exit codes differ: configure = %d, codex = %d", newCode, oldCode)
	}
	if newOut.String() != oldOut.String() {
		t.Errorf("stdout differs:\nconfigure:\n%s\ncodex:\n%s", newOut.String(), oldOut.String())
	}
}

func TestConfigureUsage_CodexPersists(t *testing.T) {
	if want := "  agentop configure codex enable  [--yes] [--env PATH] [--config PATH]\n" +
		"  agentop configure codex disable [--yes] [--env PATH]\n" +
		"  agentop configure codex status  [--env PATH] [--config PATH]\n"; !strings.Contains(configureUsage, want) {
		t.Errorf("configureUsage lacks %q", want)
	}
	prose := strings.Join(strings.Fields(configureUsage), " ")
	for _, want := range []string{
		`"agentop configure codex --help"`,
		"Five agents persist, by four different mechanisms.",
		"loads a dotenv file, ~/.codex/.env, on its own at startup",
	} {
		if !strings.Contains(prose, want) {
			t.Errorf("configureUsage lacks %q", want)
		}
	}
	for _, stale := range []string{
		"not yet persistent", `use "agentop exec -- codex"`, "coming soon",
	} {
		if strings.Contains(prose, stale) {
			t.Errorf("configureUsage still says %q", stale)
		}
	}
}
