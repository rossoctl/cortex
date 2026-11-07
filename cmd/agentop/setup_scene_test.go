package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil { //nolint:gosec
		t.Fatal(err)
	}
}

// stageDir is what install.sh leaves for setup: agentop and cortex side by side.
func stageDir(t *testing.T, cortexScript string) string {
	t.Helper()
	dir := t.TempDir()
	writeExe(t, filepath.Join(dir, "agentop"), "#!/bin/sh\necho agentop v9.9.9\n")
	writeExe(t, filepath.Join(dir, "cortex"), cortexScript)
	return dir
}

// cortexStub is a stand-in cortex for setup's tests: --version, the built-in
// config writer (with a health_addr the test serves), a long-running
// --supervise for the unsupervised path, the unit-file forms that
// cmd_service_platform.go writes (launchd: --supervise --config <path>,
// systemd: --config <path>), and --fake-start, the line a
// runningCortex load logs. Its config is deliberately unpinned, as
// an old built-in one would be, so the config step's migrations have work to do.
func cortexStub(healthAddr string) string {
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
  health_addr: ` + healthAddr + `
tls_bridge:
  mode: enabled
  ca_dir: "$HOME/.cortex/ca"
  generate_ca: true
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

// freePorts makes every port read free: these tests run beside a real Cortex.
// lsof and ss find no listener either, so portHolder names no holder — the real
// lsof would name that Cortex. A later installStub of either still wins.
func freePorts(t *testing.T) {
	t.Helper()
	saved := portInUse
	portInUse = func(string) bool { return false }
	t.Cleanup(func() { portInUse = saved })
	installStub(t, "lsof", "#!/bin/sh\nexit 1\n")
	installStub(t, "ss", "#!/bin/sh\nexit 1\n")
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
	writeExe(t, filepath.Join(env.binDir, "cortex"), cortexStub(strings.TrimPrefix(srv.URL, "http://")))
	out, err := exec.Command(filepath.Join(env.binDir, "cortex"), "--local", "--write-config").CombinedOutput()
	if err != nil {
		t.Fatalf("stub write-config: %v %s", err, out)
	}
	return env
}
