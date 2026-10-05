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

// spoilConfig leaves the scene's config one that will not load, broken as a hand
// edit leaves it or missing as setup's "move it aside" remedy leaves it, and resolves
// the paths again, as the next agentop command would.
func (sc *serviceScene) spoilConfig(t *testing.T, missing bool) {
	t.Helper()
	var err error
	if missing {
		err = os.Remove(sc.p.configFile)
	} else {
		err = os.WriteFile(sc.p.configFile, []byte(brokenConfig), 0o600)
	}
	if err != nil {
		t.Fatal(err)
	}
	p, err := resolveServicePaths(sc.p.configFile, sc.p.unitFile, sc.p.binary)
	if err != nil {
		t.Fatal(err)
	}
	if p.configErr == nil {
		t.Fatal("the spoiled config loaded")
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
// the config it last loaded. A stop needs no config, and still works. A missing
// config is worded as install words it, not as a file to fix.
func TestServiceControl_RefusesToStartOntoABrokenConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		missing bool
		what    func(sc serviceScene) string
		fix     string
	}{
		{"broken", false, func(sc serviceScene) string {
			return "$HOME/.cortex/config.yaml will not load: " + sc.p.configErr.Error()
		}, "Fix the file"},
		{"missing", true, func(serviceScene) string {
			return "no config at $HOME/.cortex/config.yaml"
		}, "Create it with `cortex --local --write-config`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loaded := fakeSupervisor(t)
			sc := newServiceSceneServing(t, loaded)
			sc.writeUnit(t)
			if r := sc.control(t, "restart"); r.code != 0 {
				t.Fatalf("restart onto a good config, exit %d: %s%s", r.code, r.out, r.errOut)
			}
			// The scene's health endpoint follows the fake job, as the built-in one
			// would follow a proxy serving from before the edit.
			withBuiltinHealthURL(t, sc.p.healthURL)
			sc.spoilConfig(t, tc.missing)

			for _, action := range []string{"restart", "start"} {
				want := "agentop: " + tc.what(sc) + ", so Cortex could not start.\n" +
					"  Nothing was stopped or started: a running Cortex keeps the config it last loaded.\n" +
					"  " + tc.fix + ", then run: agentop service " + action + "\n"
				sc.control(t, action).check(t, 1, "", want)
				wantFile(t, loaded, true, "after a refused "+action+" (the job must not have been booted out)")
			}

			sc.control(t, "stop").check(t, 0,
				"Cortex proxy stopped, and it will stay stopped across logins.\n  agentop service start\n", "")
			wantFile(t, loaded, false, "after a stop with a "+tc.name+" config")
		})
	}
}

// With the config unreadable, a stop asks the built-in health address whether
// anything was serving, and silence there is not a no: the file may have moved
// health_addr. So the stop says what is now true rather than "was not running".
func TestServiceControl_StopCannotTellWithABrokenConfig(t *testing.T) {
	loaded := fakeSupervisor(t)
	sc := newServiceSceneServing(t, loaded)
	sc.writeUnit(t)
	if r := sc.control(t, "restart"); r.code != 0 {
		t.Fatalf("restart onto a good config, exit %d: %s%s", r.code, r.out, r.errOut)
	}
	withBuiltinHealthURL(t, "http://127.0.0.1:1/healthz") // the file had moved it
	sc.spoilConfig(t, false)
	sc.control(t, "stop").check(t, 0,
		"Cortex proxy is stopped, and it will stay stopped across logins.\n  agentop service start\n", "")
	wantFile(t, loaded, false, "after the stop")
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

// statusScene is an installed service whose config will not load: broken, or
// missing.
func statusScene(t *testing.T, missing bool) servicePaths {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if !missing {
		if err := os.WriteFile(cfg, []byte(brokenConfig), 0o600); err != nil {
			t.Fatal(err)
		}
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
// built-in health address, and exits 1 either way: the file needs fixing. Silence at
// that address is reported as only that, since the file may have moved health_addr.
func TestServiceStatus_BrokenConfig(t *testing.T) {
	status := func(t *testing.T, p servicePaths, health string) string {
		t.Helper()
		withBuiltinHealthURL(t, health)
		var out bytes.Buffer
		if code := serviceStatus(p, &out); code != 1 {
			t.Errorf("exit = %d, want 1", code)
		}
		return out.String()
	}
	wantAll := func(t *testing.T, out string, wants ...string) {
		t.Helper()
		for _, want := range wants {
			if !strings.Contains(out, want) {
				t.Errorf("status lacks %q:\n%s", want, out)
			}
		}
	}
	serving := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(serving.Close)

	t.Run("a proxy serving from before the edit", func(t *testing.T) {
		p := statusScene(t, false)
		wantAll(t, status(t, p, serving.URL+"/healthz"),
			"config: "+p.configFile+" will not load: parsing config: ",
			"\n  Fix the file. A running Cortex keeps the config it last loaded, but none can start until then.\n",
			"Cortex is healthy according to "+serving.URL+"/healthz, serving the config it last loaded\n")
	})
	t.Run("nothing at the built-in address", func(t *testing.T) {
		p := statusScene(t, false)
		out := status(t, p, "http://127.0.0.1:1/healthz")
		wantAll(t, out,
			"config: "+p.configFile+" will not load: parsing config: ",
			"Nothing answers http://127.0.0.1:1/healthz, the built-in health address.\n"+
				"  The config is what names the address Cortex serves on, so this does not say whether it is running.\n"+
				"  Last log lines:\n")
		for _, never := range []string{"NOT answering", "will fail"} {
			if strings.Contains(out, never) {
				t.Errorf("status claims %q from silence at the built-in address:\n%s", never, out)
			}
		}
	})
	t.Run("missing", func(t *testing.T) {
		p := statusScene(t, true)
		out := status(t, p, serving.URL+"/healthz")
		wantAll(t, out,
			"config: no config at "+p.configFile+"\n"+
				"  Create it with `cortex --local --write-config`. A running Cortex keeps the config it last loaded, but none can start until then.\n")
		if strings.Contains(out, "no such file") {
			t.Errorf("status passes on the read error for a missing file:\n%s", out)
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
