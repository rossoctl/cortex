package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestReportJobNotAnswering is TestReportUnitNotAnswering for macOS: each state
// launchd can report for Cortex's job, and what status says about it.
func TestReportJobNotAnswering(t *testing.T) {
	for _, tc := range []struct {
		name     string
		state    launchdState
		code     int
		want     []string
		rejected []string
	}{
		{"stopped by service stop",
			launchdState{known: true, session: true, loaded: false, disabled: true}, 0,
			[]string{"Cortex is stopped, and stays stopped across logins and reboots", "agentop service start"},
			[]string{"NOT answering", "NOT running", "Last log lines", "not disabled"}},
		{"booted out by hand, not disabled",
			launchdState{known: true, session: true, loaded: false, disabled: false}, 1,
			[]string{"stopped, but not disabled, so it starts again at the next login", "agentop service start"},
			[]string{"stays stopped", "NOT answering"}},
		// The state the login-time on-demand-only mode leaves, and the gap between a
		// crash and launchd's restart.
		{"loaded, not running",
			launchdState{known: true, session: true, loaded: true, state: "not running"}, 1,
			[]string{"launchd has its job loaded but not running (state = not running)", "agentop service restart", "Last log lines"},
			[]string{"stopped", "NOT answering"}},
		{"running but silent",
			launchdState{known: true, session: true, loaded: true, state: "running"}, 1,
			[]string{"Cortex is NOT answering"}, []string{"stopped", "loaded but not running"}},
		// This user not at the screen: what status over SSH to an unattended Mac sees,
		// or to one where someone else is logged in. It must not promise a start at
		// the next login, which a `service stop` would have cancelled.
		{"no desktop session",
			launchdState{known: true}, 1,
			[]string{"you are not logged in to this Mac's desktop", "unless `agentop service stop` stopped it", "An SSH login does not start it"},
			[]string{"Cortex is stopped", "NOT answering", "nobody"}},
		{"launchd could not be asked", launchdState{}, 1,
			[]string{"Cortex is NOT answering"}, []string{"stopped", "logged in"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := servicePathsFixture(t)
			var out bytes.Buffer
			if code := reportJobNotAnswering(p, tc.state, &out); code != tc.code {
				t.Errorf("exit = %d, want %d", code, tc.code)
			}
			for _, w := range tc.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("output lacks %q:\n%s", w, out.String())
				}
			}
			for _, r := range tc.rejected {
				if strings.Contains(out.String(), r) {
					t.Errorf("output should not contain %q:\n%s", r, out.String())
				}
			}
		})
	}
}

// A job launchd has loaded but is not running can be losing the bind to another
// process, and the holder is named before the consequence, as on Linux (#1122).
func TestReportJobNotAnsweringNamesTheHolder(t *testing.T) {
	const holder = "127.0.0.1:47600 is held by /usr/bin/python3 (pid 31337), not this Cortex"
	p := servicePathsFixture(t)
	p.forwardAddr = "127.0.0.1:47600"
	holderOnPort(t, "47600", 31337, "/usr/bin/python3")
	var out bytes.Buffer
	reportJobNotAnswering(p, launchdState{known: true, session: true, loaded: true, state: "not running"}, &out)
	s := out.String()
	at, after := strings.Index(s, holder), strings.Index(s, "Claude Code and OpenCode will fail meanwhile")
	if at < 0 || after < 0 || at > after {
		t.Errorf("want the holder named before the consequence:\n%s", s)
	}

	// A stopped Cortex is not failing to bind, so nothing is said about the port.
	out.Reset()
	reportJobNotAnswering(p, launchdState{known: true, session: true, disabled: true}, &out)
	if strings.Contains(out.String(), "is held by") {
		t.Errorf("a stopped Cortex names a port holder:\n%s", out.String())
	}
}

