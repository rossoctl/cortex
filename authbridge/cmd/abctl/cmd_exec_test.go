package main

import (
	"bytes"
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
	return cfgPath, filepath.Join(caDir, "ca.crt")
}

// TestExecEnv_AllEightNamesFromTwoValues is the contract: eight variables, two
// distinct values, both derived from the config rather than hardcoded.
func TestExecEnv_AllEightNamesFromTwoValues(t *testing.T) {
	cfgPath, caPath := execCfg(t)
	env, err := execEnv(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
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
	for _, k := range execCAVars {
		if env[k] != caPath {
			t.Errorf("%s = %q, want %q", k, env[k], caPath)
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

	// Pre-set two of the eight to wrong values, so this also proves replacement
	// rather than "it happened to be unset".
	t.Setenv("HTTPS_PROXY", "http://corporate.example:3128")
	t.Setenv("SSL_CERT_FILE", "/etc/ssl/wrong.pem")

	var stdout, stderr bytes.Buffer
	code := runExec([]string{"--config", cfgPath, "--",
		"/bin/sh", "-c", `for v in HTTP_PROXY HTTPS_PROXY http_proxy https_proxy \
			NODE_EXTRA_CA_CERTS CURL_CA_BUNDLE REQUESTS_CA_BUNDLE SSL_CERT_FILE; do
			eval "printf '%s=%s\n' \"$v\" \"\$$v\""
		done`}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	got := stdout.String()
	for _, k := range execProxyVars {
		if !strings.Contains(got, k+"=http://127.0.0.1:47600\n") {
			t.Errorf("child did not see %s=http://127.0.0.1:47600\n%s", k, got)
		}
	}
	for _, k := range execCAVars {
		if !strings.Contains(got, k+"="+caPath+"\n") {
			t.Errorf("child did not see %s=%s\n%s", k, caPath, got)
		}
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
