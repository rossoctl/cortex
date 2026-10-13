package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// This file holds the cross-platform tests for the two functions #1106 finished
// parameterizing: serviceIsCurrent and reportInstallSuccess. Everything here passes
// "darwin" and "linux" explicitly rather than runtime.GOOS, so BOTH platforms' logic
// runs on whatever host the suite runs on — the same contract the header of
// cmd_service_systemd_test.go states for the four functions #1080 refactored.
//
// It is a separate file from cmd_service_systemd_test.go because these are not
// systemd tests: each case here runs twice, once per platform, and the point is the
// pairing rather than the Linux side.

// supervisorSaysRunning stubs whichever supervisor CLI goos's branch of
// supervisorRunning shells out to, answering "our job is up".
//
// One helper for both, because every serviceIsCurrent case below needs to get PAST
// the supervisor clause to reach the one it is actually about, and the two platforms
// need different binaries stubbed with different vocabularies to say the same thing:
// launchctl prints an indented "state = running" block, systemctl is-active prints
// the bare word and exits 0. Spelling that out at each call site would put the
// platform dispatch in the test bodies, which is exactly what this refactor removed
// from the code under test.
func supervisorSaysRunning(t *testing.T, goos string) {
	t.Helper()
	if goos == "darwin" {
		fakeLaunchctl(t, "#!/bin/sh\necho '\tstate = running'\nexit 0\n")
		return
	}
	fakeSystemctl(t, "#!/bin/sh\necho 'active'\nexit 0\n")
}

// currentServiceFixture builds the state serviceIsCurrent is supposed to accept for
// goos: goos's own unit text on disk, a stamp matching the binary, a supervisor
// reporting the job up, and a health endpoint that answers 200.
//
// Note renderUnitFor(goos, …), not renderUnit(): the unit must be the one goos's
// branch reads, or the darwin --supervise clause would be checked against a systemd
// unit that never contains the flag (and so would fail for the wrong reason).
func currentServiceFixture(t *testing.T, goos string) servicePaths {
	t.Helper()
	p := servicePathsFixture(t)
	if err := os.WriteFile(p.unitFile, []byte(renderUnitFor(goos, p)), 0o600); err != nil {
		t.Fatal(err)
	}
	writeProxyStamp(p)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	p.healthURL = srv.URL + "/healthz"
	supervisorSaysRunning(t, goos)
	return p
}

// TestServiceIsCurrent_BothPlatforms exercises serviceIsCurrent's platform-dependent
// clauses from either host. Before #1106 it read runtime.GOOS internally, so the
// darwin --supervise clause could only be reached on a Mac and the supervisor clause
// could only ever be driven through one platform's stub — the existing
// TestServiceIsCurrent (cmd_service_restricted_test.go) skips its --supervise case
// off darwin for exactly that reason.
func TestServiceIsCurrent_BothPlatforms(t *testing.T) {
	// The baseline each case below perturbs. Asserted first, per platform, because a
	// fixture that was not current to begin with would make every "not current"
	// expectation below pass for free.
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos+": a freshly written, running, healthy service is current", func(t *testing.T) {
			p := currentServiceFixture(t, goos)
			if !serviceIsCurrent(goos, p) {
				t.Error("not current, though the unit, stamp, supervisor and health all agree")
			}
		})
	}

	// The clause that was unreachable off darwin, now asserted in both directions on
	// every host: a macOS unit that lost --supervise leaves crashes unrecovered, which
	// is the whole point of the flag, while the systemd unit never carries it at all —
	// Restart=on-failure is its recovery, so requiring it there would make every Linux
	// install reinstall itself forever.
	t.Run("darwin: a unit that lost --supervise is not current", func(t *testing.T) {
		p := currentServiceFixture(t, "darwin")
		body, err := os.ReadFile(p.unitFile)
		if err != nil {
			t.Fatal(err)
		}
		stripped := strings.Replace(string(body), "<string>--supervise</string>", "", 1)
		if !strings.Contains(string(body), "<string>--supervise</string>") {
			t.Fatal("fixture's darwin unit has no --supervise to remove")
		}
		if err := os.WriteFile(p.unitFile, []byte(stripped), 0o600); err != nil {
			t.Fatal(err)
		}
		if serviceIsCurrent("darwin", p) {
			t.Error("claimed current for a plist that would leave crashes unrecovered")
		}
	})

	t.Run("linux: the systemd unit is current without --supervise", func(t *testing.T) {
		p := currentServiceFixture(t, "linux")
		if strings.Contains(renderUnitFor("linux", p), "--supervise") {
			t.Fatal("the systemd unit now carries --supervise; this case no longer says anything")
		}
		if !serviceIsCurrent("linux", p) {
			t.Error("not current — the darwin --supervise requirement leaked onto Linux")
		}
	})

	// The supervisor clause, driven through each platform's own stub. "Installed but
	// not running" is the state a bad config leaves, and reporting it as current means
	// an install that skips the repair.
	for _, tc := range []struct {
		goos string
		// body is the stub for goos's supervisor CLI, answering "our job is NOT up".
		body string
	}{
		// launchctl print exits nonzero when the label is not in the domain.
		{"darwin", "#!/bin/sh\necho 'Could not find service' >&2\nexit 113\n"},
		// systemctl is-active prints the real state and exits nonzero for anything but active.
		{"linux", "#!/bin/sh\necho 'failed'\nexit 3\n"},
	} {
		t.Run(tc.goos+": a job the supervisor does not report running is not current", func(t *testing.T) {
			// supervisorRunning's darwin branch POLLS for serviceReadyTimeout before it
			// will call a job not-running — transient post-kickstart states must not be
			// read as a verdict. Nothing here is transient, so shorten the wait rather
			// than spend the real 15s on a stub that answers the same way every time.
			saved := serviceReadyTimeout
			serviceReadyTimeout = 200 * time.Millisecond
			t.Cleanup(func() { serviceReadyTimeout = saved })

			p := currentServiceFixture(t, tc.goos)
			// Prepended after the fixture's own stub, so this one wins — installStub
			// puts its dir at the front of PATH each time.
			if tc.goos == "darwin" {
				fakeLaunchctl(t, tc.body)
			} else {
				fakeSystemctl(t, tc.body)
			}
			if serviceIsCurrent(tc.goos, p) {
				t.Error("claimed current for the installed-but-dead state")
			}
		})
	}
}