// status asks launchd only once the probe has failed, and only on macOS.
func TestServiceStatusAsksLaunchdOnlyWhenSilent(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("status reads launchd only on macOS")
	}
	asked := 0
	saved := jobStateOf
	jobStateOf = func() launchdState {
		asked++
		return launchdState{known: true, session: true, disabled: true}
	}
	t.Cleanup(func() { jobStateOf = saved })

	p := servicePathsFixture(t)
	if err := os.WriteFile(p.unitFile, []byte("unit"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := serviceStatus(p, &out); code != 0 || !strings.Contains(out.String(), "Cortex is stopped, and stays stopped") {
		t.Errorf("status = %d:\n%s\nwant 0 and the stopped line", code, out.String())
	}
	if asked != 1 {
		t.Errorf("asked launchd %d times, want 1", asked)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(srv.Close)
	p.healthURL = srv.URL + "/healthz"
	asked = 0
	out.Reset()
	if code := serviceStatus(p, &out); code != 0 || asked != 0 {
		t.Errorf("healthy: exit %d, asked launchd %d times; want 0 and 0", code, asked)
	}
}

// launchctlTearingDown stubs a launchctl whose bootout succeeds while the label stays
// in the domain for the first `lingers` prints, as a real bootout leaves it while the
// supervisor takes the proxy down. It returns the file that counts the prints.
func launchctlTearingDown(t *testing.T, lingers int) string {
	t.Helper()
	count := filepath.Join(t.TempDir(), "prints")
	fakeLaunchctl(t, `#!/bin/sh
case "$1" in
  print)
    n=$(cat '`+count+`' 2>/dev/null || echo 0); n=$((n + 1)); echo $n > '`+count+`'
    if [ $n -le `+strconv.Itoa(lingers)+` ]; then echo 'state = running'; exit 0; fi
    echo 'Could not find service "io.rossoctl.cortex" in domain for user gui: 501' >&2; exit 113 ;;
esac
exit 0
`)
	return count
}

func prints(t *testing.T, count string) int {
	t.Helper()
	b, _ := os.ReadFile(count)
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

// stop and unload return only once launchd has the label out of the domain. A
// `launchctl bootout` returns before teardown finishes, so stop used to say
// "stopped" while the supervisor was still taking the proxy down and holding its
// ports. Passing "darwin" makes this run on any host: only launchctl is stubbed.
// TestServiceUninstallSaysStoppedOnlyOnceItIs: `service uninstall` says "Cortex is
// stopped" and exits 0 once launchd has taken the job down. When the teardown outlasts
// the wait, it still removes the plist, but says Cortex is still shutting down and
// exits 1: the stopped claim was what the wait exists to make true.
func TestServiceUninstallSaysStoppedOnlyOnceItIs(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the wait after a bootout is launchd's; systemd's disable --now waits itself")
	}
	for _, tc := range []struct {
		name     string
		lingers  int
		code     int
		want     string
		mustNot  string
		wantWarn bool
		wait     time.Duration
	}{
		{"the teardown finishes", 2, 0, "Removed. Cortex is stopped", "still shutting down", false, time.Hour},
		{"the teardown outlasts the wait", 1000, 1, "Removed, but Cortex was still shutting down", "Cortex is stopped", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved := serviceBootoutTimeout
			serviceBootoutTimeout = tc.wait
			t.Cleanup(func() { serviceBootoutTimeout = saved })
			launchctlTearingDown(t, tc.lingers)
			p := servicePathsFixture(t)
			if err := os.WriteFile(p.unitFile, []byte("plist"), 0o600); err != nil {
				t.Fatal(err)
			}
			var out, errb bytes.Buffer
			if code := serviceUninstall(p, true, &out, &errb); code != tc.code {
				t.Errorf("exit = %d, want %d", code, tc.code)
			}
			if !strings.Contains(out.String(), tc.want) || strings.Contains(out.String(), tc.mustNot) {
				t.Errorf("stdout =\n%s\nwant %q and not %q", out.String(), tc.want, tc.mustNot)
			}
			if got := strings.Contains(errb.String(), "still shutting down"); got != tc.wantWarn {
				t.Errorf("stderr =\n%s\nwant the still-shutting-down error: %v", errb.String(), tc.wantWarn)
			}
			if _, err := os.Stat(p.unitFile); !os.IsNotExist(err) {
				t.Errorf("plist still at %s, want it removed either way", p.unitFile)
			}
		})
	}
}

func TestStopAndUnloadWaitForTheBootout(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(p servicePaths) error
	}{
		{"stop", func(p servicePaths) error { return controlService("darwin", "stop", p, nil) }},
		{"unload", func(p servicePaths) error { return unloadService("darwin", p) }},
	} {
		t.Run(tc.name+" waits out a slow teardown", func(t *testing.T) {
			count := launchctlTearingDown(t, 3)
			if err := tc.run(servicePathsFixture(t)); err != nil {
				t.Fatalf("err = %v, want nil once the label is gone", err)
			}
			if n := prints(t, count); n < 4 {
				t.Errorf("returned after %d prints, want at least 4: the label was still there for 3", n)
			}
		})
		t.Run(tc.name+" says so when the teardown does not finish", func(t *testing.T) {
			saved := serviceBootoutTimeout
			serviceBootoutTimeout = 0
			t.Cleanup(func() { serviceBootoutTimeout = saved })
			launchctlTearingDown(t, 1000)
			err := tc.run(servicePathsFixture(t))
			if err == nil || !strings.Contains(err.Error(), "still shutting down") {
				t.Errorf("err = %v, want it to say the job was still shutting down", err)
			}
		})
	}
}
