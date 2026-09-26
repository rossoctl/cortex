package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/tlsbridge"
)

// settingsWithSecret is shaped like a real settings.json: unrelated top-level
// keys, and an env block already holding a gateway URL and an auth token. Those
// must survive untouched — the whole risk of this command is collateral damage to
// a file the user did not ask us to reorganise.
const settingsWithSecret = `{
  "model": "opus",
  "permissions": {"allow": ["Bash(git:*)"]},
  "env": {
    "ANTHROPIC_BASE_URL": "https://gateway.example.com",
    "ANTHROPIC_AUTH_TOKEN": "sk-do-not-touch",
    "CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS": "1"
  }
}`

const cortexCfg = `mode: proxy-sidecar
listener:
  roles: [forward]
  forward_proxy_addr: "127.0.0.1:47600"
  session_api_addr: 127.0.0.1:47601
  health_addr: 127.0.0.1:47604
tls_bridge:
  mode: enabled
  ca_dir: "CADIR"
  generate_ca: true
pipeline:
  outbound:
    plugins:
      - name: inference-parser
`

func fixture(t *testing.T, settings string) (settingsPath, cfgPath string) {
	t.Helper()
	dir := t.TempDir()
	settingsPath = filepath.Join(dir, "settings.json")
	if settings != "" {
		if err := os.WriteFile(settingsPath, []byte(settings), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath = filepath.Join(dir, "config.yaml")
	body := strings.Replace(cortexCfg, "CADIR", filepath.Join(dir, "ca"), 1)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return settingsPath, cfgPath
}

func readEnv(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, b)
	}
	out := map[string]string{}
	if e, ok := doc["env"].(map[string]any); ok {
		for k, v := range e {
			if s, ok := v.(string); ok {
				out[k] = s
			}
		}
	}
	return out
}

// TestClaudeCodeEnable_PreservesEverythingElse is the property that matters most:
// this file routinely holds an API token, and we are editing it on the user's
// behalf.
func TestClaudeCodeEnable_PreservesEverythingElse(t *testing.T) {
	settings, cfg := fixture(t, settingsWithSecret)
	var out, errb bytes.Buffer
	if code := claudeCodeEnable(settings, cfg, true, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}

	env := readEnv(t, settings)
	if env["ANTHROPIC_AUTH_TOKEN"] != "sk-do-not-touch" {
		t.Errorf("auth token altered: %q", env["ANTHROPIC_AUTH_TOKEN"])
	}
	if env["ANTHROPIC_BASE_URL"] != "https://gateway.example.com" {
		t.Errorf("base URL altered: %q", env["ANTHROPIC_BASE_URL"])
	}
	if env["CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS"] != "1" {
		t.Error("an unrelated env entry was dropped")
	}
	// Addresses come from the Cortex config, not a hardcoded constant.
	if env[envProxy] != "http://127.0.0.1:47600" {
		t.Errorf("%s = %q", envProxy, env[envProxy])
	}
	if !strings.HasSuffix(env[envCACerts], filepath.Join("ca", "ca.crt")) {
		t.Errorf("%s = %q, want it under the config's ca_dir", envCACerts, env[envCACerts])
	}
	if env[envNoTelem] != "1" {
		t.Errorf("%s = %q", envNoTelem, env[envNoTelem])
	}

	// Unrelated top-level keys survive.
	b, _ := os.ReadFile(settings)
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"model", "permissions"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("top-level key %q was dropped", k)
		}
	}
	if _, err := os.Stat(settings + ".bak"); err != nil {
		t.Errorf("no backup written: %v", err)
	}
}

