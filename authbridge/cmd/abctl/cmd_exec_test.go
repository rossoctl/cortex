package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
	return cfgPath, caPath
}

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

// TestExecEnv_AllEightNamesFromTwoValues is the contract: eight variables, two
// distinct values, both derived from the config rather than hardcoded.
func TestExecEnv_AllEightNamesFromTwoValues(t *testing.T) {
	cfgPath, caPath := execCfg(t)
	env, cleanup, err := execEnv(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if len(env) != 8 {
		t.Fatalf("want 8 variables, got %d: %v", len(env), env)
	}
	// The config fixture says forward_proxy_addr: 127.0.0.1:47600.
	const wantProxy = "http://127.0.0.1:47600"
	for _, k := range execProxyVars {
		if env[k] != wantProxy {
			t.Errorf("%s = %q, want %q", k, env[k], wantProxy)
		}
	}
	// The additive var gets the bridge CA itself.
	for _, k := range execCAExtraVars {
		if env[k] != caPath {
			t.Errorf("%s = %q, want the bridge CA %q", k, env[k], caPath)
		}
	}
	// The replacing vars must NOT name the lone CA — see execCAReplaceVars.
	for _, k := range execCAReplaceVars {
		if env[k] == caPath {
			t.Errorf("%s = the lone bridge CA, which would drop all public trust", k)
		}
		if env[k] == "" {
			t.Errorf("%s is unset", k)
		}
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
	env, cleanup, err := execEnv(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
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
	if _, _, err := execEnv(cfgPath); err == nil {
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

	// Pre-set two of the eight to wrong values, so this also proves replacement
	// rather than "it happened to be unset".
	t.Setenv("HTTPS_PROXY", "http://corporate.example:3128")
	t.Setenv("SSL_CERT_FILE", "/etc/ssl/wrong.pem")

	var stdout, stderr bytes.Buffer
	code := runExec([]string{"--config", cfgPath, "--",
		"/bin/sh", "-c", `for v in HTTP_PROXY HTTPS_PROXY http_proxy https_proxy \
			NODE_EXTRA_CA_CERTS CURL_CA_BUNDLE REQUESTS_CA_BUNDLE SSL_CERT_FILE; do
			eval "printf '%s=%s\n' \"$v\" \"\$$v\""
		done
		# Prove the bundle is usable from inside the child, where it still exists.
		grep -q BRIDGECA "$CURL_CA_BUNDLE" && echo BUNDLE_HAS_BRIDGE_CA`}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	got := stdout.String()
	for _, k := range execProxyVars {
		if !strings.Contains(got, k+"=http://127.0.0.1:47600\n") {
			t.Errorf("child did not see %s=http://127.0.0.1:47600\n%s", k, got)
		}
	}
	for _, k := range execCAExtraVars {
		if !strings.Contains(got, k+"="+caPath+"\n") {
			t.Errorf("child did not see %s=%s\n%s", k, caPath, got)
		}
	}
	// The replacing vars name the generated bundle, not the lone CA, and the child
	// must be able to read it.
	for _, k := range execCAReplaceVars {
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
	if n := strings.Count(got, "export "); n != 8 {
		t.Errorf("got %d export lines, want 8:\n%s", n, got)
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

// TestExecEnv_ReplacingVarsBundleSystemRoots is the first finding. ca.crt is a
// single certificate, so pointing CURL_CA_BUNDLE / REQUESTS_CA_BUNDLE /
// SSL_CERT_FILE at it replaced the child's whole trust store with one CA —
// breaking every host the bridge does NOT terminate (passthrough_hosts,
// skip_hosts, ports outside tls_bridge.ports). Confirmed against a Linux/OpenSSL
// curl, which exits 77 in that configuration.
//
// The bundle must therefore contain the bridge CA *and*, when the machine has
// one, the system roots.
func TestExecEnv_ReplacingVarsBundleSystemRoots(t *testing.T) {
	cfgPath, caPath := execCfg(t)
	const caBody = testCAPEM

	env, cleanup, err := execEnv(cfgPath)
	if err != nil && !errors.Is(err, errNoSystemRoots) {
		t.Fatal(err)
	}
	defer cleanup()

	bundlePath := env["CURL_CA_BUNDLE"]
	if bundlePath == caPath {
		t.Fatal("CURL_CA_BUNDLE names the lone bridge CA; public trust would be lost")
	}
	// All three replacing vars must agree on the one bundle.
	for _, k := range execCAReplaceVars {
		if env[k] != bundlePath {
			t.Errorf("%s = %q, want the shared bundle %q", k, env[k], bundlePath)
		}
	}

	got, rerr := os.ReadFile(bundlePath)
	if rerr != nil {
		t.Fatalf("bundle unreadable: %v", rerr)
	}
	if !strings.Contains(string(got), "BRIDGECA") {
		t.Error("bundle omits the bridge CA, so bridged hosts would fail verification")
	}
	// If this machine has a system store, the bundle must be strictly larger than
	// the bridge CA alone — that difference IS the retained public trust.
	var systemFound bool
	for _, f := range systemRootFiles {
		if b, e := os.ReadFile(f); e == nil && len(b) > 0 {
			systemFound = true
			break
		}
	}
	if systemFound && len(got) <= len(caBody) {
		t.Errorf("bundle is %d bytes, no larger than the bridge CA alone (%d); system roots were dropped",
			len(got), len(caBody))
	}
	if !systemFound {
		t.Logf("no system CA store on this machine; skipped the public-trust size check")
	}
}

// TestExecEnv_CleanupRemovesBundle: the bundle is a temp file scoped to one
// child, so it must not accumulate in the temp dir across invocations.
func TestExecEnv_CleanupRemovesBundle(t *testing.T) {
	cfgPath, _ := execCfg(t)
	env, cleanup, err := execEnv(cfgPath)
	if err != nil && !errors.Is(err, errNoSystemRoots) {
		t.Fatal(err)
	}
	bundle := env["CURL_CA_BUNDLE"]
	if _, serr := os.Stat(bundle); serr != nil {
		t.Fatalf("bundle missing before cleanup: %v", serr)
	}
	cleanup()
	if _, serr := os.Stat(bundle); !os.IsNotExist(serr) {
		t.Errorf("bundle survived cleanup at %s", bundle)
	}
}

// TestExecEnv_BundleIsNotWorldWritable: it is a trust store. A world-writable one
// would let any local user add a root the child then trusts.
func TestExecEnv_BundleIsNotWorldWritable(t *testing.T) {
	cfgPath, _ := execCfg(t)
	env, cleanup, err := execEnv(cfgPath)
	if err != nil && !errors.Is(err, errNoSystemRoots) {
		t.Fatal(err)
	}
	defer cleanup()
	fi, serr := os.Stat(env["SSL_CERT_FILE"])
	if serr != nil {
		t.Fatal(serr)
	}
	if perm := fi.Mode().Perm(); perm&0o022 != 0 {
		t.Errorf("bundle mode is %04o; group/other must not be writable", perm)
	}
}

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
			_, _, err := execEnv(cfgPath)
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

// TestExecEnv_BeforeFirstStart_LeavesReplacingVarsUnset. Cortex writes ca.crt on
// boot, and enabling before that is legitimate — so exec must still run the
// child. But the replacing vars must be left UNSET rather than pointed at a
// bundle with no bridge CA in it: unset costs only the bridged hosts (the same
// state `claude-code enable` has always produced), whereas a bridge-CA-less
// bundle would break every host, bridged or not.
//
// This is the regression that making an unreadable CA fatal introduced.
func TestExecEnv_BeforeFirstStart_LeavesReplacingVarsUnset(t *testing.T) {
	cfgPath, caPath := execCfgNoCA(t)
	if _, err := os.Stat(caPath); err == nil {
		t.Fatal("fixture should not have created ca.crt")
	}
	env, cleanup, err := execEnv(cfgPath)
	if err != nil {
		t.Fatalf("a not-yet-written CA must not be fatal: %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	// The proxy is still worth setting — that half works before first start.
	for _, k := range execProxyVars {
		if env[k] == "" {
			t.Errorf("%s should still be set", k)
		}
	}
	// The additive var is harmless when the file is missing: Node warns, keeps its
	// own roots. This matches what `claude-code enable` writes.
	if env["NODE_EXTRA_CA_CERTS"] != caPath {
		t.Errorf("NODE_EXTRA_CA_CERTS = %q, want %q", env["NODE_EXTRA_CA_CERTS"], caPath)
	}
	for _, k := range execCAReplaceVars {
		if v, ok := env[k]; ok {
			t.Errorf("%s = %q; must be unset before the CA exists, or the child loses all trust", k, v)
		}
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
