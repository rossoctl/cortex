package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/peerproc"
	"github.com/rossoctl/cortex/core/tlsbridge"
)

// fakeOpenCodePath is the --opencode every test passes. Nothing exists there: the
// runner is stubbed, and the stub fails a test that is handed any other binary.
const fakeOpenCodePath = "/fake/opencode"

// openCodeKeyOrder is the order the brief gives, written out rather than read from
// openCodeKeys so the tests pin it.
var openCodeKeyOrder = []string{
	"HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy",
	"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "GIT_SSL_CAINFO", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE",
}

// openCodeCLI is an in-memory opencode: the service environment that `service get
// env` prints and `service set env` / `unset env` change, whether the service runs and
// on which port, and the environment its process started with. Like OpenCode 2.0.21,
// it stops the running service on every `service set env` and `service unset env`,
// unless keepsRunning is set, and `service restart` starts it again from its service
// environment: a new pid, one more than the last, whose environment is PATH plus the
// service environment, unless restartKeepsEnv is set.
type openCodeCLI struct {
	env             map[string]string
	running         bool
	keepsRunning    bool  // the service survives `set env` and `unset env`: an OpenCode that does not stop it
	statusErr       error // what `service status` fails with
	port            int
	pid             int32
	listenErr       error    // what openCodeListener fails with for a running service
	procEnv         []string // what openCodeEnviron returns for the service's pid
	procErr         error
	getEnv          *string // when set, what `service get env` prints instead of env as JSON
	failSet         string  // a key whose `service set env` fails
	failUnset       string  // a key whose `service unset env` fails
	restartErr      error   // what `service restart` fails with
	restartKeepsEnv bool    // `service restart` leaves procEnv as it was
	answer          bool    // what opencodeConfirm answers

	calls    []string          // every command, its arguments space-joined
	cliWant  map[string]string // openCodeCLIWant as it stood at the last command
	prompts  int
	atPrompt string // stdout as it stood when opencodeConfirm was last asked
}

// restarts is how many times `service restart` ran.
func (f *openCodeCLI) restarts() int {
	n := 0
	for _, c := range f.calls {
		if c == "service restart" {
			n++
		}
	}
	return n
}

// writes is the calls that changed the service environment, in order.
func (f *openCodeCLI) writes() []string {
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "service set env ") || strings.HasPrefix(c, "service unset env ") {
			out = append(out, c)
		}
	}
	return out
}

// stubOpenCodeCLI points the opencode seams and the prompt at f, and gives the test a
// HOME of its own holding a Cortex config whose ca_dir is ~/.cortex/ca. It returns
// that HOME. Nothing a test does can run the real opencode, prompt on a terminal, or
// touch the real user's files.
func stubOpenCodeCLI(t *testing.T, f *openCodeCLI) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeOpenCodeCortexCfg(t, home, cortexCfg)
	stubOpenCodeSeams(t, f)
	return home
}

// stubOpenCodeSeams is stubOpenCodeCLI under the HOME the test already has.
func stubOpenCodeSeams(t *testing.T, f *openCodeCLI) {
	t.Helper()
	if f.env == nil {
		f.env = map[string]string{}
	}
	if f.port == 0 {
		f.port = 49374
	}
	if f.pid == 0 {
		f.pid = 4242
	}

	run, listener, environ, prompt := openCodeRun, openCodeListener, openCodeEnviron, opencodeConfirm
	t.Cleanup(func() {
		openCodeRun, openCodeListener, openCodeEnviron, opencodeConfirm = run, listener, environ, prompt
	})
	openCodeRun = func(bin string, args ...string) (string, error) {
		if bin != fakeOpenCodePath {
			t.Errorf("ran %s, want the --opencode binary %s", bin, fakeOpenCodePath)
		}
		f.calls = append(f.calls, strings.Join(args, " "))
		f.cliWant = maps.Clone(openCodeCLIWant)
		switch {
		case slices.Equal(args, []string{"service", "status"}):
			if f.statusErr != nil {
				return "", f.statusErr
			}
			if !f.running {
				return "stopped", nil
			}
			return fmt.Sprintf("http://127.0.0.1:%d", f.port), nil
		case slices.Equal(args, []string{"service", "get", "env"}):
			if f.getEnv != nil {
				return *f.getEnv, nil
			}
			b, err := json.Marshal(f.env)
			return string(b), err
		case len(args) == 5 && slices.Equal(args[:3], []string{"service", "set", "env"}):
			if !f.keepsRunning {
				f.running = false
			}
			if args[3] == f.failSet {
				return "", fmt.Errorf("opencode service set env %s: exit status 1", args[3])
			}
			f.env[args[3]] = args[4]
			return "", nil
		case len(args) == 4 && slices.Equal(args[:3], []string{"service", "unset", "env"}):
			if !f.keepsRunning {
				f.running = false
			}
			if args[3] == f.failUnset {
				return "", fmt.Errorf("opencode service unset env %s: exit status 1", args[3])
			}
			delete(f.env, args[3])
			return "", nil
		case slices.Equal(args, []string{"service", "restart"}):
			if f.restartErr != nil {
				f.running = false
				return "", f.restartErr
			}
			f.running = true
			f.pid++
			if !f.restartKeepsEnv {
				f.procEnv = []string{"PATH=/usr/bin"}
				for _, k := range slices.Sorted(maps.Keys(f.env)) {
					f.procEnv = append(f.procEnv, k+"="+f.env[k])
				}
			}
			return "", nil
		}
		t.Errorf("ran opencode %s, which the fake does not answer", strings.Join(args, " "))
		return "", errors.New("unexpected command")
	}
	openCodeListener = func(addr netip.AddrPort) (int32, error) {
		if !f.running || int(addr.Port()) != f.port {
			return 0, peerproc.ErrNotFound
		}
		if f.listenErr != nil {
			return 0, f.listenErr
		}
		return f.pid, nil
	}
	openCodeEnviron = func(pid int32) ([]string, error) {
		if pid != f.pid {
			t.Errorf("read the environment of pid %d, want the service's %d", pid, f.pid)
		}
		return f.procEnv, f.procErr
	}
	opencodeConfirm = func(w io.Writer) bool {
		f.prompts++
		if b, ok := w.(*bytes.Buffer); ok {
			f.atPrompt = b.String()
		}
		return f.answer
	}
}

