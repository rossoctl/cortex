package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/config"
)

// praxisServer is a running Cortex whose GET /config reports praxis.enabled as on.
func praxisServer(t *testing.T, on bool) string {
	t.Helper()
	body := `{"listener":{"forward_proxy_addr":"127.0.0.1:47600"}}`
	if on {
		body = `{"listener":{"forward_proxy_addr":"127.0.0.1:47600"},"praxis":{"enabled":true}}`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// praxisConfigFile writes a config with praxis mode on. Its stats address has no
// listener, so no local Cortex is read.
func praxisConfigFile(t *testing.T) (path, src string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	src = `mode: proxy-sidecar
stats:
  address: ` + closedAddr(t) + `
listener:
  bind_loopback_only: true
praxis:
  enabled: true
pipeline:
  outbound:
    plugins:
      - name: inference-parser
      - name: tool-prune
        config:
          remove: []
`
	path = filepath.Join(home, "cortex.yaml")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, src
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("config changed:\n%s", got)
	}
}

func TestPraxisRefusal_DirectMode(t *testing.T) {
	if err := praxisRefusal(&config.Config{}, "/c.yaml", praxisServer(t, false)); err != nil {
		t.Errorf("got %v, want nil", err)
	}
}

func TestPraxisRefusal_FileOnly(t *testing.T) {
	cfg := &config.Config{Praxis: &config.PraxisConfig{Enabled: true}}
	err := praxisRefusal(cfg, "/c.yaml", "")
	if err == nil {
		t.Fatal("got nil, want a refusal")
	}
	for _, want := range []string{"praxis mode is on (in /c.yaml)", "set praxis.enabled: false in /c.yaml", "agentop service restart"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q does not contain %q", err, want)
		}
	}
}

// The file says direct mode, but the running Cortex still runs praxis mode.
func TestPraxisRefusal_RunningOnly(t *testing.T) {
	err := praxisRefusal(&config.Config{}, "/c.yaml", praxisServer(t, true))
	if err == nil || !strings.Contains(err.Error(), "praxis mode is on (in the running Cortex)") {
		t.Errorf("got %v, want a refusal that names the running Cortex", err)
	}
}

func TestPraxisRefusal_Both(t *testing.T) {
	cfg := &config.Config{Praxis: &config.PraxisConfig{Enabled: true}}
	err := praxisRefusal(cfg, "/c.yaml", praxisServer(t, true))
	if err == nil || !strings.Contains(err.Error(), "(in /c.yaml and in the running Cortex)") {
		t.Errorf("got %v, want a refusal that names both", err)
	}
}

func TestServerAdd_RefusedInPraxisMode(t *testing.T) {
	path, src := praxisConfigFile(t)
	stubServerModels(t, testModels, anyModels)
	code, _, errOut := runServerCmd(t, "", "add", "ete", "https://ete.example.com", "--main", "opus", "--helper", "haiku", "--yes", "--config", path)
	if code != 1 || !strings.Contains(errOut, "praxis mode is on") {
		t.Errorf("exit %d, stderr: %s", code, errOut)
	}
	assertFile(t, path, src)
}

func TestScriptList_RefusedInPraxisMode(t *testing.T) {
	path, _ := praxisConfigFile(t)
	code, _, errOut := runScriptCmd(t, "list", "--config", path)
	if code != 1 || !strings.Contains(errOut, "praxis mode is on") {
		t.Errorf("exit %d, stderr: %s", code, errOut)
	}
}

func praxisTranscripts(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "projects")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, dir, "a.jsonl", true)
	return dir
}

func TestToolsScanWrite_RefusedInPraxisMode(t *testing.T) {
	path, src := praxisConfigFile(t)
	var out, errb bytes.Buffer
	if code := runTools([]string{"scan", "--dir", praxisTranscripts(t), "--write", path}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "praxis mode is on") {
		t.Errorf("exit %d, stderr: %s", code, errb.String())
	}
	assertFile(t, path, src)
}

// Without --write the scan only reads transcripts, so praxis mode does not stop it.
func TestToolsScan_NoWriteWorksInPraxisMode(t *testing.T) {
	praxisConfigFile(t)
	var out, errb bytes.Buffer
	if code := runTools([]string{"scan", "--dir", praxisTranscripts(t)}, &out, &errb); code != 0 {
		t.Errorf("exit %d, stderr: %s", code, errb.String())
	}
}
