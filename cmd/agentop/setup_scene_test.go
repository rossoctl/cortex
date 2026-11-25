package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// stubCache holds one read-only file per distinct stub body, for this test
// binary. macOS scans an executable file on its first exec, and one process does
// that for the whole machine. A hard link to a file that already ran is not
// scanned again, so linkStub puts the same file at each test's path.
var stubCache struct {
	sync.Mutex
	dir string
}

// linkStub puts an executable with body at path, as a hard link to the cached
// file for that body. The file is read-only, so a test that writes over a stub in
// place fails, and does not change the stub of every other test. Such a test must
// remove the stub first. A per-test value goes in an environment variable that
// the stub reads, not in body: each different body is a new file.
func linkStub(t *testing.T, path, body string) {
	t.Helper()
	stubCache.Lock()
	defer stubCache.Unlock()
	if stubCache.dir == "" {
		dir, err := os.MkdirTemp("", "agentop-stubs-")
		if err != nil {
			t.Fatal(err)
		}
		stubCache.dir = dir
	}
	sum := sha256.Sum256([]byte(body))
	src := filepath.Join(stubCache.dir, hex.EncodeToString(sum[:]))
	if _, err := os.Stat(src); err != nil {
		if err := os.WriteFile(src, []byte(body), 0o555); err != nil { //nolint:gosec // a test stub
			t.Fatal(err)
		}
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Link(src, path); err != nil {
		t.Fatal(err)
	}
}

// removeStubCache removes linkStub's folder. TestMain calls it after the run.
func removeStubCache() {
	if stubCache.dir != "" {
		_ = os.RemoveAll(stubCache.dir)
	}
}

// plainOutput clears CLICOLOR_FORCE, which makes termenv colour a writer that is
// not a terminal, for a test that asserts setup's plain lines.
func plainOutput(t *testing.T) {
	t.Helper()
	t.Setenv("CLICOLOR_FORCE", "")
}

func writeExe(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// A new file, not a write through: path can be one of linkStub's shared links.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil { //nolint:gosec
		t.Fatal(err)
	}
}

// stageDir is what install.sh leaves for setup: agentop and cortex side by side.
func stageDir(t *testing.T, cortexScript string) string {
	t.Helper()
	dir := t.TempDir()
	linkStub(t, filepath.Join(dir, "agentop"), "#!/bin/sh\necho agentop v9.9.9\n")
	linkStub(t, filepath.Join(dir, "cortex"), cortexScript)
	return dir
}

// cortexStub is a stand-in cortex for setup's tests: --version, the built-in
// config writer (with a health_addr the test serves), a long-running
// --supervise for the unsupervised path, the unit-file forms that
// cmd_service_platform.go writes (launchd: --supervise --config <path>,
// systemd: --config <path>), and --fake-start, the line a
// runningCortex load logs. Its config is deliberately unpinned, as
// an old built-in one would be, so the config step's migrations have work to do.
// It has context-guru and a metrics_push key, so the two migrations for those
// have none: their own tests cover them.
func cortexStub(t *testing.T, healthAddr string) string {
	t.Helper()
	t.Setenv("CORTEX_STUB_HEALTH", healthAddr)
	return `#!/bin/sh
case "$*" in
  --version) echo "cortex v9.9.9" ;;
  "--local --write-config")
    mkdir -p "$HOME/.cortex" && chmod 700 "$HOME/.cortex"
    if [ ! -f "$HOME/.cortex/config.yaml" ]; then
      cat > "$HOME/.cortex/config.yaml" <<EOF
mode: proxy-sidecar
listener:
  roles: [forward]
  forward_proxy_addr: 127.0.0.1:47600
  health_addr: $CORTEX_STUB_HEALTH
tls_bridge:
  mode: enabled
  ca_dir: "$HOME/.cortex/ca"
  generate_ca: true
pipeline:
  outbound:
    plugins:
      - name: inference-parser
      - name: context-guru
metrics_push:
  enabled: false
EOF
      chmod 600 "$HOME/.cortex/config.yaml"
    fi ;;
  "--local --supervise") exec sleep 300 ;;
  "--supervise --config "*) exec sleep 300 ;;
  "--config "*) exec sleep 300 ;;
  --fake-start) echo "cortex stub: listening on 127.0.0.1:47600" ;;
  *) echo "cortex stub: unexpected $*" >&2; exit 2 ;;
esac
`
}

// skipSceneInShort skips a setup, service, doctor or uninstall scene under
// go test -short. These scenes start many stub processes, and they are most of
// the suite's time. fakeSupervisor, fakeNoSupervisor and freePorts call it, so
// every scene built on them skips. CI does not use -short.
func skipSceneInShort(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("scene test: skipped under -short. Run the full suite before you push.")
	}
}

// freePorts makes every port read free: these tests run beside a real Cortex.
// lsof and ss find no listener either, so portHolder names no holder — the real
// lsof would name that Cortex. A later installStub of either still wins.
func freePorts(t *testing.T) {
	t.Helper()
	skipSceneInShort(t)
	saved := portInUse
	portInUse = func(string) bool { return false }
	t.Cleanup(func() { portInUse = saved })
	installFalse(t, "lsof")
	installFalse(t, "ss")
}

// serviceEnv is a setupEnv with the stub cortex installed, its config written,
// and a health endpoint served by healthz. The caller installs fakeSupervisor
// or fakeNoSupervisor first.
func serviceEnv(t *testing.T, healthz http.HandlerFunc) *setupEnv {
	t.Helper()
	freePorts(t)
	srv := httptest.NewServer(healthz)
	t.Cleanup(srv.Close)
	env := newTestSetupEnv(t)
	writeExe(t, filepath.Join(env.binDir, "cortex"), cortexStub(t, strings.TrimPrefix(srv.URL, "http://")))
	out, err := exec.Command(filepath.Join(env.binDir, "cortex"), "--local", "--write-config").CombinedOutput()
	if err != nil {
		t.Fatalf("stub write-config: %v %s", err, out)
	}
	return env
}
