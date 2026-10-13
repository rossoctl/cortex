package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeLaunchctl puts a launchctl on PATH that behaves like a restricted sandbox: it
// cannot answer, exactly as reported from a real one. Delegates to installStub
// (cmd_service_systemd_test.go) so callers get the same exec.LookPath reachability
// check fakeSystemctl/fakeLoginctl have: without it, a stub that's silently
// unreachable (PATH not applied yet, or written non-executable) makes a
// zero-calls assertion pass for the wrong reason, indistinguishable from a real
// zero-calls outcome.
func fakeLaunchctl(t *testing.T, body string) {
	t.Helper()
	installStub(t, "launchctl", body)
}

// TestLabelGone_UnknownIsNotGone is the defect this fixes. launchctl print exits 113
// with "Could not find service" when a label is really absent, but 125 or a permission
// denial when it cannot see the domain. Treating the latter as "gone" walked into a
// bootstrap that failed with a bare EIO, reporting the one state we had ruled out.
func TestLabelGone_UnknownIsNotGone(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchctl only")
	}
	t.Run("really absent reads as gone", func(t *testing.T) {
		fakeLaunchctl(t, "#!/bin/sh\necho 'Could not find service \"x\" in domain for user gui: 501' >&2\nexit 113\n")
		gone, known := labelGone("gui/501/whatever")
		if !gone || !known {
			t.Errorf("gone=%v known=%v, want true/true", gone, known)
		}
	})
	t.Run("cannot query does NOT read as gone", func(t *testing.T) {
		fakeLaunchctl(t, "#!/bin/sh\necho 'Could not print domain: 125: Domain does not support specified action' >&2\nexit 125\n")
		gone, known := labelGone("gui/501/whatever")
		if gone {
			t.Error("a domain we cannot query was reported as gone — this is the bug")
		}
		if known {
			t.Error("claimed to know the state it could not query")
		}
	})
	t.Run("permission denied does NOT read as gone", func(t *testing.T) {
		fakeLaunchctl(t, "#!/bin/sh\necho 'Operation not permitted' >&2\nexit 1\n")
		if gone, _ := labelGone("gui/501/whatever"); gone {
			t.Error("a denial was reported as gone")
		}
	})
	t.Run("present reads as present", func(t *testing.T) {
		fakeLaunchctl(t, "#!/bin/sh\necho 'state = running'\nexit 0\n")
		gone, known := labelGone("gui/501/whatever")
		if gone || !known {
			t.Errorf("gone=%v known=%v, want false/true", gone, known)
		}
	})
}

// TestLaunchdUsable distinguishes "cannot talk to launchd" from "our job is not
// loaded" — the difference between an explainable failure and a bare EIO.
func TestLaunchdUsable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd only")
	}
	t.Run("a restricted domain is not usable, and says why", func(t *testing.T) {
		fakeLaunchctl(t, "#!/bin/sh\necho 'Could not print domain: 125: Domain does not support specified action' >&2\nexit 125\n")
		ok, why := launchdUsable()
		if ok {
			t.Error("reported usable in a restricted environment")
		}
		if !strings.Contains(why, "125") && !strings.Contains(why, "Domain") {
			t.Errorf("reason does not explain anything: %q", why)
		}
	})
	t.Run("a working domain is usable", func(t *testing.T) {
		fakeLaunchctl(t, "#!/bin/sh\necho 'com.apple.something'\nexit 0\n")
		if ok, why := launchdUsable(); !ok {
			t.Errorf("reported unusable against a working launchctl: %s", why)
		}
	})
}

// TestServiceInstall_RestrictedEnvironment: no unit on disk, a distinct exit code so
// install.sh can offer the unsupervised path, and a message naming the way forward.
func TestServiceInstall_RestrictedEnvironment(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd only")
	}
	fakeLaunchctl(t, "#!/bin/sh\necho 'Could not print domain: 125' >&2\nexit 125\n")
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgDir := filepath.Join(home, ".cortex")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(cfgDir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("mode: proxy-sidecar\nlistener:\n  roles: [forward]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(home, "unit.plist")
	p, err := resolveServicePaths(cfg, unit, filepath.Join(home, "proxy"))
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut strings.Builder
	code := serviceInstall(p, true, false, &out, &errOut)

	if code != exitNoSupervisor {
		t.Errorf("exit = %d, want %d so install.sh can offer the fallback", code, exitNoSupervisor)
	}
	if _, serr := os.Stat(unit); serr == nil {
		t.Error("a unit was written in an environment that cannot load it")
	}
	msg := errOut.String()
	for _, want := range []string{"cannot manage", "--local"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message is missing %q, so the user has no way forward:\n%s", want, msg)
		}
	}
}

