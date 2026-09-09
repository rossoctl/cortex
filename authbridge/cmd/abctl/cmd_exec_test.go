package main

import (
	"bytes"
	"github.com/rossoctl/cortex/authbridge/authlib/tlsbridge"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// execCfg writes a Cortex config and returns its path plus the CA path it names.
func execCfg(t *testing.T) (cfgPath, caPath string) {
	t.Helper()
	dir := t.TempDir()
	caDir := filepath.Join(dir, "ca")
	cfgPath = filepath.Join(dir, "config.yaml")
	body := strings.Replace(cortexCfg, "CADIR", caDir, 1)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// Write ca.crt, so the fixture represents a Cortex that has started at least
	// once — the normal case, and the only one where the replacing CA vars are set.
	// execCfgNoCA covers the pre-first-start state.
	if err := os.MkdirAll(caDir, 0o755); err != nil {
		t.Fatal(err)
	}
	caPath = filepath.Join(caDir, "ca.crt")
	if err := os.WriteFile(caPath, []byte(testCAPEM), 0o644); err != nil {
		t.Fatal(err)
	}
	// bundle.crt too: the proxy writes it on boot via tlsbridge.EnsureTrustBundle,
	// so a fixture representing a started Cortex has both. execCfgNoCA covers the
	// before-first-start state.
	if err := os.WriteFile(filepath.Join(caDir, tlsbridge.TrustBundleName),
		[]byte(testCAPEM+testRootPEM), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath, caPath
}

// testRootPEM stands in for the platform roots the real bundle concatenates after
// the bridge CA.
const testRootPEM = "-----BEGIN CERTIFICATE-----\nSYSTEMROOT\n-----END CERTIFICATE-----\n"

// testCAPEM stands in for the bridge CA. Only its bytes matter here — nothing
// under test parses it.
const testCAPEM = "-----BEGIN CERTIFICATE-----\nBRIDGECA\n-----END CERTIFICATE-----\n"

// execCfgNoCA is execCfg without ca.crt on disk: Cortex configured but never
// started.
func execCfgNoCA(t *testing.T) (cfgPath, caPath string) {
	t.Helper()
	dir := t.TempDir()
	caDir := filepath.Join(dir, "ca")
	cfgPath = filepath.Join(dir, "config.yaml")
	body := strings.Replace(cortexCfg, "CADIR", caDir, 1)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, filepath.Join(caDir, "ca.crt")
}

// exec injects every managed key except the Claude-Code-only telemetry switch,
// plus the lowercase proxy spellings. Nine names, two distinct values: the proxy
// URL, ca.crt for the one ADDITIVE variable, and bundle.crt for the four that
// REPLACE the trust store.
//
// bundle.crt rather than ca.crt for those four is the property that matters — see
// tlsbridge.TrustBundleName. Pointing them at the lone CA makes the child trust
// the bridge and nothing else, so every unproxied TLS call fails.
func TestExecEnv_InjectsManagedKeysPlusLowercaseProxies(t *testing.T) {
	cfgPath, caPath := execCfg(t)
	env, err := execEnv(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	const wantProxy = "http://127.0.0.1:47600"
	for _, k := range execProxyVars {
		if env[k] != wantProxy {
			t.Errorf("%s = %q, want %q", k, env[k], wantProxy)
		}
	}
	// The additive variable takes the bridge CA itself.
	if env[envCACerts] != caPath {
		t.Errorf("%s = %q, want the bridge CA %q", envCACerts, env[envCACerts], caPath)
	}
	// The replacing variables take the bundle, never the lone CA.
	wantBundle := filepath.Join(filepath.Dir(caPath), tlsbridge.TrustBundleName)
	for _, k := range bundleKeys {
		if env[k] == caPath {
			t.Errorf("%s names the lone bridge CA; the child would lose all public trust", k)
		}
		if env[k] != wantBundle {
			t.Errorf("%s = %q, want the bundle %q", k, env[k], wantBundle)
		}
	}
	// The telemetry switch is a Claude Code setting, not an environment concern for
	// an arbitrary child.
	if _, ok := env[envNoTelem]; ok {
		t.Errorf("exec injected %s", envNoTelem)
	}
}

// TestExecEnv_MatchesClaudeCodeEnable is the anti-drift check. exec and
// `claude-code enable` must point at the same proxy and the same CA; if someone
// changes one derivation and not the other, the two commands disagree about where
// Cortex is and only one of them works.
func TestExecEnv_MatchesClaudeCodeEnable(t *testing.T) {
	cfgPath, _ := execCfg(t)
	want, err := wanted(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	env, err := execEnv(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if env["HTTPS_PROXY"] != want[envProxy] {
		t.Errorf("exec HTTPS_PROXY = %q, enable writes %q", env["HTTPS_PROXY"], want[envProxy])
	}
	if env["NODE_EXTRA_CA_CERTS"] != want[envCACerts] {
		t.Errorf("exec NODE_EXTRA_CA_CERTS = %q, enable writes %q", env["NODE_EXTRA_CA_CERTS"], want[envCACerts])
	}
	// The telemetry key is a Claude Code setting, not an environment concern for an
	// arbitrary child, so exec must not inject it.
	if _, ok := env[envNoTelem]; ok {
		t.Errorf("exec injected %s, which is a Claude Code setting", envNoTelem)
	}
}

// TestExecEnv_NoCADirIsRefused: injecting the proxy without a CA is the silent
// failure mode — a lenient client tunnels through unparsed and looks fine while
// Cortex sees nothing.
func TestExecEnv_NoCADirIsRefused(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := `mode: proxy-sidecar
listener:
  roles: [forward]
  forward_proxy_addr: "127.0.0.1:47600"
`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := execEnv(cfgPath); err == nil {
		t.Fatal("want an error when tls_bridge.ca_dir is absent, got nil")
	}
}

// TestRunExec_PassesArgvIntactAndReturnsExitCode covers the two halves of the
// pass-through promise at once: the child sees exactly the arguments typed after
// --, including things that look like abctl's own flags, and its exit status
// comes back out.
func TestRunExec_PassesArgvIntactAndReturnsExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	cfgPath, _ := execCfg(t)

	// --config and --print after -- belong to the child. If runExec's flag set ever
	// saw them, this would fail by printing instead of running.
	var stdout, stderr bytes.Buffer
	code := runExec([]string{
		"--config", cfgPath, "--",
		"/bin/sh", "-c", `printf '%s\n' "$@"; exit 7`, "sh",
		"--config", "--print", "-x", "a b", "",
	}, &stdout, &stderr)

	if code != 7 {
		t.Errorf("exit code = %d, want 7 (the child's); stderr=%s", code, stderr.String())
	}
	want := "--config\n--print\n-x\na b\n\n"
	if got := stdout.String(); got != want {
		t.Errorf("child argv:\n%q\nwant:\n%q", got, want)
	}
}

// TestRunExec_ChildSeesTheVariables checks the injection actually reaches the
// process, not just the map.
func TestRunExec_ChildSeesTheVariables(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	cfgPath, caPath := execCfg(t)

	// Pre-set two to wrong values, so this also proves replacement rather than
	// "it happened to be unset".
	t.Setenv("HTTPS_PROXY", "http://corporate.example:3128")
	t.Setenv("SSL_CERT_FILE", "/etc/ssl/wrong.pem")

	// The name list is built from the managed set, not written out by hand: a
	// hardcoded list stopped covering GIT_SSL_CAINFO the moment authlib added it,
	// and a variable this test does not name is a variable it cannot check.
	names := append([]string{}, execProxyVars...)
	for _, k := range managedKeys {
		if k != envNoTelem {
			names = append(names, k)
		}
	}
	script := "for v in " + strings.Join(names, " ") + `; do
			eval "printf '%s=%s\n' \"$v\" \"\$$v\""
		done
		# Prove the bundle is usable from inside the child.
		grep -q BRIDGECA "$CURL_CA_BUNDLE" && echo BUNDLE_HAS_BRIDGE_CA`

	var stdout, stderr bytes.Buffer
	code := runExec([]string{"--config", cfgPath, "--", "/bin/sh", "-c", script}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	got := stdout.String()
	for _, k := range execProxyVars {
		if !strings.Contains(got, k+"=http://127.0.0.1:47600\n") {
			t.Errorf("child did not see %s=http://127.0.0.1:47600\n%s", k, got)
		}
	}
	for _, k := range []string{envCACerts} {
		if !strings.Contains(got, k+"="+caPath+"\n") {
			t.Errorf("child did not see %s=%s\n%s", k, caPath, got)
		}
	}
	// The replacing vars name the generated bundle, not the lone CA, and the child
	// must be able to read it.
	for _, k := range bundleKeys {
		line := k + "="
		i := strings.Index(got, line)
		if i < 0 {
			t.Errorf("child did not see %s\n%s", k, got)
			continue
		}
		v := got[i+len(line):]
		if j := strings.IndexByte(v, '\n'); j >= 0 {
			v = v[:j]
		}
		if v == caPath {
			t.Errorf("%s names the lone bridge CA; public trust would be lost", k)
		}
	}
	// Asserted from the child's output: the bundle is deliberately removed when
	// runExec returns, so it cannot be read from here.
	if !strings.Contains(got, "BUNDLE_HAS_BRIDGE_CA") {
		t.Errorf("the child could not read a bundle containing the bridge CA\n%s", got)
	}
}

// TestRunExec_InheritsUnrelatedEnvironment: scoping the proxy to one child must
// not mean handing it a stripped environment. PATH, HOME and the user's own
// variables have to survive, or `abctl exec -- claude` loses its credentials.
func TestRunExec_InheritsUnrelatedEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	cfgPath, _ := execCfg(t)
	t.Setenv("ABCTL_EXEC_CANARY", "kept")

	var stdout, stderr bytes.Buffer
	code := runExec([]string{"--config", cfgPath, "--",
		"/bin/sh", "-c", `printf '%s\n' "$ABCTL_EXEC_CANARY"`}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "kept" {
		t.Errorf("canary = %q, want %q; the child's environment was not inherited",
			stdout.String(), "kept")
	}
}

// TestMergeEnv_ReplacesRatherThanAppends is the subtle one. Appending a second
// HTTPS_PROXY= entry leaves the child's runtime to pick, and different runtimes
// pick different ends, so the variable would take effect for some children and
// not others depending on whether the user already had it set.
func TestMergeEnv_ReplacesRatherThanAppends(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"HTTPS_PROXY=http://old:3128",
		"HTTPS_PROXY=http://older:3128", // a duplicate, which is legal in execve
		"KEEP=1",
		"MALFORMED",
	}
	out := mergeEnv(env, map[string]string{
		"HTTPS_PROXY":   "http://new:47600",
		"SSL_CERT_FILE": "/ca.crt",
	})

	var proxies []string
	for _, kv := range out {
		if strings.HasPrefix(kv, "HTTPS_PROXY=") {
			proxies = append(proxies, kv)
		}
	}
	if len(proxies) != 1 {
		t.Errorf("HTTPS_PROXY appears %d times (%v), want exactly 1", len(proxies), proxies)
	}
	if len(proxies) > 0 && proxies[0] != "HTTPS_PROXY=http://new:47600" {
		t.Errorf("HTTPS_PROXY = %q, want the injected value", proxies[0])
	}
	joined := strings.Join(out, "\n")
	for _, want := range []string{"PATH=/usr/bin", "KEEP=1", "MALFORMED", "SSL_CERT_FILE=/ca.crt"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q from merged env:\n%s", want, joined)
		}
	}
}

// TestMergeEnv_IsCaseSensitive: the lowercase and uppercase proxy spellings are
// eight distinct names, not four folded pairs.
func TestMergeEnv_IsCaseSensitive(t *testing.T) {
	out := mergeEnv([]string{"http_proxy=http://old:1"}, map[string]string{
		"HTTP_PROXY": "http://new:2",
		"http_proxy": "http://new:2",
	})
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "HTTP_PROXY=http://new:2") ||
		!strings.Contains(joined, "http_proxy=http://new:2") {
		t.Errorf("both spellings must be set independently:\n%s", joined)
	}
}

// TestRunExec_RequiresDelimiter. Without --, there is no syntactic boundary
// between abctl's flags and the child's, so the command is refused rather than
// guessed at.
func TestRunExec_RequiresDelimiter(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runExec([]string{"curl", "https://example.com"}, &stdout, &stderr); code != 2 {
		t.Errorf("exit = %d, want 2 (usage) when -- is missing", code)
	}
	if !strings.Contains(stderr.String(), "--") {
		t.Errorf("usage should explain the delimiter:\n%s", stderr.String())
	}
}

// TestRunExec_RejectsArgsBeforeDelimiter. `abctl exec curl -- -sv` is a typo, and
// running `-sv` as the command is a worse answer than saying so.
func TestRunExec_RejectsArgsBeforeDelimiter(t *testing.T) {
	cfgPath, _ := execCfg(t)
	var stdout, stderr bytes.Buffer
	code := runExec([]string{"--config", cfgPath, "curl", "--", "-sv"}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "curl") {
		t.Errorf("error should name the misplaced argument:\n%s", stderr.String())
	}
}

// TestRunExec_EmptyAfterDelimiter: `abctl exec --` has nothing to run.
func TestRunExec_EmptyAfterDelimiter(t *testing.T) {
	cfgPath, _ := execCfg(t)
	var stdout, stderr bytes.Buffer
	if code := runExec([]string{"--config", cfgPath, "--"}, &stdout, &stderr); code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}

// TestRunExec_CommandNotFound reports 127, as a shell does, so a caller can tell
// "my command is missing" from "my command failed".
func TestRunExec_CommandNotFound(t *testing.T) {
	cfgPath, _ := execCfg(t)
	var stdout, stderr bytes.Buffer
	code := runExec([]string{"--config", cfgPath, "--",
		"abctl-no-such-command-xyzzy"}, &stdout, &stderr)
	if code != execEnvNotFound {
		t.Errorf("exit = %d, want %d", code, execEnvNotFound)
	}
}

// TestRunExec_BadConfigIsAnError, not a silently unproxied child. Running the
// command anyway would be the worst outcome: it works, bypasses Cortex, and
// nothing says so.
func TestRunExec_BadConfigIsAnError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runExec([]string{"--config", filepath.Join(t.TempDir(), "missing.yaml"), "--",
		"/bin/sh", "-c", "exit 0"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
}

// TestRunExec_Print emits eval-able export lines and runs nothing.
func TestRunExec_Print(t *testing.T) {
	cfgPath, caPath := execCfg(t)
	var stdout, stderr bytes.Buffer
	if code := runExec([]string{"--config", cfgPath, "--print", "--"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr.String())
	}
	got := stdout.String()
	// Nine: four proxy spellings, ca.crt for the additive name, and bundle.crt for
	// the four replacing ones. Derived from the managed set rather than hardcoded,
	// so adding a managed key does not silently go unprinted.
	wantLines := len(execProxyVars) + len(managedKeys) - 1 // -1: HTTPS_PROXY counted once
	for _, k := range managedKeys {
		if k == envNoTelem {
			wantLines-- // Claude-Code-only, never injected by exec
		}
	}
	if n := strings.Count(got, "export "); n != wantLines {
		t.Errorf("got %d export lines, want %d:\n%s", n, wantLines, got)
	}
	if !strings.Contains(got, "export HTTPS_PROXY=http://127.0.0.1:47600\n") {
		t.Errorf("missing proxy export:\n%s", got)
	}
	if !strings.Contains(got, caPath) {
		t.Errorf("missing CA path %s:\n%s", caPath, got)
	}
}

// TestShellQuote covers the case that motivates quoting at all: a CA path with a
// space in it, which --print documents as eval-able and which would otherwise
// export a truncated filename.
func TestShellQuote(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/plain/ca.crt", "/plain/ca.crt"},
		{"/Users/x/Application Support/ca.crt", "'/Users/x/Application Support/ca.crt'"},
		{"/it's/ca.crt", `'/it'\''s/ca.crt'`},
		{"", "''"},
		{"http://127.0.0.1:47600", "http://127.0.0.1:47600"},
	} {
		if got := shellQuote(tc.in); got != tc.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestCutArgs matches on whole arguments, so a child argument that merely
// contains "--" is not mistaken for the delimiter.
func TestCutArgs(t *testing.T) {
	before, after, found := cutArgs([]string{"--config", "x", "--", "sh", "--", "y"}, "--")
	if !found {
		t.Fatal("delimiter not found")
	}
	if strings.Join(before, " ") != "--config x" {
		t.Errorf("before = %v", before)
	}
	// The second -- belongs to the child and must survive.
	if strings.Join(after, " ") != "sh -- y" {
		t.Errorf("after = %v", after)
	}

	if _, _, found := cutArgs([]string{"--flag=--"}, "--"); found {
		t.Error("an argument containing -- is not the delimiter")
	}
}

// TestRunExec_SignaledChild reports 128+signum rather than the -1 ExitCode gives,
// which os.Exit would flatten to 255 and lose the cause.
func TestRunExec_SignaledChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX signals")
	}
	cfgPath, _ := execCfg(t)
	var stdout, stderr bytes.Buffer
	// 9 = SIGKILL, so 128+9 = 137.
	code := runExec([]string{"--config", cfgPath, "--",
		"/bin/sh", "-c", "kill -9 $$"}, &stdout, &stderr)
	if code != 137 {
		t.Errorf("exit = %d, want 137 (128+SIGKILL)", code)
	}
}

// --- Regression tests for the two blocking review findings (PR #916) ---

// TestExecEnv_DisabledBridgeIsRefused is the second finding. ca_dir is only
// *required* when mode is "enabled", so `mode: disabled` with a ca_dir set is a
// valid config that used to be accepted — injecting a CA for a bridge that
// terminates nothing, which fails every https request in the child.
func TestExecEnv_DisabledBridgeIsRefused(t *testing.T) {
	for _, mode := range []string{"disabled", ""} {
		t.Run("mode="+mode, func(t *testing.T) {
			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "config.yaml")
			body := "mode: proxy-sidecar\n" +
				"listener:\n  roles: [forward]\n  forward_proxy_addr: \"127.0.0.1:47600\"\n" +
				"tls_bridge:\n"
			if mode != "" {
				body += "  mode: " + mode + "\n"
			}
			body += "  ca_dir: \"" + filepath.Join(dir, "ca") + "\"\n"
			if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := execEnv(cfgPath)
			if err == nil {
				t.Fatal("want an error when the bridge is not enabled, got nil")
			}
			if !strings.Contains(err.Error(), "tls_bridge.mode") {
				t.Errorf("error should name tls_bridge.mode, got: %v", err)
			}
		})
	}
}

// TestRunExec_DisabledBridgeExitsBeforeRunning: the guard must refuse rather than
// run the command with broken TLS settings.
func TestRunExec_DisabledBridgeExitsBeforeRunning(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := "mode: proxy-sidecar\n" +
		"listener:\n  roles: [forward]\n  forward_proxy_addr: \"127.0.0.1:47600\"\n" +
		"tls_bridge:\n  mode: disabled\n  ca_dir: \"" + filepath.Join(dir, "ca") + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "ran")
	var stdout, stderr bytes.Buffer
	code := runExec([]string{"--config", cfgPath, "--",
		"/bin/sh", "-c", "touch " + marker}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the command ran despite the disabled bridge")
	}
}