// TestReportInstallSuccess_BothPlatforms pins what each platform's install prints,
// from either host.
//
// TestReportInstallSuccess (cmd_service_recovery_test.go) already covers both
// healthy outcomes, but only for the host it runs on: its crash-recovery assertion
// is behind `runtime.GOOS == "darwin"` and its "no supervisor mentioned" assertion
// behind the negation, so each run verifies one platform and takes the other on
// faith. Here all four combinations run every time.
func TestReportInstallSuccess_BothPlatforms(t *testing.T) {
	for _, tc := range []struct {
		goos    string
		healthy bool
		// wantName is the supervisor the line must name. Spelled out rather than
		// taken from supervisorName(goos), so a rename there cannot quietly rewrite
		// what this test expects to see.
		wantName string
		// wantNote is whether the crash-recovery note belongs on this platform. It is
		// darwin-only because launchd does not restart these agents (renderUnitFor);
		// systemd's Restart=on-failure does, so there is no second process to explain.
		wantNote bool
	}{
		{"darwin", true, "launchd user agent", true},
		{"darwin", false, "launchd user agent", true},
		{"linux", true, "systemd user unit", false},
		{"linux", false, "systemd user unit", false},
	} {
		name := tc.goos + "/healthy=" + map[bool]string{true: "true", false: "false"}[tc.healthy]
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			reportInstallSuccess(tc.goos, tc.healthy, &out)
			got := out.String()

			if !strings.Contains(got, "Running as a "+tc.wantName) {
				t.Errorf("does not name the %s:\n%s", tc.wantName, got)
			}
			// healthy is the only thing distinguishing the two lines, and it is the
			// difference between "we watched it serve" and "we did not look".
			if tc.healthy && !strings.Contains(got, "healthy") {
				t.Errorf("healthy=true does not say so:\n%s", got)
			}
			if !tc.healthy && strings.Contains(got, "healthy") {
				t.Errorf("healthy=false claims healthy:\n%s", got)
			}
			// The note on BOTH darwin outcomes is the regression the single-function
			// collapse exists to prevent; its absence on linux is what the parameter
			// now lets a Mac assert.
			if tc.wantNote && !strings.Contains(got, crashRecoveryNote) {
				t.Errorf("missing the crash-recovery note:\n%s", got)
			}
			if !tc.wantNote && strings.Contains(got, crashRecoveryNote) {
				t.Errorf("prints a note about launchd's supervisor on a systemd host:\n%s", got)
			}
		})
	}
}