// TestLoginHome reports the user-record home, which is what launchd scans — not $HOME.
func TestLoginHome(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("dscl only")
	}
	if _, err := exec.LookPath("dscl"); err != nil {
		t.Skip("no dscl")
	}
	// A bogus $HOME must not change the answer: that independence is the whole point.
	t.Setenv("HOME", "/tmp/definitely-not-my-home")
	got := loginHome()
	if got == "" {
		t.Skip("could not read the user record here")
	}
	if got == "/tmp/definitely-not-my-home" {
		t.Error("loginHome returned $HOME; it must come from the user record")
	}
	if !strings.HasPrefix(got, "/") {
		t.Errorf("loginHome = %q, want an absolute path", got)
	}
	_ = strconv.Itoa(os.Getuid())
}

// TestReportSessionInterruption: replacing a running Cortex cuts whatever is attached,
// and nothing on this side can make that graceful — HTTPS_PROXY is fixed in each
// client's environment at startup. A count turns the resulting "connection refused" into
// a five-second diagnosis. Silence when there is nothing to say matters just as much:
// a confident "0 connections" when we could not look would be worse than no number.
func TestReportSessionInterruption(t *testing.T) {
	t.Run("silent when the address is unknown", func(t *testing.T) {
		var out strings.Builder
		reportSessionInterruption(servicePaths{forwardAddr: ""}, &out)
		if out.Len() != 0 {
			t.Errorf("said something with nothing to go on: %q", out.String())
		}
	})
	t.Run("silent when the address is unparseable", func(t *testing.T) {
		var out strings.Builder
		reportSessionInterruption(servicePaths{forwardAddr: "not-an-address"}, &out)
		if out.Len() != 0 {
			t.Errorf("guessed at a count: %q", out.String())
		}
	})
	t.Run("silent on a port nothing is connected to", func(t *testing.T) {
		var out strings.Builder
		// A port in the ephemeral range that is almost certainly idle. If something is
		// attached the assertion would be wrong rather than the code, so tolerate it.
		reportSessionInterruption(servicePaths{forwardAddr: "127.0.0.1:59999"}, &out)
		if strings.Contains(out.String(), "0 connection") {
			t.Errorf("reported a zero count instead of staying quiet: %q", out.String())
		}
	})

	// The branch that actually says something, which had no coverage at all — the three
	// cases above are the quiet paths, so the wording an operator reads was the one part
	// of this function no test touched.
	//
	// What it must NOT say is that a session cannot reconnect, or that one should be
	// restarted: clients come back in well under a second, so a restart costs the requests
	// in flight and not the sessions. The measurements live in docs/laptop-service.md.
	t.Run("names the cost when connections are attached", func(t *testing.T) {
		// Fatalf, not Skipf: a loopback listen that fails is infrastructure breakage,
		// and a skip here would report success for the one test that pins the wording
		// this whole change exists to correct.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("cannot listen on loopback: %v", err)
		}
		defer ln.Close() //nolint:errcheck

		accepted := make(chan net.Conn, 1)
		go func() {
			c, aerr := ln.Accept()
			if aerr == nil {
				accepted <- c
			}
		}()
		client, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("cannot dial the listener we just opened: %v", err)
		}
		defer client.Close() //nolint:errcheck
		select {
		case c := <-accepted:
			defer c.Close() //nolint:errcheck
		case <-time.After(2 * time.Second):
			t.Fatal("our own listener did not accept in 2s")
		}

		var out strings.Builder
		reportSessionInterruption(servicePaths{forwardAddr: ln.Addr().String()}, &out)
		got := out.String()
		if got == "" {
			t.Fatalf("said nothing with a connection attached to %s", ln.Addr())
		}
		// "in flight" is the phrase unique to the new wording, and the only positive
		// assertion here that a revert fails. "will be cut" matched the old text too
		// ("are attached and will be cut"), and so did "reconnect" ("cannot reconnect on
		// its own") — so those two passed on both texts and the retracted-phrase loop
		// below was doing all the work.
		if !strings.Contains(got, "in flight") {
			t.Errorf("does not name the cost as the in-flight request: %q", got)
		}
		if !strings.Contains(got, "will be cut") {
			t.Errorf("does not say the connections are cut: %q", got)
		}
		for _, retracted := range []string{"cannot reconnect", "restart any session"} {
			if strings.Contains(strings.ToLower(got), retracted) {
				t.Errorf("repeats retracted advice %q: %q", retracted, got)
			}
		}
	})
}