// TestRunExec_BeforeFirstStart_StillRuns pairs with the above: the command runs,
// and the note about the missing CA is printed.
func TestRunExec_BeforeFirstStart_StillRuns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	cfgPath, _ := execCfgNoCA(t)
	var stdout, stderr bytes.Buffer
	code := runExec([]string{"--config", cfgPath, "--", "/bin/sh", "-c", "echo ran"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "ran" {
		t.Errorf("child did not run: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "does not exist yet") {
		t.Errorf("expected a note about the missing CA:\n%s", stderr.String())
	}
}

// TestRunExec_RelaysSIGTERM. A signal aimed at abctl's own PID — `timeout 30
// abctl exec -- …`, a CI runner, systemd — killed abctl under Go's default
// disposition and left the child running with the injected environment. The
// README claims this is safe in a pipeline or a Makefile, and `timeout` is
// exactly that case.
func TestRunExec_RelaysSIGTERM(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX signals")
	}
	cfgPath, _ := execCfg(t)
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	// The child records that it caught SIGTERM. Without relaying it never runs the
	// trap, and the file stays absent.
	caught := filepath.Join(dir, "caught")
	script := "trap 'touch " + caught + "; exit 0' TERM; touch " + started +
		"; i=0; while [ $i -lt 100 ]; do sleep 0.1; i=$((i+1)); done"

	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runExec([]string{"--config", cfgPath, "--", "/bin/sh", "-c", script}, &stdout, &stderr)
	}()

	// Wait for the child to install its trap.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child never started")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Signal THIS process, the way `timeout` signals abctl. runExec's relay must
	// pass it to the child.
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("runExec did not return after SIGTERM; the child was not signalled")
	}
	if _, err := os.Stat(caught); err != nil {
		t.Error("the child never received SIGTERM, so it would have been orphaned")
	}
}