// writeOpenCodeCortexCfg writes body, cmd_claudecode_test.go's fixture shape, as
// ~/.cortex/config.yaml, with its ca_dir at ~/.cortex/ca.
func writeOpenCodeCortexCfg(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, ".cortex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body = strings.Replace(body, "CADIR", filepath.Join(dir, "ca"), 1)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// openCodeWant is the nine values enable sets for the fixture config under home.
func openCodeWant(home string) map[string]string {
	ca := filepath.Join(home, ".cortex", "ca")
	bundle := filepath.Join(ca, tlsbridge.TrustBundleName)
	return map[string]string{
		"HTTPS_PROXY": "http://127.0.0.1:47600", "HTTP_PROXY": "http://127.0.0.1:47600",
		"https_proxy": "http://127.0.0.1:47600", "http_proxy": "http://127.0.0.1:47600",
		"NODE_EXTRA_CA_CERTS": filepath.Join(ca, "ca.crt"),
		"SSL_CERT_FILE":       bundle, "GIT_SSL_CAINFO": bundle, "REQUESTS_CA_BUNDLE": bundle, "CURL_CA_BUNDLE": bundle,
	}
}

func openCodeStatePath(home string) string {
	return filepath.Join(home, ".cortex", "opencode-state.json")
}

// runOC runs `agentop configure opencode ARGS --opencode /fake/opencode`.
func runOC(args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = runOpenCode(append(args, "--opencode", fakeOpenCodePath), &out, &errb)
	return code, out.String(), errb.String()
}

// openCodeOldEnvNote is the line for a service running with its old environment: the
// one the fake started with, pid 4242, and the one `service restart` started, 4243.
const (
	openCodeOldEnvNote = "OpenCode's background service (pid 4242) is running with its old environment.\n" +
		"  It picks this up when it restarts: opencode service restart   # interrupts every OpenCode session using it\n"
	openCodeOldEnvNoteRestarted = "OpenCode's background service (pid 4243) is running with its old environment.\n" +
		"  It picks this up when it restarts: opencode service restart   # interrupts every OpenCode session using it\n"
)

// The lines about the restart a change to the service environment makes: before the
// prompt when the service runs, with a pid and without one, and when that cannot be
// told; and after the restart, by what the restarted service turned out to use.
const (
	openCodeRestartNote = "OpenCode's background service is running (pid 4242). Changing its environment restarts it,\n" +
		"  which interrupts every OpenCode session using it; an open OpenCode reconnects to it.\n"
	openCodeRestartNoPIDNote = "OpenCode's background service is running. Changing its environment restarts it, which\n" +
		"  interrupts every OpenCode session using it; an open OpenCode reconnects to it.\n"
	// When `service status` fails nothing is restarted, so this one says only what the CLI does.
	openCodeMaybeStopNote = "If OpenCode's background service is running, changing its environment stops it, which\n" +
		"  interrupts every OpenCode session using it.\n"

	openCodeRestartedOnCortex  = "OpenCode's background service was restarted (pid 4243) and is using Cortex.\n"
	openCodeRestartedOffCortex = "OpenCode's background service was restarted (pid 4243) and no longer uses Cortex.\n"
	openCodeRestartedUnchecked = "OpenCode's background service was restarted (pid 4243); its environment could not be checked.\n"

	// What the lines this replaced said: the change stopped the service, and nothing
	// started it again.
	openCodeStoppedNote = "OpenCode's background service is stopped; it starts with the new environment the next time you run OpenCode.\n"
)

func TestConfigureOpenCode_EnableOnAnEmptyEnvironment(t *testing.T) {
	f := &openCodeCLI{}
	home := stubOpenCodeCLI(t, f)

	code, out, errOut := runOC("enable", "--yes")
	if code != 0 {
		t.Fatalf("exit %d; stderr: %s", code, errOut)
	}
	want := openCodeWant(home)
	if !maps.Equal(f.env, want) {
		t.Errorf("service env =\n%v\nwant\n%v", f.env, want)
	}
	// One set per key: the CA variables first, then the proxy, each in the brief's order,
	// so a failure part way never leaves the proxy set without its CA.
	var sets []string
	for _, k := range append(slices.Clone(openCodeKeyOrder[4:]), openCodeKeyOrder[:4]...) {
		sets = append(sets, "service set env "+k+" "+want[k])
	}
	if !slices.Equal(f.writes(), sets) {
		t.Errorf("writes =\n%s\nwant\n%s", strings.Join(f.writes(), "\n"), strings.Join(sets, "\n"))
	}

	var lines strings.Builder
	for _, k := range openCodeKeyOrder {
		lines.WriteString("  " + k + "=" + want[k] + "\n")
	}
	block := "Sets in OpenCode's background-service environment (opencode service set env):\n" + lines.String() +
		"Nothing else in that environment changes; agentop configure opencode disable removes them again.\n"
	if !strings.Contains(out, block) {
		t.Errorf("stdout lacks the change block:\n%s\nwant it to contain:\n%s", out, block)
	}
	if !strings.Contains(out, "Enabled — OpenCode's service uses Cortex from its next start.\n") {
		t.Errorf("stdout lacks the closing line:\n%s", out)
	}
	if f.prompts != 0 {
		t.Errorf("prompted %d times under --yes", f.prompts)
	}

	st, err := readState(openCodeStatePath(home))
	if err != nil || st == nil {
		t.Fatalf("state = %v, %v; want a record", st, err)
	}
	if st.Settings != "opencode service env" {
		t.Errorf("Settings = %q, want %q", st.Settings, "opencode service env")
	}
	if len(st.Prior) != 9 {
		t.Errorf("Prior has %d entries, want 9: %v", len(st.Prior), st.Prior)
	}
	for _, k := range openCodeKeyOrder {
		if p, ok := st.Prior[k]; !ok || p != nil {
			t.Errorf("Prior[%s] = %v (recorded %v), want a recorded nil: it was absent", k, p, ok)
		}
	}
	// Claude Code's record is a different file; cortex reads that one back.
	if _, err := os.Stat(filepath.Join(home, ".cortex", "claude-code-state.json")); !os.IsNotExist(err) {
		t.Errorf("wrote Claude Code's state file: %v", err)
	}
}

// The second run finds nothing to change, so it neither asks nor writes.
func TestConfigureOpenCode_EnableIsIdempotent(t *testing.T) {
	f := &openCodeCLI{}
	stubOpenCodeCLI(t, f)
	if code, _, errOut := runOC("enable", "--yes"); code != 0 {
		t.Fatalf("first enable: exit %d: %s", code, errOut)
	}
	f.calls = nil

	code, out, errOut := runOC("enable") // no --yes: it must not get as far as asking
	if code != 0 {
		t.Fatalf("second enable: exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "Already enabled: OpenCode's service environment routes it through Cortex.\n") {
		t.Errorf("stdout = %q, want the already-enabled line", out)
	}
	if w := f.writes(); len(w) != 0 {
		t.Errorf("second enable wrote %v", w)
	}
	if f.prompts != 0 {
		t.Errorf("prompted with nothing to change")
	}
}

// A value someone else set is refused, for any of the nine, and nothing is written.
func TestConfigureOpenCode_EnableRefusesAForeignValue(t *testing.T) {
	for _, tc := range []struct{ key, val string }{
		{"HTTPS_PROXY", "http://corp:3128"},
		{"http_proxy", "http://corp:3128"},
		// A CA of the user's own: Cortex's are under ~/.cortex.
		{"NODE_EXTRA_CA_CERTS", "/my/ca.pem"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			f := &openCodeCLI{env: map[string]string{tc.key: tc.val}}
			home := stubOpenCodeCLI(t, f)
			code, _, errOut := runOC("enable", "--yes")
			if code != 1 {
				t.Errorf("exit %d, want 1", code)
			}
			want := fmt.Sprintf("agentop: %s is already set to %q in OpenCode's service environment.\n"+
				"  Refusing to overwrite a value you set. Remove it first: opencode service unset env %s\n",
				tc.key, tc.val, tc.key)
			if errOut != want {
				t.Errorf("stderr =\n%s\nwant\n%s", errOut, want)
			}
			if w := f.writes(); len(w) != 0 {
				t.Errorf("wrote %v despite the refusal", w)
			}
			if f.env[tc.key] != tc.val {
				t.Errorf("%s = %q, want the user's value", tc.key, f.env[tc.key])
			}
			if _, err := os.Stat(openCodeStatePath(home)); !os.IsNotExist(err) {
				t.Errorf("a refused enable recorded state: %v", err)
			}
		})
	}
}

// isCortexValue knows the proxy only as HTTPS_PROXY. A lowercase or HTTP_ spelling an
// earlier enable wrote, for an older Cortex address, must update rather than refuse.
func TestConfigureOpenCode_EnableUpdatesAnOlderCortexValue(t *testing.T) {
	f := &openCodeCLI{env: map[string]string{
		"HTTP_PROXY":  "http://localhost:47610",
		"https_proxy": "http://127.0.0.1:47610",
		"http_proxy":  "http://localhost:47610",
	}}
	home := stubOpenCodeCLI(t, f)
	if code, _, errOut := runOC("enable", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !maps.Equal(f.env, openCodeWant(home)) {
		t.Errorf("service env = %v, want the current Cortex values", f.env)
	}
}

// A value enable replaces without asking is Cortex's, so it is recorded as absent and
// disable removes it. Restoring it would leave OpenCode on Cortex after a disable that
// says it no longer is.
func TestConfigureOpenCode_DisableDoesNotPutCortexBack(t *testing.T) {
	f := &openCodeCLI{env: map[string]string{
		"HTTPS_PROXY":         "http://127.0.0.1:47601",                  // an old Cortex port
		"NODE_EXTRA_CA_CERTS": filepath.Join("/my", ".cortex", "ca.pem"), // Cortex-shaped
	}}
	home := stubOpenCodeCLI(t, f)

	if code, _, errOut := runOC("enable", "--yes"); code != 0 {
		t.Fatalf("enable: exit %d: %s", code, errOut)
	}
	st, err := readState(openCodeStatePath(home))
	if err != nil || st == nil {
		t.Fatalf("state = %v, %v; want a record", st, err)
	}
	for _, k := range []string{"HTTPS_PROXY", "NODE_EXTRA_CA_CERTS"} {
		if p, ok := st.Prior[k]; !ok || p != nil {
			t.Errorf("Prior[%s] = %v (recorded %v), want a recorded nil: the value was Cortex's", k, p, ok)
		}
	}

	code, out, errOut := runOC("disable", "--yes")
	if code != 0 {
		t.Fatalf("disable: exit %d: %s", code, errOut)
	}
	if len(f.env) != 0 {
		t.Errorf("service env after disable = %v, want it empty", f.env)
	}
	if !strings.Contains(out, "Disabled. OpenCode's service no longer routes through Cortex from its next start.\n") {
		t.Errorf("stdout lacks the closing line:\n%s", out)
	}
	if strings.Contains(out, "Restored") {
		t.Errorf("restored a Cortex value:\n%s", out)
	}
	if strings.Contains(out, "\n\n\n") {
		t.Errorf("two blank lines in a row:\n%q", out)
	}
	if _, err := os.Stat(openCodeStatePath(home)); !os.IsNotExist(err) {
		t.Errorf("state file survived disable: %v", err)
	}

	_, out, _ = runOC("disable", "--yes")
	if out != "Nothing to do: none of the Cortex variables are set in OpenCode's service environment.\n" {
		t.Errorf("second disable: stdout = %q, want Nothing to do", out)
	}
}

// A recorded value is put back. enable never records one it may replace, so this
// record is written by hand, as a user repairing or extending it would.
func TestConfigureOpenCode_DisableRestoresARecordedValue(t *testing.T) {
	f := &openCodeCLI{}
	home := stubOpenCodeCLI(t, f)
	maps.Copy(f.env, openCodeWant(home))
	f.env["OPENCODE_OTHER"] = "keep"
	prior := map[string]any{}
	for _, k := range openCodeKeyOrder {
		prior[k] = nil
	}
	prior["NODE_EXTRA_CA_CERTS"] = "/my/ca.pem"
	b, err := json.Marshal(map[string]any{"settings": "opencode service env", "prior": prior})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(openCodeStatePath(home), b, 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runOC("disable", "--yes")
	if code != 0 {
		t.Fatalf("disable: exit %d: %s", code, errOut)
	}
	if want := map[string]string{"NODE_EXTRA_CA_CERTS": "/my/ca.pem", "OPENCODE_OTHER": "keep"}; !maps.Equal(f.env, want) {
		t.Errorf("service env after disable = %v, want %v", f.env, want)
	}
	var want []string
	for _, k := range openCodeKeyOrder {
		if k == "NODE_EXTRA_CA_CERTS" {
			want = append(want, "service set env NODE_EXTRA_CA_CERTS /my/ca.pem")
		} else {
			want = append(want, "service unset env "+k)
		}
	}
	if !slices.Equal(f.writes(), want) {
		t.Errorf("disable's writes =\n%s\nwant\n%s", strings.Join(f.writes(), "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(out, "This will remove from OpenCode's service environment: "+strings.Join(openCodeKeyOrder, ", ")+"\n") {
		t.Errorf("stdout lacks the removal list:\n%s", out)
	}
	if !strings.Contains(out, "Restored to the value(s) you had before: NODE_EXTRA_CA_CERTS\n") {
		t.Errorf("stdout lacks the restored list:\n%s", out)
	}
	if strings.Contains(out, "\n\n\n") {
		t.Errorf("two blank lines in a row:\n%q", out)
	}
	if _, err := os.Stat(openCodeStatePath(home)); !os.IsNotExist(err) {
		t.Errorf("state file survived disable: %v", err)
	}
}

// disable changes a key only when its value is Cortex's, whatever the record says. A
// value someone else set is left, and said so with the command that removes it, before
// anything is asked.
func TestConfigureOpenCode_DisableLeavesAValueCortexDidNotSet(t *testing.T) {
	const left = "  HTTPS_PROXY left as \"http://corp:3128\": it is not a value Cortex set. " +
		"Remove it yourself: opencode service unset env HTTPS_PROXY\n"
	t.Run("no record, nothing else set", func(t *testing.T) {
		f := &openCodeCLI{env: map[string]string{"HTTPS_PROXY": "http://corp:3128"}}
		stubOpenCodeCLI(t, f)
		code, out, errOut := runOC("disable", "--yes")
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		if w := f.writes(); len(w) != 0 {
			t.Errorf("wrote %v", w)
		}
		if out != left {
			t.Errorf("stdout = %q, want only %q", out, left)
		}
	})
	// The record says HTTPS_PROXY was absent before enable, but someone has set it
	// since. It is theirs now, and disable leaves it.
	t.Run("replaced after enable", func(t *testing.T) {
		f := &openCodeCLI{}
		home := stubOpenCodeCLI(t, f)
		if code, _, errOut := runOC("enable", "--yes"); code != 0 {
			t.Fatalf("enable: exit %d: %s", code, errOut)
		}
		f.env["HTTPS_PROXY"] = "http://corp:3128"
		f.calls = nil
		code, out, errOut := runOC("disable", "--yes")
		if code != 0 {
			t.Fatalf("disable: exit %d: %s", code, errOut)
		}
		if want := map[string]string{"HTTPS_PROXY": "http://corp:3128"}; !maps.Equal(f.env, want) {
			t.Errorf("service env = %v, want only the user's HTTPS_PROXY", f.env)
		}
		var unsets []string
		for _, k := range openCodeKeyOrder[1:] {
			unsets = append(unsets, "service unset env "+k)
		}
		if !slices.Equal(f.writes(), unsets) {
			t.Errorf("writes =\n%s\nwant\n%s", strings.Join(f.writes(), "\n"), strings.Join(unsets, "\n"))
		}
		if !strings.Contains(out, left) {
			t.Errorf("stdout lacks %q:\n%s", left, out)
		}
		if strings.Contains(out, "This will remove from OpenCode's service environment: HTTPS_PROXY") {
			t.Errorf("the removal list names the key it leaves:\n%s", out)
		}
		if _, err := os.Stat(openCodeStatePath(home)); !os.IsNotExist(err) {
			t.Errorf("state file survived disable: %v", err)
		}
	})
	// A recorded value is put back only over Cortex's. Over someone else's, putting it
	// back would lose theirs.
	t.Run("recorded value, replaced after enable", func(t *testing.T) {
		f := &openCodeCLI{}
		home := stubOpenCodeCLI(t, f)
		maps.Copy(f.env, openCodeWant(home))
		f.env["NODE_EXTRA_CA_CERTS"] = "/theirs/ca.pem"
		if err := os.WriteFile(openCodeStatePath(home),
			[]byte(`{"settings":"opencode service env","prior":{"NODE_EXTRA_CA_CERTS":"/my/ca.pem"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		code, out, errOut := runOC("disable", "--yes")
		if code != 0 {
			t.Fatalf("disable: exit %d: %s", code, errOut)
		}
		if want := map[string]string{"NODE_EXTRA_CA_CERTS": "/theirs/ca.pem"}; !maps.Equal(f.env, want) {
			t.Errorf("service env = %v, want %v", f.env, want)
		}
		if line := "  NODE_EXTRA_CA_CERTS left as \"/theirs/ca.pem\": it is not a value Cortex set. " +
			"Remove it yourself: opencode service unset env NODE_EXTRA_CA_CERTS\n"; !strings.Contains(out, line) {
			t.Errorf("stdout lacks %q:\n%s", line, out)
		}
		if strings.Contains(out, "Restored") {
			t.Errorf("restored over someone else's value:\n%s", out)
		}
	})
	t.Run("a record that lacks the key", func(t *testing.T) {
		f := &openCodeCLI{env: map[string]string{
			"HTTPS_PROXY": "http://corp:3128",
			"HTTP_PROXY":  "http://127.0.0.1:47600",
		}}
		home := stubOpenCodeCLI(t, f)
		if err := os.WriteFile(openCodeStatePath(home),
			[]byte(`{"settings":"opencode service env","prior":{"HTTP_PROXY":null}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		code, out, errOut := runOC("disable", "--yes")
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		if want := []string{"service unset env HTTP_PROXY"}; !slices.Equal(f.writes(), want) {
			t.Errorf("writes = %v, want %v", f.writes(), want)
		}
		if !strings.Contains(out, "This will remove from OpenCode's service environment: HTTP_PROXY\n") {
			t.Errorf("the removal list names a key it leaves:\n%s", out)
		}
		if !strings.Contains(out, left) {
			t.Errorf("stdout lacks %q:\n%s", left, out)
		}
	})
}

// Cortex's proxy is recognised by its listener, not its spelling. A config moved from
// ":8081" (written as localhost) to "127.0.0.1:8081" names the same listener, so
// disable removes all nine; leaving the proxies would keep OpenCode on Cortex while
// saying it was disabled.
func TestConfigureOpenCode_DisableRecognisesTheListenerInAnotherSpelling(t *testing.T) {
	f := &openCodeCLI{}
	home := stubOpenCodeCLI(t, f)
	writeOpenCodeCortexCfg(t, home, strings.Replace(cortexCfg, `"127.0.0.1:47600"`, `":8081"`, 1))
	if code, _, errOut := runOC("enable", "--yes"); code != 0 {
		t.Fatalf("enable: exit %d: %s", code, errOut)
	}
	if got := f.env["HTTPS_PROXY"]; got != "http://localhost:8081" {
		t.Fatalf("HTTPS_PROXY = %q, want http://localhost:8081, or the edit below proves nothing", got)
	}
	writeOpenCodeCortexCfg(t, home, strings.Replace(cortexCfg, `"127.0.0.1:47600"`, `"127.0.0.1:8081"`, 1))

	code, out, errOut := runOC("disable", "--yes")
	if code != 0 {
		t.Fatalf("disable: exit %d: %s", code, errOut)
	}
	if len(f.env) != 0 {
		t.Errorf("service env after disable = %v, want all nine unset", f.env)
	}
	if strings.Contains(out, "left as") {
		t.Errorf("left Cortex's own proxy:\n%s", out)
	}
	if !strings.Contains(out, "Disabled. OpenCode's service no longer routes through Cortex from its next start.\n") {
		t.Errorf("stdout lacks the closing line:\n%s", out)
	}
}

// The default listener spelled as IPv6 loopback is Cortex's too: enable replaces it
// rather than refusing it, and disable removes it.
func TestConfigureOpenCode_IPv6LoopbackProxyIsCortexs(t *testing.T) {
	f := &openCodeCLI{env: map[string]string{"https_proxy": "http://[::1]:47600"}}
	home := stubOpenCodeCLI(t, f)
	if code, _, errOut := runOC("enable", "--yes"); code != 0 {
		t.Fatalf("enable: exit %d: %s", code, errOut)
	}
	if !maps.Equal(f.env, openCodeWant(home)) {
		t.Errorf("service env after enable = %v, want Cortex's nine", f.env)
	}
	if code, _, errOut := runOC("disable", "--yes"); code != 0 {
		t.Fatalf("disable: exit %d: %s", code, errOut)
	}
	if len(f.env) != 0 {
		t.Errorf("service env after disable = %v, want it empty", f.env)
	}
}

func TestConfigureOpenCode_DisableWithNothingSet(t *testing.T) {
	f := &openCodeCLI{env: map[string]string{"OPENCODE_OTHER": "keep"}}
	stubOpenCodeCLI(t, f)
	code, out, errOut := runOC("disable")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if out != "Nothing to do: none of the Cortex variables are set in OpenCode's service environment.\n" {
		t.Errorf("stdout = %q", out)
	}
	if w := f.writes(); len(w) != 0 || f.prompts != 0 {
		t.Errorf("wrote %v, prompted %d times, with nothing to do", w, f.prompts)
	}
}

// No record means removal, silently: what an enable that could not record leaves.
// An unreadable record means removal too, but said out loud.
func TestConfigureOpenCode_DisableWithoutAUsableRecord(t *testing.T) {
	for _, tc := range []struct {
		name   string
		record string // "" leaves no file
		warns  bool
	}{
		{name: "no record"},
		{name: "unreadable record", record: `{"settings":"`, warns: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &openCodeCLI{}
			home := stubOpenCodeCLI(t, f)
			maps.Copy(f.env, openCodeWant(home))
			if tc.record != "" {
				if err := os.WriteFile(openCodeStatePath(home), []byte(tc.record), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			code, _, errOut := runOC("disable", "--yes")
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			if len(f.env) != 0 {
				t.Errorf("service env = %v, want every key unset", f.env)
			}
			for _, w := range f.writes() {
				if !strings.HasPrefix(w, "service unset env ") {
					t.Errorf("%q: with no usable record every key is unset", w)
				}
			}
			warned := strings.Contains(errOut, "cannot read the record") &&
				strings.Contains(errOut, "written into the record by hand")
			if warned != tc.warns {
				t.Errorf("warned = %v, want %v; stderr: %q", warned, tc.warns, errOut)
			}
		})
	}
}

func TestConfigureOpenCode_Declining(t *testing.T) {
	t.Run("enable", func(t *testing.T) {
		f := &openCodeCLI{answer: false}
		home := stubOpenCodeCLI(t, f)
		code, out, _ := runOC("enable")
		if code != exitDeclined {
			t.Errorf("exit %d, want %d", code, exitDeclined)
		}
		if !strings.HasSuffix(out, "Not changed.\n") {
			t.Errorf("stdout = %q, want it to end with Not changed.", out)
		}
		if f.prompts != 1 {
			t.Errorf("prompted %d times, want once", f.prompts)
		}
		if w := f.writes(); len(w) != 0 {
			t.Errorf("a declined enable wrote %v", w)
		}
		if _, err := os.Stat(openCodeStatePath(home)); !os.IsNotExist(err) {
			t.Errorf("a declined enable recorded state: %v", err)
		}
	})
	t.Run("disable", func(t *testing.T) {
		f := &openCodeCLI{}
		home := stubOpenCodeCLI(t, f)
		if code, _, errOut := runOC("enable", "--yes"); code != 0 {
			t.Fatalf("enable: exit %d: %s", code, errOut)
		}
		f.calls = nil
		code, out, _ := runOC("disable")
		if code != exitDeclined || !strings.HasSuffix(out, "Not changed.\n") {
			t.Errorf("exit %d, stdout %q; want %d and Not changed.", code, out, exitDeclined)
		}
		if w := f.writes(); len(w) != 0 {
			t.Errorf("a declined disable wrote %v", w)
		}
		if _, err := os.Stat(openCodeStatePath(home)); err != nil {
			t.Errorf("a declined disable lost the record: %v", err)
		}
	})
}

func TestConfigureOpenCode_Status(t *testing.T) {
	f := &openCodeCLI{env: map[string]string{"OPENCODE_OTHER": "x"}}
	home := stubOpenCodeCLI(t, f)

	code, out, errOut := runOC("status")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	want := "  CURL_CA_BUNDLE (unset)\n" +
		"  GIT_SSL_CAINFO (unset)\n" +
		"  HTTPS_PROXY (unset)\n" +
		"  HTTP_PROXY (unset)\n" +
		"  NODE_EXTRA_CA_CERTS (unset)\n" +
		"  REQUESTS_CA_BUNDLE (unset)\n" +
		"  SSL_CERT_FILE (unset)\n" +
		"  http_proxy (unset)\n" +
		"  https_proxy (unset)\n" +
		"not fully enabled (0 of 9 set)\n" +
		"OpenCode's background service is not running.\n"
	if out != want {
		t.Errorf("stdout =\n%s\nwant\n%s", out, want)
	}

	if code, _, errOut := runOC("enable", "--yes"); code != 0 {
		t.Fatalf("enable: exit %d: %s", code, errOut)
	}
	f.calls = nil
	code, out, _ = runOC("status")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "\nenabled\n") {
		t.Errorf("stdout lacks the enabled line:\n%s", out)
	}
	if !strings.Contains(out, "  HTTPS_PROXY=http://127.0.0.1:47600\n") ||
		!strings.Contains(out, "  NODE_EXTRA_CA_CERTS="+openCodeWant(home)["NODE_EXTRA_CA_CERTS"]+"\n") {
		t.Errorf("stdout lacks the values:\n%s", out)
	}
	if w := f.writes(); len(w) != 0 {
		t.Errorf("status wrote %v", w)
	}
}

// A value left from an older Cortex address is shown but not counted: a service
// started with it would not reach this Cortex.
func TestConfigureOpenCode_StatusCountsOnlyCortexsValues(t *testing.T) {
	f := &openCodeCLI{}
	home := stubOpenCodeCLI(t, f)
	maps.Copy(f.env, openCodeWant(home))
	f.env["HTTPS_PROXY"] = "http://127.0.0.1:47610"
	_, out, _ := runOC("status")
	if !strings.Contains(out, "  HTTPS_PROXY=http://127.0.0.1:47610\n") || !strings.Contains(out, "not fully enabled (8 of 9 set)\n") {
		t.Errorf("stdout =\n%s\nwant the stale value shown and 8 of 9 counted", out)
	}
}

// Without a readable Cortex config there is nothing to judge against: the keys and
// the service are reported as they are, with no verdict, and that is not a failure.
func TestConfigureOpenCode_StatusWithoutACortexConfig(t *testing.T) {
	f := &openCodeCLI{running: true, procEnv: []string{"HTTPS_PROXY=http://127.0.0.1:47600"}}
	home := stubOpenCodeCLI(t, f)
	f.env["HTTPS_PROXY"] = "http://127.0.0.1:47600"
	cfg := filepath.Join(home, ".cortex", "config.yaml")
	if err := os.Remove(cfg); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runOC("status")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "  HTTPS_PROXY=http://127.0.0.1:47600\n") {
		t.Errorf("stdout lacks the keys:\n%s", out)
	}
	if strings.Contains(out, "enabled") || strings.Contains(out, "old environment") || strings.Contains(out, "using Cortex") {
		t.Errorf("gave a verdict with no config to judge by:\n%s", out)
	}
	if !strings.Contains(out, "cannot compare with Cortex's config: reading "+cfg) {
		t.Errorf("stdout does not say why there is no verdict:\n%s", out)
	}
	if !strings.Contains(out, "OpenCode's background service (pid 4242) is running with proxy http://127.0.0.1:47600.\n") {
		t.Errorf("stdout lacks the service's pid and proxy:\n%s", out)
	}
}

// The service line once enable, disable or status is done. enable and disable restart a
// service that was running before their change, so their line is about the restarted
// one; the restart itself, and the lines before it, are
// TestConfigureOpenCode_ChangingTheEnvironmentRestartsTheService's.
func TestConfigureOpenCode_ServiceNote(t *testing.T) {
	cortexEnv := []string{"PATH=/usr/bin", "HTTPS_PROXY=http://localhost:47600"}
	for _, tc := range []struct {
		name     string
		action   string
		enabled  bool // enable first
		fake     *openCodeCLI
		want     string // a line the output must contain; "" = no service line after the change
		restarts int    // how many times the action runs `service restart`
	}{
		{
			name: "enabled, service without a proxy", action: "enable",
			fake: &openCodeCLI{running: true, procEnv: []string{"PATH=/usr/bin"}}, want: openCodeRestartedOnCortex, restarts: 1,
		},
		{
			name: "enabled, service on another proxy", action: "enable",
			fake: &openCodeCLI{running: true, procEnv: []string{"HTTPS_PROXY=http://corp:3128"}}, want: openCodeRestartedOnCortex, restarts: 1,
		},
		{
			// The CLI did not stop it; it is restarted all the same.
			name: "enabled, service survives the change", action: "enable",
			fake: &openCodeCLI{running: true, keepsRunning: true, procEnv: []string{"PATH=/usr/bin"}}, want: openCodeRestartedOnCortex, restarts: 1,
		},
		{
			name: "enabled, the restart keeps the old environment", action: "enable",
			fake: &openCodeCLI{running: true, restartKeepsEnv: true, procEnv: []string{"PATH=/usr/bin"}},
			want: openCodeOldEnvNoteRestarted, restarts: 1,
		},
		{
			name: "already enabled, service without a proxy", action: "enable", enabled: true,
			fake: &openCodeCLI{running: true, keepsRunning: true, procEnv: []string{"PATH=/usr/bin"}}, want: openCodeOldEnvNote,
		},
		{name: "enabled, service stopped", action: "enable", fake: &openCodeCLI{}},
		{
			name: "disabled, service on Cortex", action: "disable", enabled: true,
			fake: &openCodeCLI{running: true, keepsRunning: true, procEnv: cortexEnv}, want: openCodeRestartedOffCortex, restarts: 1,
		},
		{
			name: "disabled, the restart keeps the old environment", action: "disable", enabled: true,
			fake: &openCodeCLI{running: true, keepsRunning: true, restartKeepsEnv: true, procEnv: cortexEnv},
			want: openCodeOldEnvNoteRestarted, restarts: 1,
		},
		{
			name: "status, enabled, service without a proxy", action: "status", enabled: true,
			fake: &openCodeCLI{running: true, keepsRunning: true, procEnv: []string{"PATH=/usr/bin"}}, want: openCodeOldEnvNote,
		},
		{
			name: "status, enabled, service on Cortex", action: "status", enabled: true,
			fake: &openCodeCLI{running: true, keepsRunning: true, procEnv: cortexEnv},
			want: "OpenCode's background service (pid 4242) is using Cortex.\n",
		},
		{
			name: "status, not enabled, service off Cortex", action: "status",
			fake: &openCodeCLI{running: true, procEnv: []string{"PATH=/usr/bin"}},
			want: "OpenCode's background service (pid 4242) is not using Cortex.\n",
		},
		{
			name: "status, service stopped", action: "status", fake: &openCodeCLI{},
			want: "OpenCode's background service is not running.\n",
		},
		{
			// Nobody can say it was running, so it is not restarted, and the line says
			// what to do.
			name: "service status fails", action: "enable",
			fake: &openCodeCLI{statusErr: errors.New("opencode service status: exit status 1")},
			want: "Could not check OpenCode's running service (opencode service status: exit status 1); " +
				"restart it to be sure: opencode service restart\n",
		},
		{
			name: "status, service status fails", action: "status",
			fake: &openCodeCLI{statusErr: errors.New("opencode service status: exit status 1")},
			want: "Could not check OpenCode's background service (opencode service status: exit status 1).\n",
		},
		{
			name: "environment withheld", action: "enable",
			fake: &openCodeCLI{running: true, procErr: fmt.Errorf("%w: pid 4242's environment is withheld", peerproc.ErrNotFound)},
			want: openCodeRestartedUnchecked, restarts: 1,
		},
		{
			name: "status, environment withheld", action: "status",
			fake: &openCodeCLI{running: true, procErr: fmt.Errorf("%w: pid 4242's environment is withheld", peerproc.ErrNotFound)},
			want: "Could not check OpenCode's background service (reading the environment of pid 4242: " +
				"peerproc: not found: pid 4242's environment is withheld).\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubOpenCodeCLI(t, tc.fake)
			if tc.enabled {
				running := tc.fake.running
				tc.fake.running = false // so the setup enable has no service to restart
				if code, _, errOut := runOC("enable", "--yes"); code != 0 {
					t.Fatalf("enable: exit %d: %s", code, errOut)
				}
				tc.fake.running = running
			}
			tc.fake.calls = nil
			args := []string{tc.action}
			if tc.action != "status" {
				args = append(args, "--yes")
			}
			code, out, errOut := runOC(args...)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			if tc.want != "" && !strings.Contains(out, tc.want) {
				t.Errorf("stdout =\n%s\nwant it to contain\n%s", out, tc.want)
			}
			if after := strings.Replace(out, openCodeRestartNote, "", 1); tc.want == "" && strings.Contains(after, "OpenCode's background service") {
				t.Errorf("stdout has a service line it should not:\n%s", out)
			}
			if n := tc.fake.restarts(); n != tc.restarts {
				t.Errorf("ran service restart %d times, want %d; calls %v", n, tc.restarts, tc.fake.calls)
			}
			for _, c := range tc.fake.calls {
				for _, verb := range []string{"service stop", "service start"} {
					if strings.HasPrefix(c, verb) {
						t.Errorf("ran opencode %s: agentop runs no stop or start", c)
					}
				}
			}
		})
	}
}

// OpenCode's CLI stops the running service whenever its environment changes, and an
// open OpenCode starts it again before the change lands. So enable and disable say,
// before the prompt and under --yes before the change, that the change restarts a
// running service; then, after their last write, they restart it once and say what the
// restarted service uses. A service that was not running is not restarted and gets no
// line, and one the CLI cannot report on gets the hedged line and no restart.
func TestConfigureOpenCode_ChangingTheEnvironmentRestartsTheService(t *testing.T) {
	statusErr := errors.New("opencode service status: exit status 1")
	const couldNotCheck = "Could not check OpenCode's running service (opencode service status: exit status 1); " +
		"restart it to be sure: opencode service restart\n"
	// Nothing was probed after the failed restart, so the line says what it may be and how to apply the change.
	const restartFailed = "agentop: could not restart OpenCode's background service (opencode service restart: exit status 1).\n" +
		"  It may be stopped, or running with its old environment. To apply the change now:\n" +
		"    opencode service restart   # interrupts every OpenCode session using it\n"
	for _, action := range []string{"enable", "disable"} {
		// The environment the service started with is the one the change replaces.
		procEnv := []string{"PATH=/usr/bin"}
		closing, restarted := "Enabled — OpenCode's service uses Cortex from its next start.\n", "Enabled.\n"
		restartedLine := openCodeRestartedOnCortex
		if action == "disable" {
			procEnv = []string{"HTTPS_PROXY=http://127.0.0.1:47600"}
			closing, restarted = "Disabled. OpenCode's service no longer routes through Cortex from its next start.\n", "Disabled.\n"
			restartedLine = openCodeRestartedOffCortex
		}
		for _, tc := range []struct {
			name          string
			running       bool
			keepsRunning  bool
			statusErr     error
			listenErr     error
			restartErr    error
			yes           bool
			before, after string // the line before the change and the one after it; "" = none
			stderr        string
		}{
			{name: "running", running: true, before: openCodeRestartNote, after: restartedLine},
			{name: "running, --yes", running: true, yes: true, before: openCodeRestartNote, after: restartedLine},
			{name: "not running"},
			{name: "not running, --yes", yes: true},
			{name: "service status fails", statusErr: statusErr, before: openCodeMaybeStopNote, after: couldNotCheck},
			{name: "service status fails, --yes", statusErr: statusErr, yes: true, before: openCodeMaybeStopNote, after: couldNotCheck},
			// Running, but no process is found on its port, so there is no pid to name,
			// before the restart or after it.
			{name: "running, no pid", running: true, listenErr: peerproc.ErrNotFound, yes: true,
				before: openCodeRestartNoPIDNote,
				after:  "OpenCode's background service was restarted; its environment could not be checked.\n"},
			// An OpenCode whose CLI does not stop the service is restarted all the same.
			{name: "survives the change", running: true, keepsRunning: true, yes: true, before: openCodeRestartNote, after: restartedLine},
			// The change succeeded, so the exit status is still 0.
			{name: "the restart fails", running: true, yes: true, restartErr: errors.New("opencode service restart: exit status 1"),
				before: openCodeRestartNote, stderr: restartFailed},
		} {
			t.Run(action+", "+tc.name, func(t *testing.T) {
				f := &openCodeCLI{running: tc.running, keepsRunning: tc.keepsRunning, statusErr: tc.statusErr,
					listenErr: tc.listenErr, restartErr: tc.restartErr, procEnv: procEnv, answer: true}
				home := stubOpenCodeCLI(t, f)
				if action == "disable" {
					maps.Copy(f.env, openCodeWant(home))
				}
				args := []string{action}
				if tc.yes {
					args = append(args, "--yes")
				}
				code, out, errOut := runOC(args...)
				if code != 0 {
					t.Fatalf("exit %d: %s", code, errOut)
				}
				wasRunning := tc.running && tc.statusErr == nil
				end := closing
				if wasRunning {
					end = restarted
				}
				i := strings.Index(out, end)
				if i < 0 {
					t.Fatalf("stdout lacks %q:\n%s", end, out)
				}
				before, after := out[:i], out[i+len(end):]
				if wasRunning && strings.Contains(out, closing) {
					t.Errorf("a restarted service got the next-start line:\n%s", out)
				}

				if tc.before == "" {
					if strings.Contains(out, "OpenCode's background service") {
						t.Errorf("a service that is not running got a service line:\n%s", out)
					}
				} else if !strings.Contains(before, tc.before) {
					t.Errorf("stdout before the change lacks\n%s\ngot:\n%s", tc.before, before)
				}
				if tc.yes {
					if f.prompts != 0 {
						t.Errorf("prompted %d times under --yes", f.prompts)
					}
				} else if f.prompts != 1 || !strings.HasSuffix(f.atPrompt, tc.before+"\n") {
					t.Errorf("prompted %d times, with stdout then\n%s\nwant one prompt, straight after\n%s", f.prompts, f.atPrompt, tc.before)
				}
				if after != tc.after {
					t.Errorf("stdout after the closing line =\n%s\nwant\n%s", after, tc.after)
				}
				if errOut != tc.stderr {
					t.Errorf("stderr =\n%s\nwant\n%s", errOut, tc.stderr)
				}
				if strings.Contains(out, openCodeStoppedNote) {
					t.Errorf("still says the service is stopped and starts with the next run:\n%s", out)
				}

				// The service is asked about before anything is written. A service that
				// was running is restarted once, after the last write, and asked about
				// again after that.
				writes := f.writes()
				first := slices.Index(f.calls, "service status")
				if first < 0 || first > slices.Index(f.calls, writes[0]) {
					t.Errorf("calls = %v, want service status before the first write", f.calls)
				}
				last := slices.Index(f.calls, writes[len(writes)-1])
				if !wasRunning {
					if n := f.restarts(); n != 0 {
						t.Errorf("restarted a service that was not seen running; calls %v", f.calls)
					}
					return
				}
				if n := f.restarts(); n != 1 {
					t.Fatalf("ran service restart %d times, want once; calls %v", n, f.calls)
				}
				r := slices.Index(f.calls, "service restart")
				if r < last {
					t.Errorf("calls = %v, want the restart after the last write", f.calls)
				}
				if tc.restartErr == nil && !slices.Contains(f.calls[r:], "service status") {
					t.Errorf("calls = %v, want service status after the restart", f.calls)
				}
			})
		}
	}
}

// The docs that say what a failed restart does name the command agentop prints for it.
// The command is read from finishOpenCodeChange's own output, so the docs are held to
// the code rather than to a literal here. cmd/agentop/README.md named `agentop
// configure opencode status` instead, which after disable advises enabling again.
func TestOpenCodeDocs_AFailedRestartNamesTheCommandAgentopPrints(t *testing.T) {
	stubOpenCodeCLI(t, &openCodeCLI{running: true, restartErr: errors.New("opencode service restart: exit status 1")})
	// enable and disable print the same command, and the docs describe them together.
	var cmd string
	for _, configured := range []bool{true, false} {
		var stdout, stderr bytes.Buffer
		finishOpenCodeChange(fakeOpenCodePath, "", configured, true, &stdout, &stderr)
		_, after, ok := strings.Cut(stderr.String(), "To apply the change now:\n")
		if !ok {
			t.Fatalf("configured=%v: a failed restart printed no command to apply the change:\n%s", configured, stderr.String())
		}
		line, _, _ := strings.Cut(after, "\n")
		got, _, _ := strings.Cut(line, "#")
		got = strings.TrimSpace(got)
		if cmd != "" && got != cmd {
			t.Fatalf("enable names %q after a failed restart and disable names %q", cmd, got)
		}
		cmd = got
	}

	failed := regexp.MustCompile(`(?i)\brestart (that )?fails\b`)
	for _, doc := range []string{"README.md", filepath.Join("..", "..", "docs", "agents", "opencode.md")} {
		raw, err := os.ReadFile(doc)
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		found := 0
		for _, s := range strings.SplitAfter(strings.Join(strings.Fields(string(raw)), " "), ". ") {
			if !failed.MatchString(s) {
				continue
			}
			found++
			if !strings.Contains(s, "`"+cmd+"`") {
				t.Errorf("%s says what a failed restart does without naming `%s`, which agentop prints for it:\n%s", doc, cmd, s)
			}
		}
		if found == 0 {
			t.Errorf("%s no longer says what a failed restart does; this test pins that sentence and cannot find it", doc)
		}
	}
}

// With a running service, a run with nothing to change neither warns about a restart,
// nor asks, nor restarts it: enable a second time, and disable with only a value Cortex
// did not set.
func TestConfigureOpenCode_NothingToChangeLeavesARunningServiceAlone(t *testing.T) {
	notes := []string{openCodeRestartNote, openCodeRestartNoPIDNote, openCodeMaybeStopNote}
	t.Run("enable, already enabled", func(t *testing.T) {
		f := &openCodeCLI{}
		home := stubOpenCodeCLI(t, f)
		maps.Copy(f.env, openCodeWant(home))
		f.running, f.procEnv = true, []string{"PATH=/usr/bin", "HTTPS_PROXY=http://127.0.0.1:47600"}
		code, out, errOut := runOC("enable")
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		want := "Already enabled: OpenCode's service environment routes it through Cortex.\n" +
			"OpenCode's background service (pid 4242) is using Cortex.\n"
		if out != want {
			t.Errorf("stdout =\n%s\nwant\n%s", out, want)
		}
		for _, n := range notes {
			if strings.Contains(out, n) {
				t.Errorf("printed a restart note with nothing to change:\n%s", out)
			}
		}
		if f.prompts != 0 || f.restarts() != 0 || len(f.writes()) != 0 {
			t.Errorf("prompted %d times, restarted %d times, wrote %v, with nothing to change", f.prompts, f.restarts(), f.writes())
		}
	})
	t.Run("disable, only a value Cortex did not set", func(t *testing.T) {
		f := &openCodeCLI{env: map[string]string{"HTTPS_PROXY": "http://corp:3128"}, running: true, procEnv: []string{"PATH=/usr/bin"}}
		stubOpenCodeCLI(t, f)
		code, out, errOut := runOC("disable")
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		want := "  HTTPS_PROXY left as \"http://corp:3128\": it is not a value Cortex set. " +
			"Remove it yourself: opencode service unset env HTTPS_PROXY\n"
		if out != want {
			t.Errorf("stdout =\n%s\nwant\n%s", out, want)
		}
		if f.prompts != 0 || f.restarts() != 0 || len(f.writes()) != 0 {
			t.Errorf("prompted %d times, restarted %d times, wrote %v, with nothing to change", f.prompts, f.restarts(), f.writes())
		}
	})
}

// Declining after the restart line changes nothing, so the service keeps running and is
// not restarted.
func TestConfigureOpenCode_DecliningLeavesTheServiceRunning(t *testing.T) {
	for _, action := range []string{"enable", "disable"} {
		t.Run(action, func(t *testing.T) {
			f := &openCodeCLI{running: true, procEnv: []string{"PATH=/usr/bin"}, answer: false}
			home := stubOpenCodeCLI(t, f)
			if action == "disable" {
				maps.Copy(f.env, openCodeWant(home))
			}
			code, out, _ := runOC(action)
			if code != exitDeclined {
				t.Errorf("exit %d, want %d", code, exitDeclined)
			}
			if f.prompts != 1 || !strings.HasSuffix(f.atPrompt, openCodeRestartNote+"\n") {
				t.Errorf("prompted %d times, with stdout then\n%s\nwant one prompt, straight after the restart line", f.prompts, f.atPrompt)
			}
			if !strings.HasSuffix(out, "Not changed.\n") {
				t.Errorf("stdout = %q, want it to end with Not changed.", out)
			}
			if w := f.writes(); len(w) != 0 {
				t.Errorf("a declined %s wrote %v", action, w)
			}
			if !f.running {
				t.Errorf("a declined %s stopped the service", action)
			}
			if n := f.restarts(); n != 0 {
				t.Errorf("a declined %s restarted the service %d times", action, n)
			}
		})
	}
}

// status's service line is judged by the proxy a restart would give the service — the
// one its service environment yields, in Bun's order — not by the nine-key verdict.
// A service started under agentop exec is on Cortex without the environment saying so,
// and telling it to restart would take it off.
func TestConfigureOpenCode_StatusServiceLineFollowsTheConfiguredProxy(t *testing.T) {
	onCortex := []string{"HTTPS_PROXY=http://localhost:47600"}
	offCortex := []string{"PATH=/usr/bin"}
	const usingButNotConfigured = "OpenCode's background service (pid 4242) is using Cortex, but its service environment does not route it there,\n" +
		"  so its next start will not use Cortex. To keep it on Cortex:\n" +
		"    agentop configure opencode enable   # restarts the service, interrupting every OpenCode session using it\n"
	for _, tc := range []struct {
		name    string
		env     func(home string) map[string]string
		procEnv []string
		verdict string
		want    string
	}{
		{
			name: "configured, on Cortex", env: openCodeWant, procEnv: onCortex, verdict: "enabled\n",
			want: "OpenCode's background service (pid 4242) is using Cortex.\n",
		},
		{
			// The proxy is configured; only a CA variable is missing. A restart keeps
			// the service on Cortex, so there is nothing to restart for.
			name: "8 of 9, on Cortex",
			env: func(home string) map[string]string {
				m := openCodeWant(home)
				delete(m, "SSL_CERT_FILE")
				return m
			},
			procEnv: onCortex, verdict: "not fully enabled (8 of 9 set)\n",
			want: "OpenCode's background service (pid 4242) is using Cortex.\n",
		},
		{name: "configured, off Cortex", env: openCodeWant, procEnv: offCortex, verdict: "enabled\n", want: openCodeOldEnvNote},
		{
			name: "not configured, on Cortex", env: func(string) map[string]string { return map[string]string{} },
			procEnv: onCortex, verdict: "not fully enabled (0 of 9 set)\n", want: usingButNotConfigured,
		},
		{
			name: "not configured, off Cortex", env: func(string) map[string]string { return map[string]string{} },
			procEnv: offCortex, verdict: "not fully enabled (0 of 9 set)\n",
			want: "OpenCode's background service (pid 4242) is not using Cortex.\n",
		},
		{
			// Bun reads https_proxy first, so this environment does not route to Cortex.
			name: "uppercase Cortex, lowercase another",
			env: func(string) map[string]string {
				return map[string]string{"HTTPS_PROXY": "http://127.0.0.1:47600", "https_proxy": "http://corp:3128"}
			},
			procEnv: onCortex, verdict: "not fully enabled (1 of 9 set)\n", want: usingButNotConfigured,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &openCodeCLI{running: true, procEnv: tc.procEnv}
			home := stubOpenCodeCLI(t, f)
			maps.Copy(f.env, tc.env(home))
			code, out, errOut := runOC("status")
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			if want := tc.verdict + tc.want; !strings.HasSuffix(out, want) {
				t.Errorf("stdout =\n%s\nwant it to end with\n%s", out, want)
			}
		})
	}
}

// With Cortex's config unreadable, disable compares the restarted service with the
// HTTPS_PROXY it took out: that is what a service started under the old environment
// uses. The restart keeps the environment the service had (restartKeepsEnv), so the
// comparison has both answers to give.
func TestConfigureOpenCode_DisableWithoutACortexConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		procEnv []string
		note    bool
	}{
		{"service on the removed proxy", []string{"HTTPS_PROXY=http://localhost:47600"}, true},
		{"service off it", []string{"PATH=/usr/bin"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &openCodeCLI{restartKeepsEnv: true, procEnv: tc.procEnv}
			home := stubOpenCodeCLI(t, f)
			if code, _, errOut := runOC("enable", "--yes"); code != 0 {
				t.Fatalf("enable: exit %d: %s", code, errOut)
			}
			f.running = true
			if err := os.Remove(filepath.Join(home, ".cortex", "config.yaml")); err != nil {
				t.Fatal(err)
			}
			code, out, errOut := runOC("disable", "--yes")
			if code != 0 {
				t.Fatalf("disable: exit %d: %s", code, errOut)
			}
			if len(f.env) != 0 {
				t.Errorf("service env = %v, want it empty", f.env)
			}
			if got := strings.Contains(out, openCodeOldEnvNoteRestarted); got != tc.note {
				t.Errorf("old-environment line = %v, want %v:\n%s", got, tc.note, out)
			}
			if got := strings.Contains(out, openCodeRestartedOffCortex); got == tc.note {
				t.Errorf("no-longer-uses-Cortex line = %v, want %v:\n%s", got, !tc.note, out)
			}
		})
	}
}

// The same refusal claude-code gives: a bridge that terminates no TLS gives OpenCode
// nothing to trust. It is refused before OpenCode is asked anything.
func TestConfigureOpenCode_EnableRefusesADisabledBridge(t *testing.T) {
	f := &openCodeCLI{}
	home := stubOpenCodeCLI(t, f)
	writeOpenCodeCortexCfg(t, home, strings.Replace(cortexCfg, "mode: enabled", "mode: disabled", 1))
	cfg := filepath.Join(home, ".cortex", "config.yaml")

	code, _, errOut := runOC("enable", "--yes")
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if want := "agentop: " + errBridgeDisabled(cfg).Error() + "\n"; errOut != want {
		t.Errorf("stderr =\n%s\nwant\n%s", errOut, want)
	}
	if len(f.calls) != 0 {
		t.Errorf("ran %v before refusing", f.calls)
	}
}

func TestConfigureOpenCode_GetEnvMustBeAnObjectOfStrings(t *testing.T) {
	for _, printed := range []string{"", "null", "[]", `{"A": 1}`, `{"A": null}`, "Error: no such command"} {
		t.Run(printed, func(t *testing.T) {
			f := &openCodeCLI{getEnv: &printed}
			stubOpenCodeCLI(t, f)
			code, _, errOut := runOC("status")
			if code != 1 {
				t.Errorf("exit %d, want 1", code)
			}
			if want := fmt.Sprintf("opencode service get env printed %q", printed); !strings.Contains(errOut, want) {
				t.Errorf("stderr = %q, want it to name the output: %s", errOut, want)
			}
		})
	}
}

// A set that fails part way leaves the record, so disable can still put back what
// was there, and says which keys did change. The CA variables are set first, so a
// failure on the first of them leaves no proxy set: the proxy is never set without the
// CA that lets OpenCode trust it.
func TestConfigureOpenCode_EnableFailingPartWay(t *testing.T) {
	t.Run("on the first CA variable", func(t *testing.T) {
		f := &openCodeCLI{failSet: "NODE_EXTRA_CA_CERTS"}
		home := stubOpenCodeCLI(t, f)
		code, out, errOut := runOC("enable", "--yes")
		if code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
		if strings.Contains(out, "Enabled") {
			t.Errorf("reported success:\n%s", out)
		}
		if want := "agentop: opencode service set env NODE_EXTRA_CA_CERTS: exit status 1\n"; errOut != want {
			t.Errorf("stderr = %q, want %q", errOut, want)
		}
		for _, k := range openCodeKeyOrder[:4] {
			if v, ok := f.env[k]; ok {
				t.Errorf("%s = %q: a proxy key was set without its CA", k, v)
			}
		}
		if st, err := readState(openCodeStatePath(home)); err != nil || st == nil {
			t.Errorf("record = %v, %v; want it kept for disable", st, err)
		}
	})
	// Part way, with the service running before: the writes so far stopped it, and it is
	// not restarted, because its environment is half changed.
	t.Run("on the proxy, service running", func(t *testing.T) {
		f := &openCodeCLI{failSet: "HTTPS_PROXY", running: true, procEnv: []string{"PATH=/usr/bin"}}
		stubOpenCodeCLI(t, f)
		code, out, errOut := runOC("enable", "--yes")
		if code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
		if strings.Contains(out, "Enabled") {
			t.Errorf("reported success:\n%s", out)
		}
		want := "agentop: opencode service set env HTTPS_PROXY: exit status 1\n" +
			"  Already set: NODE_EXTRA_CA_CERTS, SSL_CERT_FILE, GIT_SSL_CAINFO, REQUESTS_CA_BUNDLE, CURL_CA_BUNDLE. " +
			"agentop configure opencode disable undoes them\n" +
			"  OpenCode's background service may be stopped; re-run this command to finish.\n"
		if errOut != want {
			t.Errorf("stderr =\n%s\nwant\n%s", errOut, want)
		}
		if n := f.restarts(); n != 0 {
			t.Errorf("restarted the service %d times after a failed change", n)
		}
	})
	// Not running before: nothing was stopped, so nothing is said about it.
	t.Run("on the proxy, service not running", func(t *testing.T) {
		f := &openCodeCLI{failSet: "HTTPS_PROXY"}
		stubOpenCodeCLI(t, f)
		_, _, errOut := runOC("enable", "--yes")
		if !strings.Contains(errOut, "Already set: ") || strings.Contains(errOut, "stopped") {
			t.Errorf("stderr = %q, want the keys already set and nothing about a stopped service", errOut)
		}
	})
}

// An unset that fails part way says the same about a service that was running, and does
// not restart it either.
func TestConfigureOpenCode_DisableFailingPartWay(t *testing.T) {
	f := &openCodeCLI{failUnset: "HTTP_PROXY", running: true, procEnv: []string{"HTTPS_PROXY=http://127.0.0.1:47600"}}
	home := stubOpenCodeCLI(t, f)
	maps.Copy(f.env, openCodeWant(home))
	code, out, errOut := runOC("disable", "--yes")
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if strings.Contains(out, "Disabled") {
		t.Errorf("reported success:\n%s", out)
	}
	want := "agentop: opencode service unset env HTTP_PROXY: exit status 1\n" +
		"  Run agentop configure opencode disable again to finish\n" +
		"  OpenCode's background service may be stopped; re-run this command to finish.\n"
	if errOut != want {
		t.Errorf("stderr =\n%s\nwant\n%s", errOut, want)
	}
	if n := f.restarts(); n != 0 {
		t.Errorf("restarted the service %d times after a failed change", n)
	}
}

func TestConfigureOpenCode_Usage(t *testing.T) {
	stubOpenCodeCLI(t, &openCodeCLI{})
	for _, tc := range []struct {
		name     string
		args     []string
		code     int
		onStdout bool
	}{
		{name: "no args", args: nil, code: 2},
		{name: "--help", args: []string{"--help"}, code: 0, onStdout: true},
		{name: "help after the action", args: []string{"status", "--help"}, code: 0, onStdout: true},
		{name: "unknown action", args: []string{"enabel"}, code: 2},
		// status writes nothing, so there is nothing for --yes to skip.
		{name: "status --yes", args: []string{"status", "--yes"}, code: 2},
		{name: "a stray argument", args: []string{"enable", "now"}, code: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := runOpenCode(tc.args, &out, &errb)
			if code != tc.code {
				t.Errorf("exit %d, want %d; stderr %q", code, tc.code, errb.String())
			}
			if got := strings.Contains(out.String(), "agentop configure opencode —"); got != tc.onStdout {
				t.Errorf("usage on stdout = %v, want %v:\n%s", got, tc.onStdout, out.String())
			}
		})
	}
}

// Every opencode CLI call configure makes judges agentop's own variables against Cortex's
// values from the config it was given, so a Cortex on a port no shape check knows is still
// recognised and kept from the CLI. The values are cleared once the command is done.
func TestConfigureOpenCode_TheCLIIsJudgedByTheCortexConfig(t *testing.T) {
	for _, action := range []string{"enable", "disable", "status"} {
		t.Run(action, func(t *testing.T) {
			f := &openCodeCLI{}
			home := stubOpenCodeCLI(t, f)
			writeOpenCodeCortexCfg(t, home, strings.Replace(cortexCfg, `"127.0.0.1:47600"`, `"127.0.0.1:8081"`, 1))
			args := []string{action}
			if action != "status" {
				args = append(args, "--yes")
			}
			if code, _, errOut := runOC(args...); code != 0 {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			if got := f.cliWant["HTTPS_PROXY"]; got != "http://127.0.0.1:8081" {
				t.Errorf("the CLI was judged against HTTPS_PROXY %q, want the config's http://127.0.0.1:8081", got)
			}
			if openCodeCLIWant != nil {
				t.Errorf("openCodeCLIWant = %v after the command, want nil", openCodeCLIWant)
			}
		})
	}
	// Without a readable config there is nothing to judge by but the values' shape.
	t.Run("disable, no config", func(t *testing.T) {
		f := &openCodeCLI{}
		home := stubOpenCodeCLI(t, f)
		if err := os.Remove(filepath.Join(home, ".cortex", "config.yaml")); err != nil {
			t.Fatal(err)
		}
		if code, _, errOut := runOC("disable", "--yes"); code != 0 {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		if f.cliWant != nil {
			t.Errorf("the CLI was judged against %v, want nothing", f.cliWant)
		}
	})
}

// The usage gives each action the flags it takes, so status shows no --yes, and says
// what a change does to a running service: one restart, after it.
func TestOpenCodeUsage_SynopsisAndRestart(t *testing.T) {
	if want := "  agentop configure opencode enable | disable [--yes] [--config PATH] [--opencode BIN]\n" +
		"  agentop configure opencode status [--config PATH] [--opencode BIN]\n\n"; !strings.Contains(openCodeUsage, want) {
		t.Errorf("openCodeUsage lacks %q", want)
	}
	prose := strings.Join(strings.Fields(openCodeUsage), " ")
	for _, want := range []string{
		"enable and disable restart it once after their change, which interrupts every OpenCode session using it; " +
			"an open OpenCode reconnects to it.",
		"(--yes skips the question, not the warning)",
	} {
		if !strings.Contains(prose, want) {
			t.Errorf("openCodeUsage lacks %q", want)
		}
	}
	for _, stale := range []string{"ends every OpenCode session", "keeps the environment it started with, and each says"} {
		if strings.Contains(prose, stale) {
			t.Errorf("openCodeUsage still says %q", stale)
		}
	}
}

// With no --opencode and no opencode to find, every action says how to name one.
func TestConfigureOpenCode_NoOpenCode(t *testing.T) {
	f := &openCodeCLI{}
	stubOpenCodeCLI(t, f)
	t.Setenv("PATH", t.TempDir())
	var out, errb bytes.Buffer
	if code := runOpenCode([]string{"status"}, &out, &errb); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "opencode not found") || !strings.Contains(errb.String(), "--opencode") {
		t.Errorf("stderr = %q, want the lookup failure and the flag", errb.String())
	}
	if len(f.calls) != 0 {
		t.Errorf("ran %v with no binary", f.calls)
	}
}

// `configure opencode` reaches runOpenCode, and no longer prints the coming-soon text.
func TestConfigure_OpenCodeReachesRunOpenCode(t *testing.T) {
	stubOpenCodeCLI(t, &openCodeCLI{})

	var viaConfigure, configureErr bytes.Buffer
	configureCode := runConfigure([]string{"opencode", "status", "--opencode", fakeOpenCodePath}, &viaConfigure, &configureErr)
	var direct, directErr bytes.Buffer
	directCode := runOpenCode([]string{"status", "--opencode", fakeOpenCodePath}, &direct, &directErr)

	if configureCode != directCode || viaConfigure.String() != direct.String() || configureErr.String() != directErr.String() {
		t.Errorf("configure: %d %q %q\nrunOpenCode: %d %q %q", configureCode, viaConfigure.String(), configureErr.String(),
			directCode, direct.String(), directErr.String())
	}
	if strings.Contains(viaConfigure.String(), "coming soon") {
		t.Errorf("still the coming-soon text:\n%s", viaConfigure.String())
	}
	if !strings.Contains(viaConfigure.String(), "not fully enabled (0 of 9 set)") {
		t.Errorf("stdout is not status's:\n%s", viaConfigure.String())
	}
}

// configure's own usage names the new command and no longer sends OpenCode users to exec.
func TestConfigureUsage_OpenCodePersists(t *testing.T) {
	if want := "  agentop configure opencode enable | disable [--yes] [--config PATH] [--opencode BIN]\n" +
		"  agentop configure opencode status [--config PATH] [--opencode BIN]\n"; !strings.Contains(configureUsage, want) {
		t.Errorf("configureUsage lacks %q", want)
	}
	// The prose is wrapped, so it is compared with its whitespace collapsed.
	prose := strings.Join(strings.Fields(configureUsage), " ")
	for _, want := range []string{
		`"agentop configure opencode --help"`,
		"Five agents persist, by four different mechanisms.",
		"OpenCode's background service keeps an environment of its own, so its configuration goes there, through the opencode CLI.",
	} {
		if !strings.Contains(prose, want) {
			t.Errorf("configureUsage lacks %q", want)
		}
	}
	for _, stale := range []string{
		`use "agentop exec -- opencode"`, "Codex and OpenCode read", "configure codex | opencode",
		"Four agents persist, by three different mechanisms.", "Codex reads the process environment and nothing else",
	} {
		if strings.Contains(prose, stale) {
			t.Errorf("configureUsage still says %q", stale)
		}
	}
}