// TestClaudeCodeEnable_ReadsAddressesFromConfig: hardcoding 47600 would point
// Claude Code at nothing the moment someone edited their Cortex config.
func TestClaudeCodeEnable_ReadsAddressesFromConfig(t *testing.T) {
	settings, cfg := fixture(t, "{}")
	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	moved := strings.Replace(string(body), "127.0.0.1:47600", "127.0.0.1:19999", 1)
	if err := os.WriteFile(cfg, []byte(moved), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := claudeCodeEnable(settings, cfg, true, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if got := readEnv(t, settings)[envProxy]; got != "http://127.0.0.1:19999" {
		t.Errorf("%s = %q, want the config's port", envProxy, got)
	}
}

// TestClaudeCodeEnable_RefusesToClobberForeignProxy: someone behind a corporate
// proxy already has HTTPS_PROXY set. Replacing it would break their network and
// give no clue why.
func TestClaudeCodeEnable_RefusesToClobberForeignProxy(t *testing.T) {
	settings, cfg := fixture(t, `{"env":{"HTTPS_PROXY":"http://corp:3128"}}`)
	var out, errb bytes.Buffer
	if code := claudeCodeEnable(settings, cfg, true, &out, &errb); code == 0 {
		t.Fatal("accepted a foreign HTTPS_PROXY")
	}
	if !strings.Contains(errb.String(), "Refusing to overwrite") {
		t.Errorf("error did not explain itself: %q", errb.String())
	}
	if got := readEnv(t, settings)[envProxy]; got != "http://corp:3128" {
		t.Errorf("value was changed to %q despite the refusal", got)
	}
}

// TestClaudeCodeDisable_RemovesOnlyOurKeys pairs with the enable test: the off
// switch must not take the user's own settings with it.
func TestClaudeCodeDisable_RemovesOnlyOurKeys(t *testing.T) {
	settings, cfg := fixture(t, settingsWithSecret)
	var out, errb bytes.Buffer
	if code := claudeCodeEnable(settings, cfg, true, &out, &errb); code != 0 {
		t.Fatalf("enable: %s", errb.String())
	}
	if code := claudeCodeDisable(settings, true, &out, &errb); code != 0 {
		t.Fatalf("disable: %s", errb.String())
	}
	env := readEnv(t, settings)
	for _, k := range managedKeys {
		if _, ok := env[k]; ok {
			t.Errorf("%s survived disable", k)
		}
	}
	if env["ANTHROPIC_AUTH_TOKEN"] != "sk-do-not-touch" {
		t.Error("disable removed the user's token")
	}
	if env["ANTHROPIC_BASE_URL"] == "" {
		t.Error("disable removed the user's base URL")
	}
}

// TestClaudeCodeEnable_Idempotent: install.sh may run this on every invocation.
func TestClaudeCodeEnable_Idempotent(t *testing.T) {
	settings, cfg := fixture(t, settingsWithSecret)
	var out, errb bytes.Buffer
	if code := claudeCodeEnable(settings, cfg, true, &out, &errb); code != 0 {
		t.Fatalf("first: %s", errb.String())
	}
	first, _ := os.ReadFile(settings)
	out.Reset()
	if code := claudeCodeEnable(settings, cfg, true, &out, &errb); code != 0 {
		t.Fatalf("second: %s", errb.String())
	}
	second, _ := os.ReadFile(settings)
	if string(first) != string(second) {
		t.Error("second run changed the file")
	}
	if !strings.Contains(out.String(), "Already enabled") {
		t.Errorf("second run did not report it was already done: %q", out.String())
	}
}

// TestClaudeCodeEnable_MissingSettingsFileIsCreated: a fresh machine may have no
// settings.json at all.
func TestClaudeCodeEnable_MissingSettingsFileIsCreated(t *testing.T) {
	settings, cfg := fixture(t, "")
	var out, errb bytes.Buffer
	if code := claudeCodeEnable(settings, cfg, true, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if got := readEnv(t, settings)[envNoTelem]; got != "1" {
		t.Errorf("%s = %q", envNoTelem, got)
	}
}

// TestClaudeCodeEnable_RejectsBrokenJSON: overwriting a file we cannot parse
// would destroy settings we never read.
func TestClaudeCodeEnable_RejectsBrokenJSON(t *testing.T) {
	settings, cfg := fixture(t, `{"env": {`)
	var out, errb bytes.Buffer
	if code := claudeCodeEnable(settings, cfg, true, &out, &errb); code == 0 {
		t.Fatal("accepted unparseable settings")
	}
	if !strings.Contains(errb.String(), "not valid JSON") {
		t.Errorf("error did not name the problem: %q", errb.String())
	}
}

// TestConfirmFrom_OnlyExplicitYesApplies: the file holds API tokens, so anything
// ambiguous — including EOF — must decline.
func TestConfirmFrom_OnlyExplicitYesApplies(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"y\n", true}, {"Y\n", true}, {"yes\n", true}, {"YES\n", true},
		{"n\n", false}, {"no\n", false}, {"\n", false}, {"", false},
		{"maybe\n", false}, {"ya\n", false},
	} {
		var out bytes.Buffer
		if got := confirmFrom(strings.NewReader(tc.in), &out); got != tc.want {
			t.Errorf("confirmFrom(%q) = %v, want %v", tc.in, got, tc.want)
		}
		if !strings.Contains(out.String(), "[y/N]") {
			t.Errorf("prompt did not show the default: %q", out.String())
		}
	}
}

// TestClaudeCodeEnable_KeepsNonStringEnvValues: the env block is typed
// map[string]any in JSON, and a bool or number there is perfectly legal. An
// earlier version read the block into map[string]string and assigned the filtered
// copy back, so those entries vanished — while the help text promised every other
// entry was left exactly as it was. The all-strings fixture above cannot catch it.
func TestClaudeCodeEnable_KeepsNonStringEnvValues(t *testing.T) {
	settings, cfg := fixture(t, `{
	  "env": {
	    "ANTHROPIC_AUTH_TOKEN": "sk-keep",
	    "SOME_BOOL": true,
	    "SOME_NUMBER": 42,
	    "SOME_NULL": null,
	    "SOME_LIST": ["a", "b"],
	    "SOME_OBJECT": {"nested": "value"}
	  }
	}`)
	var out, errb bytes.Buffer
	if code := claudeCodeEnable(settings, cfg, true, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}

	b, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	env, ok := doc["env"].(map[string]any)
	if !ok {
		t.Fatal("env block is gone")
	}
	for _, k := range []string{"SOME_BOOL", "SOME_NUMBER", "SOME_NULL", "SOME_LIST", "SOME_OBJECT"} {
		if _, present := env[k]; !present {
			t.Errorf("non-string env entry %q was dropped", k)
		}
	}
	if v, _ := env["SOME_BOOL"].(bool); !v {
		t.Errorf("SOME_BOOL = %#v, want true", env["SOME_BOOL"])
	}
	if env["ANTHROPIC_AUTH_TOKEN"] != "sk-keep" {
		t.Error("token altered")
	}

	// And disable must not drop them either.
	out.Reset()
	if code := claudeCodeDisable(settings, true, &out, &errb); code != 0 {
		t.Fatalf("disable: %s", errb.String())
	}
	b, _ = os.ReadFile(settings)
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	env, _ = doc["env"].(map[string]any)
	for _, k := range []string{"SOME_BOOL", "SOME_LIST", "SOME_OBJECT"} {
		if _, present := env[k]; !present {
			t.Errorf("disable dropped non-string env entry %q", k)
		}
	}
}

// TestClaudeCodeEnable_BackupKeepsThePristineFile: the backup's whole value is
// being the version the user wrote. Refreshing it on every call replaced it with
// our own output after one enable/disable round trip.
func TestClaudeCodeEnable_BackupKeepsThePristineFile(t *testing.T) {
	settings, cfg := fixture(t, settingsWithSecret)
	var out, errb bytes.Buffer

	if code := claudeCodeEnable(settings, cfg, true, &out, &errb); code != 0 {
		t.Fatalf("enable: %s", errb.String())
	}
	if code := claudeCodeDisable(settings, true, &out, &errb); code != 0 {
		t.Fatalf("disable: %s", errb.String())
	}
	if code := claudeCodeEnable(settings, cfg, true, &out, &errb); code != 0 {
		t.Fatalf("re-enable: %s", errb.String())
	}

	bak, err := os.ReadFile(settings + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(bak, &doc); err != nil {
		t.Fatal(err)
	}
	env, _ := doc["env"].(map[string]any)
	for _, k := range managedKeys {
		if _, present := env[k]; present {
			t.Errorf("backup is not pristine: it contains %s, so the original was lost", k)
		}
	}
	if env["ANTHROPIC_AUTH_TOKEN"] != "sk-do-not-touch" {
		t.Error("backup lost the original token")
	}
}

// TestClaudeCodeEnable_WarnsWhenCAMissing: writing NODE_EXTRA_CA_CERTS for a file
// that does not exist fails silently at request time — traffic flows, nothing is
// parsed, nothing looks broken. Say it at the moment we create that situation.
func TestClaudeCodeEnable_WarnsWhenCAMissing(t *testing.T) {
	settings, cfg := fixture(t, "{}")
	var out, errb bytes.Buffer
	if code := claudeCodeEnable(settings, cfg, true, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "does not exist yet") {
		t.Errorf("no warning about the missing CA file:\n%s", out.String())
	}
}

// TestClaudeCodeEnable_SilentWhenCAPresent is the other half. An earlier version
// of this test used strings.Replace with a count of 0, which replaces nothing, so
// the CA stayed missing and this branch was never exercised — the assertion
// existed but could not fail.
func TestClaudeCodeEnable_SilentWhenCAPresent(t *testing.T) {
	settings, cfg := fixture(t, "{}")

	// Create the CA exactly where the config's ca_dir points.
	var out, errb bytes.Buffer
	if code := claudeCodeEnable(settings, cfg, true, &out, &errb); code != 0 {
		t.Fatalf("first pass: %s", errb.String())
	}
	envAfter := readEnv(t, settings)
	caPath := envAfter[envCACerts]
	if caPath == "" {
		t.Fatal("no CA path was written")
	}
	if err := os.MkdirAll(filepath.Dir(caPath), 0o700); err != nil {
		t.Fatal(err)
	}
	// Both trust files must exist to silence the notes: ca.crt for Node, and the
	// bundle for the tools whose CA variable replaces their trust store. They are
	// written by different steps, so each gets its own note and each must be
	// created here.
	for _, p := range []string{caPath, envAfter[envSSLCert]} {
		if p == "" {
			t.Fatal("no trust path was written")
		}
		if err := os.WriteFile(p, []byte("-----BEGIN CERTIFICATE-----\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Re-run against a clean settings file, same config, now that the CA exists.
	settings2 := filepath.Join(filepath.Dir(caPath), "..", "settings2.json")
	if err := os.WriteFile(settings2, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errb.Reset()
	if code := claudeCodeEnable(settings2, cfg, true, &out, &errb); code != 0 {
		t.Fatalf("second pass: %s", errb.String())
	}
	if strings.Contains(out.String(), "does not exist yet") {
		t.Errorf("warned about a CA file that exists at %s:\n%s", caPath, out.String())
	}
}

// TestClaudeCodeEnable_HandlesIPv6ForwardProxy: strings.Cut split at the first
// colon, so "[::1]:47600" became host="[" and the value written into
// settings.json was a malformed http://[:1]:47600 — a broken proxy setting rather
// than an error, in the file this command works hardest to keep correct.
func TestClaudeCodeEnable_HandlesIPv6ForwardProxy(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want string
	}{
		{"127.0.0.1:47600", "http://127.0.0.1:47600"},
		{"[::1]:47600", "http://[::1]:47600"},
		{"[fe80::1]:8081", "http://[fe80::1]:8081"},
		// Wildcards are rewritten to something dialable.
		{":8081", "http://localhost:8081"},
		{"0.0.0.0:8081", "http://localhost:8081"},
		{"[::]:8081", "http://localhost:8081"},
	} {
		settings, cfg := fixture(t, "{}")
		body, err := os.ReadFile(cfg)
		if err != nil {
			t.Fatal(err)
		}
		moved := strings.Replace(string(body), "127.0.0.1:47600", tc.addr, 1)
		if err := os.WriteFile(cfg, []byte(moved), 0o600); err != nil {
			t.Fatal(err)
		}
		var out, errb bytes.Buffer
		if code := claudeCodeEnable(settings, cfg, true, &out, &errb); code != 0 {
			t.Errorf("%s: exit %d: %s", tc.addr, code, errb.String())
			continue
		}
		if got := readEnv(t, settings)[envProxy]; got != tc.want {
			t.Errorf("forward_proxy_addr %q -> %s=%q, want %q", tc.addr, envProxy, got, tc.want)
		}
	}
}

// TestClaudeCodeEnable_RejectsMalformedForwardProxy: an unparseable address must
// be reported, not written as a URL that silently never connects.
func TestClaudeCodeEnable_RejectsMalformedForwardProxy(t *testing.T) {
	settings, cfg := fixture(t, "{}")
	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	moved := strings.Replace(string(body), "127.0.0.1:47600", "not-a-host-port", 1)
	if err := os.WriteFile(cfg, []byte(moved), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := claudeCodeEnable(settings, cfg, true, &out, &errb); code == 0 {
		t.Fatal("accepted a malformed forward_proxy_addr")
	}
	if !strings.Contains(errb.String(), "is not host:port") {
		t.Errorf("error did not name the problem: %q", errb.String())
	}
}

// TestClaudeCodeEnable_NullSettingsRoot: `null` is valid JSON that unmarshals to
// a nil map, and assigning into one panics. A file someone truncated or a tool
// wrote badly should not crash the command.
func TestClaudeCodeEnable_NullSettingsRoot(t *testing.T) {
	settings, cfg := fixture(t, "null")
	var out, errb bytes.Buffer
	code := claudeCodeEnable(settings, cfg, true, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if got := readEnv(t, settings)[envNoTelem]; got != "1" {
		t.Errorf("%s = %q after a null root", envNoTelem, got)
	}
}

// TestClaudeCodeDeclineUsesADistinctExitCode: the installer treats a refusal as
// "skipped" and anything else as a failure it must report. One shared code made
// a genuine error — refusing to clobber a corporate proxy, unparseable settings —
// look like the user having said no, and the installer then exited 0 with Claude
// Code unconfigured.
func TestClaudeCodeDeclineUsesADistinctExitCode(t *testing.T) {
	// An operational failure: HTTPS_PROXY already set to something foreign.
	settings, cfg := fixture(t, `{"env":{"HTTPS_PROXY":"http://corp:3128"}}`)
	var out, errb bytes.Buffer
	if code := claudeCodeEnable(settings, cfg, true, &out, &errb); code != 1 {
		t.Errorf("clobber refusal exit = %d, want 1 (a failure, not a decline)", code)
	}

	// Unparseable settings is also a failure, not a decline.
	settings2, cfg2 := fixture(t, `{"env": {`)
	out.Reset()
	errb.Reset()
	if code := claudeCodeEnable(settings2, cfg2, true, &out, &errb); code != 1 {
		t.Errorf("bad-JSON exit = %d, want 1", code)
	}

	// And exitDeclined must not collide with either.
	if exitDeclined == 0 || exitDeclined == 1 || exitDeclined == 2 {
		t.Errorf("exitDeclined = %d collides with success, failure or usage", exitDeclined)
	}
}

// TestClaudeCodeDisable_RestoresAValueTheUserSetFirst is the ownership property.
// A user who already had CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 lost it on
// disable, because "1" is byte-identical to what we write and nothing recorded
// that it predated us.
func TestClaudeCodeDisable_RestoresAValueTheUserSetFirst(t *testing.T) {
	settings, cfg := fixture(t, `{"env":{
	  "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC":"1",
	  "ANTHROPIC_AUTH_TOKEN":"sk-x"
	}}`)
	state := filepath.Join(t.TempDir(), "claude-code-state.json")

	var out, errb bytes.Buffer
	if code := claudeCodeEnable2(settings, cfg, state, true, &out, &errb); code != 0 {
		t.Fatalf("enable: %s", errb.String())
	}
	if code := claudeCodeDisable2(settings, state, true, &out, &errb); code != 0 {
		t.Fatalf("disable: %s", errb.String())
	}

	env := readEnv(t, settings)
	if got, ok := env[envNoTelem]; !ok || got != "1" {
		t.Errorf("%s = %q present=%v; the user set this before enable and it must survive",
			envNoTelem, got, ok)
	}
	// The keys we genuinely added are still removed.
	for _, k := range []string{envProxy, envCACerts} {
		if _, ok := env[k]; ok {
			t.Errorf("%s survived disable although we added it", k)
		}
	}
	if env["ANTHROPIC_AUTH_TOKEN"] != "sk-x" {
		t.Error("unrelated entry lost")
	}
}

// TestClaudeCodeEnable_StateRecordedOnlyOnce: a second enable must not overwrite
// the ownership record with our own values, or the original is lost exactly when
// it is needed.
func TestClaudeCodeEnable_StateRecordedOnlyOnce(t *testing.T) {
	settings, cfg := fixture(t, `{"env":{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC":"1"}}`)
	state := filepath.Join(t.TempDir(), "state.json")

	var out, errb bytes.Buffer
	for i := 0; i < 3; i++ {
		if code := claudeCodeEnable2(settings, cfg, state, true, &out, &errb); code != 0 {
			t.Fatalf("enable %d: %s", i, errb.String())
		}
	}
	st, err := readState(state)
	if err != nil || st == nil {
		t.Fatalf("no state recorded: %v", err)
	}
	prior, recorded := st.Prior[envNoTelem]
	if !recorded || prior == nil || *prior != "1" {
		t.Errorf("prior for %s = %v, want the user's original \"1\"", envNoTelem, prior)
	}
	// And the keys we added are recorded as absent-before.
	if p, ok := st.Prior[envProxy]; !ok || p != nil {
		t.Errorf("prior for %s = %v, want nil (absent before)", envProxy, p)
	}
}

// TestClaudeCodeDisable_NoStateFallsBackToRemoval: enabled by an older abctl, or
// the record was lost. Removing is what this always did, and is better than
// leaving the proxy pointed at a Cortex the user is trying to turn off.
func TestClaudeCodeDisable_NoStateFallsBackToRemoval(t *testing.T) {
	settings, cfg := fixture(t, `{"env":{"ANTHROPIC_AUTH_TOKEN":"sk-x"}}`)
	var out, errb bytes.Buffer
	if code := claudeCodeEnable(settings, cfg, true, &out, &errb); code != 0 {
		t.Fatalf("enable: %s", errb.String())
	}
	if code := claudeCodeDisable2(settings, filepath.Join(t.TempDir(), "absent.json"), true, &out, &errb); code != 0 {
		t.Fatalf("disable: %s", errb.String())
	}
	env := readEnv(t, settings)
	for _, k := range managedKeys {
		if _, ok := env[k]; ok {
			t.Errorf("%s survived disable with no state file", k)
		}
	}
	if env["ANTHROPIC_AUTH_TOKEN"] != "sk-x" {
		t.Error("unrelated entry lost")
	}
}

// TestClaudeCodeDisable_WarnsOnCorruptState: a truncated record looked identical
// to no record, so disable silently fell back to deleting every managed key —
// re-opening the exact data loss the record was added to prevent. It still
// proceeds (the user asked for this off) but must say what is being lost.
func TestClaudeCodeDisable_WarnsOnCorruptState(t *testing.T) {
	settings, cfg := fixture(t, `{"env":{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC":"1"}}`)
	state := filepath.Join(t.TempDir(), "state.json")

	var out, errb bytes.Buffer
	if code := claudeCodeEnable2(settings, cfg, state, true, &out, &errb); code != 0 {
		t.Fatalf("enable: %s", errb.String())
	}
	// A partial write, a disk-full, a hand edit.
	if err := os.WriteFile(state, []byte(`{"settings":"`), 0o600); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errb.Reset()
	if code := claudeCodeDisable2(settings, state, true, &out, &errb); code != 0 {
		t.Fatalf("disable: %s", errb.String())
	}
	if !strings.Contains(errb.String(), "cannot read the record") {
		t.Errorf("corrupt state produced no warning:\nstderr=%q", errb.String())
	}
	if !strings.Contains(errb.String(), "not recoverable") {
		t.Errorf("warning does not say what is lost:\nstderr=%q", errb.String())
	}
}

// TestReadState_AbsentIsNotAnError: the silent fallback is correct for a machine
// that enabled with an older abctl, and must stay silent — a warning on every
// disable would be noise that trains people to ignore it.
func TestReadState_AbsentIsNotAnError(t *testing.T) {
	st, err := readState(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Errorf("absent state reported as an error: %v", err)
	}
	if st != nil {
		t.Error("absent state returned a record")
	}
}

// TestWriteState_RefusesToOverwriteAnUnreadableRecord: if it can be repaired by
// hand it is still the only copy of what the user had.
func TestWriteState_RefusesToOverwriteAnUnreadableRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"settings":"`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := writeState(path, managedState{Settings: "/x/settings.json", Prior: map[string]*string{}})
	if err == nil {
		t.Fatal("overwrote an unreadable record")
	}
	b, _ := os.ReadFile(path)
	if string(b) != `{"settings":"` {
		t.Errorf("the unreadable record was modified: %q", b)
	}
}

// TestClaudeCodeEnable_ReplacingVarsGetTheBundleNotTheCA is the regression guard
// for the bug this set of keys exists to fix. NODE_EXTRA_CA_CERTS EXTENDS Node's
// trust store, so ca.crt is right for it. Every other CA variable REPLACES the
// trust store, so ca.crt would leave that tool trusting one private CA and
// nothing else — breaking all unproxied TLS. The failure is platform-split
// (macOS keeps working via root_darwin.go, Linux and CI break), so a human is
// unlikely to catch a regression here by running it locally.
func TestClaudeCodeEnable_ReplacingVarsGetTheBundleNotTheCA(t *testing.T) {
	settings, cfg := fixture(t, "{}")
	var out, errb bytes.Buffer
	if code := claudeCodeEnable(settings, cfg, true, &out, &errb); code != 0 {
		t.Fatalf("enable: %s", errb.String())
	}
	env := readEnv(t, settings)

	if got := filepath.Base(env[envCACerts]); got != "ca.crt" {
		t.Errorf("%s = %q, want ca.crt (Node's setting is additive)", envCACerts, got)
	}
	for _, k := range bundleKeys {
		got := env[k]
		if got == "" {
			t.Errorf("%s was not written", k)
			continue
		}
		if filepath.Base(got) != tlsbridge.TrustBundleName {
			t.Errorf("%s = %q, want %s — this variable replaces the trust store, so "+
				"the bare CA would break every direct TLS call", k, got, tlsbridge.TrustBundleName)
		}
	}
	// All four must agree; a split would trust different sets per tool.
	for _, k := range bundleKeys[1:] {
		if env[k] != env[bundleKeys[0]] {
			t.Errorf("%s = %q disagrees with %s = %q", k, env[k], bundleKeys[0], env[bundleKeys[0]])
		}
	}
}

// TestClaudeCodeEnable_RefusesToClobberUserSSLCertFile: someone behind a
// corporate CA already has SSL_CERT_FILE set. Replacing it would break their TLS
// and give no clue why — the same property TestClaudeCodeEnable_
// RefusesToClobberForeignProxy asserts for HTTPS_PROXY, now that the CA keys are
// managed too.
//
// A foreign value can only ever reach the refusal, never the restore path, which
// is why the round-trip below needs a cortex-shaped prior value instead.
func TestClaudeCodeEnable_RefusesToClobberUserSSLCertFile(t *testing.T) {
	settings, cfg := fixture(t, `{"env":{"SSL_CERT_FILE":"/corp/ca.pem"}}`)
	var out, errb bytes.Buffer

	if code := claudeCodeEnable(settings, cfg, true, &out, &errb); code == 0 {
		t.Fatalf("enable overwrote a user-set %s; stdout:\n%s", envSSLCert, out.String())
	}
	if !strings.Contains(errb.String(), envSSLCert) {
		t.Errorf("refusal did not name %s: %s", envSSLCert, errb.String())
	}
	if env := readEnv(t, settings); env[envSSLCert] != "/corp/ca.pem" {
		t.Errorf("%s = %q, want the user's value untouched", envSSLCert, env[envSSLCert])
	}
}

// TestClaudeCodeDisable_RestoresPriorCAValues is the restore path for the CA keys
// this change added to managedKeys. Both the capture in enable and the loop in
// disable iterate managedKeys, so the new keys come along for free — but nothing
// pinned that, and the bookkeeping is where a partial addition would go wrong
// silently: a key added to the write path but missed by the restore path deletes
// something the user owned.
//
// Uses a cortex-SHAPED prior value (a stale bundle path under ~/.cortex), because
// isCortexValue accepts it and enable proceeds to overwrite. A foreign value is
// refused outright and never reaches the state file, so it cannot exercise this.
func TestClaudeCodeDisable_RestoresPriorCAValues(t *testing.T) {
	stale := filepath.Join("/somewhere", ".cortex", "ca", "old-bundle.crt")
	settings, cfg := fixture(t, `{"env":{
	  "SSL_CERT_FILE":"`+stale+`",
	  "GIT_SSL_CAINFO":"`+stale+`",
	  "ANTHROPIC_AUTH_TOKEN":"sk-x"
	}}`)
	state := filepath.Join(t.TempDir(), "claude-code-state.json")
	var out, errb bytes.Buffer

	if code := claudeCodeEnable2(settings, cfg, state, true, &out, &errb); code != 0 {
		t.Fatalf("enable: %s", errb.String())
	}
	// Enable replaced them with the current bundle path.
	if got := readEnv(t, settings)[envSSLCert]; got == stale {
		t.Fatalf("%s was not updated, so the restore below proves nothing", envSSLCert)
	}
	if code := claudeCodeDisable2(settings, state, true, &out, &errb); code != 0 {
		t.Fatalf("disable: %s", errb.String())
	}

	env := readEnv(t, settings)
	for _, k := range []string{envSSLCert, envGitCA} {
		if got := env[k]; got != stale {
			t.Errorf("%s = %q, want the prior value %q restored", k, got, stale)
		}
	}
	// Keys the user did NOT have are still removed rather than left behind.
	for _, k := range []string{envRequestsCA, envCurlCA, envProxy, envCACerts} {
		if _, ok := env[k]; ok {
			t.Errorf("%s survived disable although we added it", k)
		}
	}
	if env["ANTHROPIC_AUTH_TOKEN"] != "sk-x" {
		t.Error("unrelated entry lost")
	}
}

// TestDarwinGoNoteNamesTheKeychainRemedy: SSL_CERT_FILE is inert on macOS, so the
// note is the only place a user learns that the Go tools need the keychain. If it
// stops naming the command, the gap goes back to being silent.
func TestDarwinGoNoteNamesTheKeychainRemedy(t *testing.T) {
	note := darwinGoNote("/Users/x/.cortex/ca/ca.crt")
	for _, want := range []string{
		"security add-trusted-cert", "login.keychain-db",
		"/Users/x/.cortex/ca/ca.crt", "inert",
	} {
		if !strings.Contains(note, want) {
			t.Errorf("note is missing %q:\n%s", want, note)
		}
	}
	// It must not claim the other tools are broken — they are not.
	if strings.Contains(note, "git, curl and Python are broken") {
		t.Error("note overstates the scope")
	}
}

// TestManualUninstallDocListsEveryManagedKey pins docs/laptop-service.md against
// managedKeys.
//
// The doc's manual-uninstall fallback tells a user which keys to delete by hand. It
// said "three" while managedKeys held seven, and four of the missing ones point at
// bundle.crt — which the same procedure deletes a step earlier. Since those four
// REPLACE their tool's trust store rather than extending it, following the doc left
// git, curl and Python failing every TLS call. The count had already drifted once, so
// it is pinned rather than trusted.
func TestManualUninstallDocListsEveryManagedKey(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "laptop-service.md"))
	if err != nil {
		t.Fatalf("cannot read the laptop-service doc: %v", err)
	}
	body := string(doc)
	for _, k := range managedKeys {
		if !strings.Contains(body, k) {
			t.Errorf("laptop-service.md does not mention %s; a user following the "+
				"manual-uninstall steps would leave it behind", k)
		}
	}
	// And it must not still claim a count, which is what went stale. Naming the
	// number in prose invites exactly this drift.
	for _, stale := range []string{"the three Cortex keys", "three keys"} {
		if strings.Contains(body, stale) {
			t.Errorf("laptop-service.md still says %q; managedKeys has %d entries",
				stale, len(managedKeys))
		}
	}
}
