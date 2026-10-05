package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// brokenConfig is a config.yaml that will not parse, as a hand edit leaves one.
const brokenConfig = "listener:\n  roles: [forward\n"

// control runs `agentop service <action>` against the scene.
func (sc serviceScene) control(t *testing.T, action string) svcRun {
	t.Helper()
	var out, errb bytes.Buffer
	code := serviceControl(action, sc.p, &out, &errb)
	return svcRun{code, sc.norm(out.String()), sc.norm(errb.String())}
}

// writeUnit puts a unit on disk, which is all serviceControl asks of "installed".
func (sc serviceScene) writeUnit(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(sc.p.unitFile, []byte(renderUnit(sc.p)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// breakConfig overwrites the scene's config with one that will not load, and
// resolves the paths again, as the next agentop command would.
func (sc *serviceScene) breakConfig(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(sc.p.configFile, []byte(brokenConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := resolveServicePaths(sc.p.configFile, sc.p.unitFile, sc.p.binary)
	if err != nil {
		t.Fatal(err)
	}
	if p.configErr == nil {
		t.Fatal("the broken config loaded")
	}
	sc.p = p
}

// withBuiltinHealthURL points the built-in health address at url for one test, so
// it never reaches the Cortex on the machine running it.
func withBuiltinHealthURL(t *testing.T, url string) {
	t.Helper()
	saved := builtinHealthURL
	builtinHealthURL = url
	t.Cleanup(func() { builtinHealthURL = saved })
}

// The outcome is named for the proxy and for what happened to it, not echoed from
// the command: "Restarted." over a proxy that had not been running, and a bare
// "Restarted." at all, are what #1282 reported.
func TestServiceControl_SaysWhatHappenedToTheProxy(t *testing.T) {
	loaded := fakeSupervisor(t)
	sc := newServiceSceneServing(t, loaded)
	sc.writeUnit(t)

	sc.control(t, "restart").check(t, 0, "Cortex proxy started.\n", "")
	wantFile(t, loaded, true, "after a restart from nothing")
	sc.control(t, "restart").check(t, 0, "Cortex proxy restarted.\n", "")

	sc.control(t, "stop").check(t, 0,
		"Cortex proxy stopped, and it will stay stopped across logins.\n  agentop service start\n", "")
	wantFile(t, loaded, false, "after a stop")
	sc.control(t, "stop").check(t, 0,
		"Cortex proxy was not running; it will stay stopped across logins.\n  agentop service start\n", "")
}

// A start or restart onto a config that will not load is refused before the
// supervisor is asked anything, so the proxy that rejected the edit keeps serving
// the config it last loaded. A stop needs no config, and still works.
func TestServiceControl_RefusesToStartOntoABrokenConfig(t *testing.T) {
	loaded := fakeSupervisor(t)
	sc := newServiceSceneServing(t, loaded)
	sc.writeUnit(t)
	if r := sc.control(t, "restart"); r.code != 0 {
		t.Fatalf("restart onto a good config, exit %d: %s%s", r.code, r.out, r.errOut)
	}
	// The scene's health endpoint follows the fake job, as the built-in one would
	// follow a proxy serving from before the edit.
	withBuiltinHealthURL(t, sc.p.healthURL)
	sc.breakConfig(t)

	for _, action := range []string{"restart", "start"} {
		r := sc.control(t, action)
		want := "agentop: $HOME/.cortex/config.yaml will not load, so Cortex could not start:\n" +
			"  " + sc.p.configErr.Error() + "\n" +
			"  Nothing was stopped or started: a running Cortex keeps the config it last loaded.\n" +
			"  Fix the file, then run: agentop service " + action + "\n"
		r.check(t, 1, "", want)
		wantFile(t, loaded, true, "after a refused "+action+" (the job must not have been booted out)")
	}

	sc.control(t, "stop").check(t, 0,
		"Cortex proxy stopped, and it will stay stopped across logins.\n  agentop service start\n", "")
	wantFile(t, loaded, false, "after a stop with a broken config")
}

func TestControlVerb(t *testing.T) {
	for _, tc := range []struct {
		goos, action string
		wasServing   bool
		want         string
	}{
		{"darwin", "restart", true, "restarted"},
		{"linux", "restart", true, "restarted"},
		{"darwin", "restart", false, "started"},
		{"linux", "restart", false, "started"},
		{"darwin", "start", false, "started"},
		{"linux", "start", false, "started"},
		// loadService boots out a running job before bootstrapping it again.
		{"darwin", "start", true, "restarted"},
		// enable --now leaves an active unit alone.
		{"linux", "start", true, ""},
		{"darwin", "stop", true, "stopped"},
		{"darwin", "stop", false, ""},
	} {
		if got := controlVerb(tc.goos, tc.action, tc.wasServing); got != tc.want {
			t.Errorf("controlVerb(%s, %s, serving=%v) = %q, want %q", tc.goos, tc.action, tc.wasServing, got, tc.want)
		}
	}
}

// statusScene is an installed service whose config will not load.
func statusScene(t *testing.T) servicePaths {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte(brokenConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(dir, "unit")
	if err := os.WriteFile(unit, []byte("unit"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := resolveServicePaths(cfg, unit, filepath.Join(dir, "cortex"))
	if err != nil {
		t.Fatal(err)
	}
	if p.configErr == nil || p.healthURL != "" {
		t.Fatalf("configErr = %v, healthURL = %q; want an error and no URL", p.configErr, p.healthURL)
	}
	return p
}

// Status on a config that will not load used to print "installed:" alone and exit 0,
// whether or not a proxy was serving (#1282). It now names the config error, asks the
// built-in health address, and exits 1 either way: the file needs fixing.
func TestServiceStatus_BrokenConfig(t *testing.T) {
	t.Run("a proxy serving from before the edit", func(t *testing.T) {
		p := statusScene(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
		t.Cleanup(srv.Close)
		withBuiltinHealthURL(t, srv.URL+"/healthz")
		var out bytes.Buffer
		if code := serviceStatus(p, &out); code != 1 {
			t.Errorf("exit = %d, want 1", code)
		}
		for _, want := range []string{
			"config: " + p.configFile + " will not load: parsing config: ",
			"  A running Cortex keeps the config it last loaded, but none can start until this file loads.\n",
			"Cortex is healthy according to " + srv.URL + "/healthz, serving the config it last loaded\n",
		} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("status lacks %q:\n%s", want, out.String())
			}
		}
	})
	t.Run("nothing serving", func(t *testing.T) {
		p := statusScene(t)
		withBuiltinHealthURL(t, "http://127.0.0.1:1/healthz")
		var out bytes.Buffer
		if code := serviceStatus(p, &out); code != 1 {
			t.Errorf("exit = %d, want 1", code)
		}
		for _, want := range []string{
			"config: " + p.configFile + " will not load: parsing config: ",
			"Cortex is NOT answering http://127.0.0.1:1/healthz, the built-in health address\n" +
				"  Claude Code and OpenCode will fail while this is true. Last log lines:\n",
		} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("status lacks %q:\n%s", want, out.String())
			}
		}
	})
}

// A healthy status names the subject and its source, rather than "healthy: <url>".
func TestServiceStatus_HealthyNamesCortex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(srv.Close)
	p := servicePathsFixture(t)
	p.healthURL = srv.URL + "/healthz"
	if err := os.WriteFile(p.unitFile, []byte("unit"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := serviceStatus(p, &out); code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if want := "Cortex is healthy according to " + p.healthURL + "\n"; !strings.HasSuffix(out.String(), want) {
		t.Errorf("status =\n%s\nwant it to end with %q", out.String(), want)
	}
}