// TestServiceIsCurrent covers the no-op decision. Each clause is a way for
// "installed" to be a lie, and getting any of them wrong means either a pointless
// restart — which cuts every attached connection, costing each client its in-flight
// request — or skipping a real upgrade.
func TestServiceIsCurrent(t *testing.T) {
	base := func(t *testing.T) servicePaths {
		t.Helper()
		dir := t.TempDir()
		p := servicePaths{
			unitFile:   filepath.Join(dir, "unit.plist"),
			binary:     filepath.Join(dir, "cortex"),
			configFile: filepath.Join(dir, "config.yaml"),
			home:       dir,
		}
		body := renderUnitFor(runtime.GOOS, p)
		if err := os.WriteFile(p.unitFile, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("no unit at all is not current", func(t *testing.T) {
		p := base(t)
		if err := os.Remove(p.unitFile); err != nil {
			t.Fatal(err)
		}
		if serviceIsCurrent(runtime.GOOS, p) {
			t.Error("claimed current with no unit installed")
		}
	})

	t.Run("a unit from a different agentop is not current", func(t *testing.T) {
		p := base(t)
		body, err := os.ReadFile(p.unitFile)
		if err != nil {
			t.Fatal(err)
		}
		stale := strings.Replace(string(body), version, "v0.0.1-ancient", 1)
		if err := os.WriteFile(p.unitFile, []byte(stale), 0o600); err != nil {
			t.Fatal(err)
		}
		if serviceIsCurrent(runtime.GOOS, p) {
			t.Error("claimed current for a unit another agentop wrote; it pins that agentop's binary")
		}
	})

	t.Run("a unit naming a different binary is not current", func(t *testing.T) {
		p := base(t)
		p.binary = filepath.Join(t.TempDir(), "some-other-proxy")
		if serviceIsCurrent(runtime.GOOS, p) {
			t.Error("claimed current while the unit names a different binary")
		}
	})

	t.Run("a unit naming a different config is not current", func(t *testing.T) {
		p := base(t)
		p.configFile = filepath.Join(t.TempDir(), "other.yaml")
		if serviceIsCurrent(runtime.GOOS, p) {
			t.Error("claimed current while the unit names a different config")
		}
	})

	t.Run("no health URL means we cannot confirm it is serving", func(t *testing.T) {
		p := base(t)
		p.healthURL = ""
		if serviceIsCurrent(runtime.GOOS, p) {
			t.Error("claimed current without being able to confirm it serves")
		}
	})

	t.Run("darwin requires --supervise, or crashes go unrecovered", func(t *testing.T) {
		if runtime.GOOS != "darwin" {
			t.Skip("darwin only")
		}
		p := base(t)
		body, err := os.ReadFile(p.unitFile)
		if err != nil {
			t.Fatal(err)
		}
		no := strings.Replace(string(body), "<string>--supervise</string>", "", 1)
		if err := os.WriteFile(p.unitFile, []byte(no), 0o600); err != nil {
			t.Fatal(err)
		}
		if serviceIsCurrent(runtime.GOOS, p) {
			t.Error("claimed current for a unit that lost --supervise")
		}
	})
}

// The session-history line must not appear on a first install, where there is no store to
// clear. It printed unconditionally from the Makefile before, on a clean machine too. Where the
// session archive runs, a restart clears memory and not the history, and the line says that.
func TestReportHistoryCleared(t *testing.T) {
	t.Run("silent when nothing was running", func(t *testing.T) {
		for _, archived := range []bool{false, true} {
			var out strings.Builder
			reportHistoryCleared(false, archived, &out)
			if out.Len() != 0 {
				t.Errorf("archived=%v: claimed something about history on a first install: %q", archived, out.String())
			}
		}
	})
	t.Run("names the cleared store when no archive runs", func(t *testing.T) {
		var out strings.Builder
		reportHistoryCleared(true, false, &out)
		if !strings.Contains(out.String(), "session history is cleared") {
			t.Errorf("did not name the cleared store: %q", out.String())
		}
	})
	t.Run("says the archive keeps it when one runs", func(t *testing.T) {
		var out strings.Builder
		reportHistoryCleared(true, true, &out)
		if s := out.String(); strings.Contains(s, "history is cleared") || !strings.Contains(s, "session archive is not") {
			t.Errorf("did not say the archive keeps the history: %q", s)
		}
	})
}

// proxyArchives asks the running proxy, on the session API its config names: only a list
// carrying an archive object counts. A proxy without one, or predating ?archived=true, answers
// the plain list, and a missing or failing API is no archive.
func TestProxyArchives(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		code int
		want bool
	}{
		{"an archive object", `{"sessions":[],"archive":{"bytes":42,"maxBytes":100,"retentionDays":30}}`, 200, true},
		{"the plain list", `{"sessions":[{"id":"live"}]}`, 200, false},
		{"an explicit null", `{"sessions":[],"archive":null}`, 200, false},
		{"not the session API", ``, 200, false},
		{"an error status", `{"archive":{}}`, 500, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/sessions" || r.URL.Query().Get("archived") != "true" {
					t.Errorf("probed %s, want /v1/sessions?archived=true", r.URL)
				}
				w.WriteHeader(tc.code)
				w.Write([]byte(tc.body))
			}))
			defer api.Close()
			cfg := filepath.Join(t.TempDir(), "config.yaml")
			body := "mode: proxy-sidecar\nlistener:\n  session_api_addr: " + strings.TrimPrefix(api.URL, "http://") + "\n"
			if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := proxyArchives(servicePaths{configFile: cfg}); got != tc.want {
				t.Errorf("proxyArchives = %v, want %v", got, tc.want)
			}
		})
	}
	t.Run("nothing listening", func(t *testing.T) {
		cfg := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(cfg, []byte("mode: proxy-sidecar\nlistener:\n  session_api_addr: 127.0.0.1:1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if proxyArchives(servicePaths{configFile: cfg}) {
			t.Error("an unreachable session API counted as an archive")
		}
	})
}