// TestRunExec_PreservesArgv0AsTyped. exec.Command sets Args[0] to the resolved
// absolute path; a shell passes the name as typed. Multi-call binaries dispatch
// on argv[0], and tools echo it in their usage and errors.
func TestRunExec_PreservesArgv0AsTyped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	cfgPath, _ := execCfg(t)
	dir := t.TempDir()
	// A compiled probe, not a shell script: /bin/sh reports $0 from its shebang
	// invocation rather than the argv[0] it was handed, so a script cannot observe
	// what is under test here.
	src := filepath.Join(dir, "argvprobe.go")
	if err := os.WriteFile(src, []byte(
		"package main\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n\nfunc main() { fmt.Printf(\"argv0=%s\\n\", os.Args[0]) }\n"),
		0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "argvprobe")
	build := exec.Command("go", "build", "-o", bin, src)
	build.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("cannot build the probe here: %v\n%s", err, out)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var stdout, stderr bytes.Buffer
	if code := runExec([]string{"--config", cfgPath, "--", "argvprobe"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "argv0=argvprobe" {
		t.Errorf("got %q, want argv0 as typed (%q), not the resolved path", got, "argv0=argvprobe")
	}
}

// --print and a command are mutually exclusive. The two modes disagree about how
// long their output is meant to last: --print emits paths for a shell to keep —
// the same ca.crt `claude-code enable` writes into settings.json, plus the
// trust-bundle.pem beside it — while a command gets them for one process. So this
// is a contradiction about intent, refused rather than half-honoured with a note.
func TestRunExec_PrintWithCommandIsAUsageError(t *testing.T) {
	cfgPath, _ := execCfg(t)
	var stdout, stderr bytes.Buffer
	code := runExec([]string{"--config", cfgPath, "--print", "--", "curl", "https://x"}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("exit = %d, want 2 (usage)", code)
	}
	// Nothing printed: a caller doing `eval "$(...)"` must not get a half-answer.
	if stdout.Len() != 0 {
		t.Errorf("--print with a command emitted exports anyway:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "mutually exclusive") {
		t.Errorf("error should say the two are mutually exclusive:\n%s", stderr.String())
	}
	// The message names the command, so the user can see which half to keep.
	if !strings.Contains(stderr.String(), "curl https://x") {
		t.Errorf("error should echo the command:\n%s", stderr.String())
	}
}

// A usage error must not touch the filesystem. The check is placed with the other
// argument validation, before the config is read, so a rejected invocation does not
// leave a trust-bundle.pem behind as a side effect of being wrong.
func TestRunExec_PrintWithCommandWritesNothing(t *testing.T) {
	cfgPath, caPath := execCfg(t)
	bundle := filepath.Join(filepath.Dir(caPath), "trust-bundle.pem")

	var stdout, stderr bytes.Buffer
	if code := runExec([]string{"--config", cfgPath, "--print", "--", "curl"}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if _, err := os.Stat(bundle); err == nil {
		t.Errorf("a rejected invocation wrote %s", bundle)
	}
}

// Each mode alone still works: this is exclusivity, not a new restriction on
// either form.
func TestRunExec_PrintAloneAndCommandAloneBothWork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	cfgPath, _ := execCfg(t)

	var pout, perr bytes.Buffer
	if code := runExec([]string{"--config", cfgPath, "--print", "--"}, &pout, &perr); code != 0 {
		t.Errorf("--print alone: exit %d: %s", code, perr.String())
	}
	if !strings.Contains(pout.String(), "export HTTPS_PROXY=") {
		t.Errorf("--print alone printed nothing useful:\n%s", pout.String())
	}

	var cout, cerr bytes.Buffer
	if code := runExec([]string{"--config", cfgPath, "--", "/bin/sh", "-c", "echo ran"}, &cout, &cerr); code != 0 {
		t.Errorf("command alone: exit %d: %s", code, cerr.String())
	}
	if strings.TrimSpace(cout.String()) != "ran" {
		t.Errorf("command alone did not run: %q", cout.String())
	}
}

// TestRunExec_PreservesInheritedNoProxy. NO_PROXY is not ours to touch: a host
// listed there bypasses Cortex, and with the replacing CA vars now carrying the
// system roots, such a request still verifies against public trust.
func TestRunExec_PreservesInheritedNoProxy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	cfgPath, _ := execCfg(t)
	t.Setenv("NO_PROXY", "internal.example.com,.corp")
	t.Setenv("no_proxy", "internal.example.com")

	var stdout, stderr bytes.Buffer
	code := runExec([]string{"--config", cfgPath, "--",
		"/bin/sh", "-c", `printf 'NO_PROXY=%s\nno_proxy=%s\n' "$NO_PROXY" "$no_proxy"`}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, "NO_PROXY=internal.example.com,.corp") ||
		!strings.Contains(got, "no_proxy=internal.example.com") {
		t.Errorf("NO_PROXY was not preserved:\n%s", got)
	}
}

// --- Regression tests for the second review round (PR #916) ---

// TestClaudeCodeEnable_DisabledBridgeIsRefused. The tls_bridge.mode gate was
// exec-only, so the two commands drifted on exactly the check whose comment claims
// they cannot: `abctl exec` refused `mode: disabled` + ca_dir, while
// `claude-code enable` wrote it into settings.json and produced the same silent
// break. Both now share bridgeEnabled/errBridgeDisabled.
func TestClaudeCodeEnable_DisabledBridgeIsRefused(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	cfgPath := filepath.Join(dir, "config.yaml")
	body := "mode: proxy-sidecar\n" +
		"listener:\n  roles: [forward]\n  forward_proxy_addr: \"127.0.0.1:47600\"\n" +
		"tls_bridge:\n  mode: disabled\n  ca_dir: \"" + filepath.Join(dir, "ca") + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := claudeCodeEnable(settingsPath, cfgPath, true, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit = %d, want 1 for a disabled bridge", code)
	}
	if !strings.Contains(stderr.String(), "tls_bridge.mode") {
		t.Errorf("error should name tls_bridge.mode:\n%s", stderr.String())
	}
	if _, err := os.Stat(settingsPath); err == nil {
		t.Error("settings.json was written despite the disabled bridge")
	}
}

// Both commands must refuse the same config for the same reason — the property the
// shared helper exists to guarantee.
func TestBridgeGate_ExecAndEnableAgree(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := "mode: proxy-sidecar\n" +
		"listener:\n  roles: [forward]\n  forward_proxy_addr: \"127.0.0.1:47600\"\n" +
		"tls_bridge:\n  mode: disabled\n  ca_dir: \"" + filepath.Join(dir, "ca") + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	_, execErr := execEnv(cfgPath)
	if execErr == nil {
		t.Fatal("exec accepted a disabled bridge")
	}
	var stdout, stderr bytes.Buffer
	if code := claudeCodeEnable(filepath.Join(dir, "settings.json"), cfgPath, true, &stdout, &stderr); code == 0 {
		t.Fatal("claude-code enable accepted a disabled bridge")
	}
	// Same message, so a user sees one explanation whichever command they reached for.
	if !strings.Contains(stderr.String(), execErr.Error()) {
		t.Errorf("the two commands explain it differently:\n exec:   %v\n enable: %s", execErr, stderr.String())
	}
}
