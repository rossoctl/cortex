package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const endpointCfg = `mode: proxy-sidecar
listener:
  roles: [forward]
  forward_proxy_addr: 127.0.0.1:47600
  session_api_addr: "SESSIONADDR"
  health_addr: 127.0.0.1:47604
pipeline:
  outbound:
    plugins:
      - name: inference-parser
`

// withCortexConfig points $HOME at a temp dir holding a Cortex config whose
// session_api_addr is addr. An empty addr writes no config at all.
func withCortexConfig(t *testing.T, addr string) {
	t.Helper()
	dir := t.TempDir()
	if addr != "" {
		if err := os.MkdirAll(filepath.Join(dir, ".cortex"), 0o700); err != nil {
			t.Fatal(err)
		}
		body := strings.Replace(endpointCfg, "SESSIONADDR", addr, 1)
		if err := os.WriteFile(filepath.Join(dir, ".cortex", "config.yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", dir)
}

// TestLocalSessionEndpoint_ReadsTheConfiguredPort: the in-cluster default is 9094
// and a local install uses 47601, so a hardcoded constant is wrong for one of
// them. It must follow whatever the operator actually configured.
func TestLocalSessionEndpoint_ReadsTheConfiguredPort(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want string
	}{
		{"127.0.0.1:47601", "http://127.0.0.1:47601"},
		{"127.0.0.1:19999", "http://127.0.0.1:19999"},
		// A bind address is not a dial address.
		{":9094", "http://localhost:9094"},
		{"0.0.0.0:9094", "http://localhost:9094"},
		// IPv6. strings.Cut split at the first colon and produced host="[",
		// yielding an unusable URL; the brackets must also survive into the
		// authority.
		{"[::1]:47601", "http://[::1]:47601"},
		{"[fe80::1]:9094", "http://[fe80::1]:9094"},
		// The v6 wildcard is a real host after SplitHostPort, so the branch that
		// rewrites it to localhost is finally reachable.
		{"[::]:9094", "http://localhost:9094"},
	} {
		withCortexConfig(t, tc.addr)
		if got := localSessionEndpoint(); got != tc.want {
			t.Errorf("session_api_addr %q -> %q, want %q", tc.addr, got, tc.want)
		}
	}
}

// TestLocalSessionEndpoint_NoConfigMeansNoLocalEndpoint: a machine that never
// installed Cortex must fall through to the cluster picker, not to a guess.
func TestLocalSessionEndpoint_NoConfigMeansNoLocalEndpoint(t *testing.T) {
	withCortexConfig(t, "")
	if got := localSessionEndpoint(); got != "" {
		t.Errorf("got %q, want empty with no config", got)
	}
}

// A config that is there but will not load is named as such, not reported as no
// local Cortex at all: the proxy that rejected the same edit may still be serving
// (#1282). One that loads, or is not there, is no problem.
func TestLocalConfigProblem(t *testing.T) {
	withCortexConfig(t, "")
	if got := localConfigProblem(); got != "" {
		t.Errorf("no config: %q, want empty", got)
	}
	withCortexConfig(t, "127.0.0.1:47601")
	if got := localConfigProblem(); got != "" {
		t.Errorf("a config that loads: %q, want empty", got)
	}
	cfg := filepath.Join(os.Getenv("HOME"), ".cortex", "config.yaml")
	if err := os.WriteFile(cfg, []byte("listener:\n  roles: [forward\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := localConfigProblem(); !strings.HasPrefix(got, "~/.cortex/config.yaml will not load: parsing config: ") {
		t.Errorf("a broken config: %q, want it named", got)
	}
}

// TestLocalSessionAPIUp_OnlyWhenSomethingAnswers is what keeps a stale config
// from hijacking agentop: an install that is no longer running must not steer
// someone away from the cluster picker.
func TestLocalSessionAPIUp_OnlyWhenSomethingAnswers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"sessions":[]}`))
	}))
	defer srv.Close()

	if !localSessionAPIUp(srv.URL) {
		t.Error("a live session API was reported down")
	}
	if localSessionAPIUp("") {
		t.Error("empty endpoint reported up")
	}
	// A port with nothing on it: the server above, closed.
	dead := srv.URL
	srv.Close()
	if localSessionAPIUp(dead) {
		t.Error("a closed port was reported up")
	}
}

// TestLocalSessionAPIUp_RejectsNon2xx: only a 2xx proves the session API is
// there. Anything else means some other service holds the port, and selecting it
// sends agentop somewhere useless instead of to the cluster picker.
func TestLocalSessionAPIUp_RejectsNon2xx(t *testing.T) {
	for _, status := range []int{
		http.StatusInternalServerError,
		http.StatusNotFound,     // an unrelated service on the port
		http.StatusUnauthorized, // something that wants credentials
		http.StatusMovedPermanently,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		if localSessionAPIUp(srv.URL) {
			t.Errorf("HTTP %d was accepted as a live session API", status)
		}
		srv.Close()
	}
}

// TestLocalSessionEndpoint_RejectsMalformedAddresses: a bad address must yield no
// endpoint rather than a URL that merely fails to connect, because the
// consequence of the latter is agentop falling silently through to the cluster
// picker with no explanation.
func TestLocalSessionEndpoint_RejectsMalformedAddresses(t *testing.T) {
	for _, addr := range []string{
		"127.0.0.1",  // no port
		"::",         // too many colons, not a host:port
		"::1:47601",  // unbracketed v6, ambiguous
		"127.0.0.1:", // empty port
	} {
		withCortexConfig(t, addr)
		if got := localSessionEndpoint(); got != "" {
			t.Errorf("session_api_addr %q -> %q, want empty", addr, got)
		}
	}
}

// pipeline get and cost fall back to the local Cortex; with its config broken they
// name the file, where they used to say no local Cortex was configured (#1282).
func TestLocalFallbackNamesABrokenConfig(t *testing.T) {
	withCortexConfig(t, "")
	dir := filepath.Join(os.Getenv("HOME"), ".cortex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("listener:\n  roles: [forward\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, run := range map[string]func(stdout, stderr io.Writer) int{
		"agentop pipeline get": func(o, e io.Writer) int { return runPipeline([]string{"get"}, o, e) },
		"agentop cost":         func(o, e io.Writer) int { return runCost(nil, o, e) },
	} {
		var out, errOut strings.Builder
		if code := run(&out, &errOut); code != 1 {
			t.Errorf("%s: exit %d, want 1", name, code)
		}
		want := name + ": no --endpoint given, and ~/.cortex/config.yaml will not load: parsing config: "
		if !strings.HasPrefix(errOut.String(), want) || strings.Contains(errOut.String(), "no local Cortex is configured") {
			t.Errorf("%s: stderr =\n%s\nwant it to start %q", name, errOut.String(), want)
		}
	}
}
