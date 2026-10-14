package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeSystemctl puts a systemctl on PATH that behaves however body says, mirroring
// fakeLaunchctl (cmd_service_restricted_test.go) for the Linux side of the same
// functions. loadService, controlService, supervisorRunning and unloadService all
// shell out to the real systemctl; until now nothing exercised their reaction to
// systemd's actual vocabulary (active/activating/failed/deactivating, or a
// daemon-reload/enable failure) — only the generated unit TEXT was tested.
//
// All tests in this file pass "linux" explicitly to the functions under test,
// rather than relying on runtime.GOOS, so they run for real on any host —
// including the one these were written on.
func fakeSystemctl(t *testing.T, body string) {
	t.Helper()
	installStub(t, "systemctl", body)
}

// fakeLoginctl puts a loginctl on PATH that behaves however body says. Its own
// temp dir, separate from fakeSystemctl's: a test using both ends up with two
// prepended PATH entries, one per binary, which is fine — each dir holds only its
// own stub, so there's nothing for the two to collide over.
func fakeLoginctl(t *testing.T, body string) {
	t.Helper()
	installStub(t, "loginctl", body)
}

// installStub writes body as an executable named "name" into its own temp dir and
// prepends that dir to PATH, then immediately confirms name actually resolves to
// it. That confirmation matters on its own, not just as a sanity check: a stub
// that silently isn't reachable (PATH not applied yet in some odd ordering, or —
// caught by mutating this file's own permission bits to prove it — written
// non-executable) makes the code under test's own exec.LookPath/exec.Command fail
// before ever touching the stub, so nothing gets appended to any call log. A test
// that only checks "the call log has zero entries" cannot tell that apart from a
// real, correct zero-calls outcome — both read as an empty log. Failing loudly
// here, once, closes that for every subtest that uses these helpers, rather than
// leaving each assertion to rediscover it independently.
func installStub(t *testing.T, name, body string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if got, err := exec.LookPath(name); err != nil || got != path {
		t.Fatalf("stub not reachable: LookPath(%q) = %q, %v; want %q", name, got, err, path)
	}
}

// noSystemctlOnPath points PATH at an empty directory, so exec.LookPath("systemctl")
// fails the same way it would on a box with no systemd at all. Call this instead
// of, never after, fakeSystemctl/fakeLoginctl/installStub in the same subtest: it
// replaces PATH rather than prepending to it, so it would silently shadow out any
// stub already installed, the same invisible failure installStub's own
// reachability check exists to catch.
func noSystemctlOnPath(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

// callLogPrefix marks a logged invocation, so that a call with NO arguments is
// still a line with content rather than a blank one.
//
// `echo "$@"` alone made a zero-argument call write an empty line, and readCallLog
// drops empty lines — the same reason installStub's reachability check exists, one
// layer in: a real but bugged zero-argument invocation read as "never called", which
// is precisely the outcome every assertion here uses to mean the opposite. No current
// call site passes zero arguments, so nothing in this package exercises it; the point
// is that it stays distinguishable if one ever does, rather than silently merging
// into a passing assertion. See #1106.
const callLogPrefix = "argv:"

// callLog returns a path inside dir and a shell snippet that appends the fake's
// own arguments to it — so a test can assert exactly what our code invoked, not
// just that it received a canned answer back.
func callLog(t *testing.T) (path string, appendLine string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "calls.log")
	// shQuote's own doc comment describes it as quoting a systemd ExecStart argument
	// specifically, but the escaping rule is plain POSIX sh, the same for any word in
	// any shell command — including this redirect target, not a unit file at all.
	//
	// The prefix and "$@" are separate echo words rather than one "argv: $@" string,
	// so a zero-argument call logs exactly "argv:" with no trailing space for
	// readCallLog to have to trim away.
	return path, `echo ` + callLogPrefix + ` "$@" >> ` + shQuote(path)
}

// readCallLog returns one entry per invocation, each the arguments that invocation
// was given — "" for a call that was given none.
//
// Every line carries callLogPrefix, so "" here can only come from a zero-argument
// call and never from the file's own trailing newline. A line WITHOUT the prefix is
// a hard failure rather than a skip: it means something other than a callLog fake
// wrote to this path, and silently filtering it would hide whatever that was.
func readCallLog(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec
	if errors.Is(err, os.ErrNotExist) {
		return nil // genuinely never called
	}
	if err != nil {
		t.Fatalf("reading the call log: %v", err)
	}
	trimmed := strings.TrimRight(string(b), "\n")
	if trimmed == "" {
		return nil // the file exists but holds nothing: still never called
	}
	var lines []string
	for _, l := range strings.Split(trimmed, "\n") {
		args, ok := strings.CutPrefix(l, callLogPrefix)
		if !ok {
			t.Fatalf("call log line %q does not start with %q — something other than a "+
				"callLog fake wrote to %s", l, callLogPrefix, path)
		}
		// One space separates the prefix from the first argument; a zero-argument
		// call has neither, so TrimPrefix rather than a fixed slice.
		lines = append(lines, strings.TrimPrefix(args, " "))
	}
	return lines
}

// TestCallLog is a test of the test harness, and it earns its place the way
// installStub's reachability check does: every assertion in this file reads a zero-entry
// call log as "our code never invoked this", so the harness must not be able to produce
// that reading from anything else. `echo "$@"` could — a call with no arguments wrote a
// blank line, which readCallLog dropped — so a bugged `systemctl` with its arguments
// lost would have come back as the clean "never called" outcome several subtests here
// assert deliberately (#1106).
//
// Driven through a stub invoked directly rather than through any function under test,
// because no production call site passes zero arguments today. That is the latency: the
// harness has to stay honest before some future caller makes it matter, and there is
// nothing in the code that would exercise it in the meantime.
func TestCallLog(t *testing.T) {
	// invoke runs the callLog-writing stub with args and returns what readCallLog
	// makes of the result.
	invoke := func(t *testing.T, args ...string) []string {
		t.Helper()
		logPath, logLine := callLog(t)
		fakeSystemctl(t, "#!/bin/sh\n"+logLine+"\nexit 0\n")
		if err := exec.Command("systemctl", args...).Run(); err != nil {
			t.Fatalf("stub exited non-zero: %v", err)
		}
		return readCallLog(t, logPath)
	}

	cases := []struct {
		name string
		args []string
		want []string
	}{
		// The case the harness could not previously express. One entry, not zero: the
		// call HAPPENED, it just carried nothing.
		{"a zero-argument call is one entry with empty arguments", nil, []string{""}},
		// And the ordinary case still round-trips verbatim, prefix stripped — which is
		// what every other assertion in this file compares against.
		{"a normal call logs its arguments verbatim", []string{"--user", "daemon-reload"}, []string{"--user daemon-reload"}},
		// A lone empty argument is INDISTINGUISHABLE from no arguments at all, in the
		// text AND in the entry count — both decode to exactly one "". `echo argv:`
		// writes "argv:" and `echo argv: ""` writes "argv: ", and CutPrefix plus
		// TrimPrefix(" ") collapse the two. Recorded as a known limit of the format,
		// beside the space-joining one below, rather than as a distinction to look for.
		//
		// WHAT THE ENTRY COUNT DOES DISTINGUISH is called-at-all (one entry) from
		// never-called (nil) — the zero-argument case above and the final subtest below
		// are the two halves of that, and it is the distinction every assertion in this
		// file keys on. Telling one-empty-arg from zero-args would need a per-argument
		// format, which nothing here needs: no agentop call site passes either.
		{"one empty argument is still one entry", []string{""}, []string{""}},
		// Arguments are joined by the shell, so an argument containing a space is
		// indistinguishable from two arguments in the log. Recorded as a known limit
		// rather than a bug: nothing agentop invokes passes an argument with a space,
		// and a per-argument format would make every existing assertion unreadable.
		{"an argument containing a space reads as two", []string{"a b"}, []string{"a b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := invoke(t, tc.args...)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("readCallLog = %#v, want %#v", got, tc.want)
			}
			if len(got) != len(tc.want) {
				t.Errorf("entry count = %d, want %d", len(got), len(tc.want))
			}
		})
	}

	// The other half of the distinction: a stub that never ran at all. Asserted in the
	// same test as the zero-argument case on purpose — the two used to be the same
	// value, and what makes the fix meaningful is that they now differ.
	t.Run("a stub that was never invoked reads as nil", func(t *testing.T) {
		logPath, logLine := callLog(t)
		fakeSystemctl(t, "#!/bin/sh\n"+logLine+"\nexit 0\n")
		if got := readCallLog(t, logPath); got != nil {
			t.Errorf("readCallLog = %#v, want nil for a fake that was never called", got)
		}
	})
}

func TestSupervisorRunning_Linux(t *testing.T) {
	p := servicePathsFixture(t)

	t.Run("active reads as running", func(t *testing.T) {
		fakeSystemctl(t, "#!/bin/sh\necho 'active'\nexit 0\n")
		running, why := supervisorRunning("linux", p)
		if !running || why != "is-active = active" {
			t.Errorf("running=%v why=%q, want true/%q", running, why, "is-active = active")
		}
	})

	// systemctl is-active exits non-zero for every state except "active", but still
	// prints the real state on stdout. Only the exit-0 case may read as running.
	// The fakes below all use exit 3, but the specific nonzero value is arbitrary:
	// supervisorRunning's Linux branch only checks err != nil and stdout content, never
	// a particular exit code, so any nonzero value here exercises the same path.
	for _, state := range []string{"activating", "failed", "deactivating", "inactive"} {
		t.Run(state+" does not read as running", func(t *testing.T) {
			fakeSystemctl(t, "#!/bin/sh\necho '"+state+"'\nexit 3\n")
			running, why := supervisorRunning("linux", p)
			if running {
				t.Errorf("state=%s read as running", state)
			}
			if why != "is-active = "+state {
				t.Errorf("why = %q, want %q", why, "is-active = "+state)
			}
		})
	}

	t.Run("no systemctl on PATH does not block", func(t *testing.T) {
		noSystemctlOnPath(t)
		running, why := supervisorRunning("linux", p)
		if !running || why != "" {
			t.Errorf("running=%v why=%q, want true/\"\" — a box with no systemd must not "+
				"be reported as not-running", running, why)
		}
	})

	t.Run("systemctl present but silent on failure", func(t *testing.T) {
		fakeSystemctl(t, "#!/bin/sh\nexit 1\n")
		running, why := supervisorRunning("linux", p)
		if running || why != "systemctl is-active gave no answer" {
			t.Errorf("running=%v why=%q, want false/%q", running, why, "systemctl is-active gave no answer")
		}
	})
}

func TestLoadService_Linux(t *testing.T) {
	// The daemon-reload/enable-now failure subtests below only prove loadService
	// reports the right failure when systemctl fails — they say nothing about what
	// gets invoked, in what order, on a run that succeeds. This closes that: the
	// calls must happen in order, with --user and the exact unit name, not just "some
	// two calls that happened to both exit 0".
	t.Run("daemon-reload, enable, then restart, in that order, with the exact unit", func(t *testing.T) {
		p := servicePathsFixture(t)
		logPath, logLine := callLog(t)
		fakeSystemctl(t, "#!/bin/sh\n"+logLine+"\nexit 0\n")
		fakeLoginctl(t, "#!/bin/sh\necho 'Linger=yes'\nexit 0\n") // skip the linger branch; not under test here
		if err := loadService("linux", p, io.Discard); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		calls := readCallLog(t, logPath)
		want := []string{"--user daemon-reload", "--user enable cortex.service", "--user restart cortex.service"}
		if strings.Join(calls, "|") != strings.Join(want, "|") {
			t.Errorf("systemctl calls = %v, want %v in that order", calls, want)
		}
	})

	t.Run("no systemctl on PATH", func(t *testing.T) {
		p := servicePathsFixture(t)
		noSystemctlOnPath(t)
		err := loadService("linux", p, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "systemctl not found") {
			t.Errorf("err = %v, want it to say systemctl was not found", err)
		}
	})

	t.Run("daemon-reload failure is reported, enable is never attempted", func(t *testing.T) {
		p := servicePathsFixture(t)
		logPath, logLine := callLog(t)
		fakeSystemctl(t, `#!/bin/sh
`+logLine+`
case "$*" in
  *daemon-reload*)
    echo "boom: unit has a syntax error" >&2
    exit 1 ;;
esac
echo "enable --now should not have run" >&2
exit 1
`)
		err := loadService("linux", p, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "daemon-reload") || !strings.Contains(err.Error(), "boom") {
			t.Errorf("err = %v, want it to name daemon-reload and the underlying reason", err)
		}
		// The error-message assertion above already implies this (an enable --now
		// attempt would hit the fallthrough and produce a different message), but
		// the call log makes "enable is never attempted" a literal check rather
		// than something a reader has to re-derive from the error format.
		calls := readCallLog(t, logPath)
		if len(calls) != 1 || calls[0] != "--user daemon-reload" {
			t.Errorf("systemctl calls = %v, want exactly [--user daemon-reload]", calls)
		}
	})

	t.Run("enable failure is reported", func(t *testing.T) {
		p := servicePathsFixture(t)
		fakeSystemctl(t, `#!/bin/sh
case "$*" in
  *daemon-reload*) exit 0 ;;
esac
echo "nope: unit not found" >&2
exit 1
`)
		err := loadService("linux", p, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "enable") || !strings.Contains(err.Error(), "nope") {
			t.Errorf("err = %v, want it to name enable --now and the underlying reason", err)
		}
	})

	// enable alone leaves a unit that is already running on the binary it started with,
	// so an upgrade kept serving the old one: a v0.7.0 install upgraded under systemd
	// was still running the deleted authbridge-proxy afterwards.
	t.Run("restart failure is reported", func(t *testing.T) {
		p := servicePathsFixture(t)
		fakeSystemctl(t, `#!/bin/sh
case "$*" in
  *restart*)
    echo "nope: start limit hit" >&2
    exit 1 ;;
esac
exit 0
`)
		fakeLoginctl(t, "#!/bin/sh\necho 'Linger=yes'\nexit 0\n")
		err := loadService("linux", p, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "restart") || !strings.Contains(err.Error(), "start limit") {
			t.Errorf("err = %v, want it to name restart and the underlying reason", err)
		}
	})

	t.Run("linger already enabled: skipped, no marker written", func(t *testing.T) {
		p := servicePathsFixture(t)
		fakeSystemctl(t, "#!/bin/sh\nexit 0\n")
		logPath, logLine := callLog(t)
		fakeLoginctl(t, `#!/bin/sh
`+logLine+`
case "$*" in
  *show-user*)
    echo "Linger=yes"
    exit 0 ;;
esac
exit 1
`)
		if err := loadService("linux", p, io.Discard); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, err := os.Stat(lingerMarker(p)); err == nil {
			t.Error("marker written even though linger was already on")
		}
		// Asserting the marker's absence proves loadService took the skip branch, but
		// not that it got there by actually calling show-user — a fake that always
		// skipped enable-linger regardless of input would pass the same way. Checking
		// the log closes that: show-user must have run, and enable-linger must not.
		calls := readCallLog(t, logPath)
		if len(calls) != 1 || !strings.Contains(calls[0], "show-user") {
			t.Errorf("loginctl calls = %v, want exactly one show-user", calls)
		}
	})

	t.Run("linger not enabled, enable-linger succeeds: marker written", func(t *testing.T) {
		p := servicePathsFixture(t)
		fakeSystemctl(t, "#!/bin/sh\nexit 0\n")
		logPath, logLine := callLog(t)
		fakeLoginctl(t, `#!/bin/sh
`+logLine+`
case "$*" in
  *show-user*)
    echo "Linger=no"
    exit 0 ;;
esac
exit 0
`)
		if err := loadService("linux", p, io.Discard); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, err := os.Stat(lingerMarker(p)); err != nil {
			t.Error("no marker written after agentop enabled linger itself")
		}
		// The marker alone doesn't prove the right command ran — a typo'd verb, the
		// wrong uid, or a bare `enable-linger` with no argument would each still exit 0
		// here and still write the marker. Assert the actual second call's shape:
		// enable-linger, with this process's own uid, and nothing else.
		calls := readCallLog(t, logPath)
		wantEnable := "enable-linger " + strconv.Itoa(os.Getuid())
		if len(calls) != 2 || !strings.Contains(calls[0], "show-user") || calls[1] != wantEnable {
			t.Errorf("loginctl calls = %v, want [show-user ..., %q]", calls, wantEnable)
		}
	})

	t.Run("linger not enabled, enable-linger fails: caveat, not fatal, no marker", func(t *testing.T) {
		p := servicePathsFixture(t)
		fakeSystemctl(t, "#!/bin/sh\nexit 0\n")
		fakeLoginctl(t, `#!/bin/sh
case "$*" in
  *show-user*)
    echo "Linger=no"
    exit 0 ;;
esac
exit 1
`)
		err := loadService("linux", p, io.Discard)
		if !errors.Is(err, errLingerUnavailable) {
			t.Errorf("err = %v, want errLingerUnavailable", err)
		}
		if _, serr := os.Stat(lingerMarker(p)); serr == nil {
			t.Error("marker written even though enable-linger failed")
		}
	})
}

func TestUnloadService_Linux(t *testing.T) {
	t.Run("no systemctl on PATH: nil, nothing attempted", func(t *testing.T) {
		p := servicePathsFixture(t)
		noSystemctlOnPath(t)
		if err := unloadService("linux", p); err != nil {
			t.Errorf("err = %v, want nil — nothing could have been loaded", err)
		}
	})

	t.Run("no marker: loginctl is never called", func(t *testing.T) {
		p := servicePathsFixture(t)
		logPath, logLine := callLog(t)
		fakeLoginctl(t, "#!/bin/sh\n"+logLine+"\nexit 0\n")
		fakeSystemctl(t, "#!/bin/sh\nexit 0\n")
		if err := unloadService("linux", p); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if calls := readCallLog(t, logPath); len(calls) != 0 {
			t.Errorf("loginctl called %v times, want 0 — no marker means we never enabled linger", len(calls))
		}
	})

	t.Run("marker present: disable-linger runs and marker is removed", func(t *testing.T) {
		p := servicePathsFixture(t)
		if err := os.WriteFile(lingerMarker(p), []byte("enabled by agentop\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		logPath, logLine := callLog(t)
		fakeLoginctl(t, "#!/bin/sh\n"+logLine+"\nexit 0\n")
		fakeSystemctl(t, "#!/bin/sh\nexit 0\n")
		if err := unloadService("linux", p); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		calls := readCallLog(t, logPath)
		if len(calls) != 1 || !strings.Contains(calls[0], "disable-linger") {
			t.Errorf("loginctl calls = %v, want exactly one disable-linger", calls)
		}
		if _, err := os.Stat(lingerMarker(p)); err == nil {
			t.Error("marker still present after unload")
		}
	})

	t.Run("disable --now failure is reported", func(t *testing.T) {
		p := servicePathsFixture(t)
		fakeSystemctl(t, `#!/bin/sh
echo "unit not loaded" >&2
exit 1
`)
		err := unloadService("linux", p)
		if err == nil ||
			!strings.Contains(err.Error(), "--user disable --now cortex.service") ||
			!strings.Contains(err.Error(), "unit not loaded") {
			t.Errorf("err = %v, want it to name the exact systemctl invocation and the underlying reason", err)
		}
	})
}

func TestLingerEnabled(t *testing.T) {
	uid := strconv.Itoa(os.Getuid())
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"explicit yes", "#!/bin/sh\necho 'Linger=yes'\nexit 0\n", true},
		{"explicit no", "#!/bin/sh\necho 'Linger=no'\nexit 0\n", false},
		// A parse failure reads as "already on" — errs toward leaving the user's
		// setting alone rather than us silently flipping it.
		{"malformed output reads as already on", "#!/bin/sh\necho 'garbage'\nexit 0\n", true},
		{"loginctl itself fails: reads as already on", "#!/bin/sh\nexit 1\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeLoginctl(t, tc.body)
			if got := lingerEnabled(uid); got != tc.want {
				t.Errorf("lingerEnabled() = %v, want %v", got, tc.want)
			}
		})
	}

	// The cases above only check the parsed result — a wrong property flag or a
	// wrong uid would leave all four green, since fakeLoginctl answers the same
	// way regardless of what it was actually asked. This pins the invocation
	// itself, closing that the same way callLog does everywhere else in this file.
	t.Run("invokes show-user with the exact uid and property", func(t *testing.T) {
		logPath, logLine := callLog(t)
		fakeLoginctl(t, "#!/bin/sh\n"+logLine+"\necho 'Linger=yes'\nexit 0\n")
		lingerEnabled(uid)
		calls := readCallLog(t, logPath)
		want := "show-user " + uid + " --property=Linger"
		if len(calls) != 1 || calls[0] != want {
			t.Errorf("loginctl calls = %v, want exactly [%s]", calls, want)
		}
	})
}

func TestControlService_Linux(t *testing.T) {
	p := servicePathsFixture(t)

	t.Run("no systemctl on PATH", func(t *testing.T) {
		noSystemctlOnPath(t)
		err := controlService("linux", "stop", p, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "systemctl not found") {
			t.Errorf("err = %v, want it to say systemctl was not found", err)
		}
	})

	// stop and start are the PERSISTENT forms (disable/enable --now), so a stop
	// survives a reboot the same way launchd's bootout+disable pairing does; only
	// restart stays transient. Nothing previously proved the right verb went with
	// the right action.
	for _, tc := range []struct {
		action string
		want   string
	}{
		{"stop", "--user disable --now cortex.service"},
		{"start", "--user enable --now cortex.service"},
		{"restart", "--user restart cortex.service"},
	} {
		t.Run(tc.action+" invokes the right systemctl verb", func(t *testing.T) {
			logPath, logLine := callLog(t)
			fakeSystemctl(t, "#!/bin/sh\n"+logLine+"\nexit 0\n")
			if err := controlService("linux", tc.action, p, io.Discard); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			calls := readCallLog(t, logPath)
			if len(calls) != 1 || calls[0] != tc.want {
				t.Errorf("systemctl called with %v, want exactly [%q]", calls, tc.want)
			}
		})
	}

	t.Run("underlying failure is reported with the exact command and reason", func(t *testing.T) {
		fakeSystemctl(t, `#!/bin/sh
echo "kaboom" >&2
exit 1
`)
		err := controlService("linux", "stop", p, io.Discard)
		if err == nil ||
			!strings.Contains(err.Error(), "--user disable --now cortex.service") ||
			!strings.Contains(err.Error(), "kaboom") {
			t.Errorf("err = %v, want it to name the exact systemctl invocation and stderr", err)
		}
	})
}

// TestSystemdUsable mirrors TestLaunchdUsable (cmd_service_restricted_test.go) for
// the Linux side: systemdUsable has no internal GOOS guard, so unlike launchdUsable
// it's directly testable from any host.
func TestSystemdUsable(t *testing.T) {
	t.Run("no systemctl on PATH", func(t *testing.T) {
		noSystemctlOnPath(t)
		ok, why := systemdUsable()
		if ok {
			t.Error("reported usable with no systemctl on PATH")
		}
		if !strings.Contains(why, "systemctl") {
			t.Errorf("reason does not name the missing binary: %q", why)
		}
	})
	t.Run("an unreachable user session is not usable, and says why", func(t *testing.T) {
		fakeSystemctl(t, "#!/bin/sh\necho 'Failed to connect to bus: No such file or directory' >&2\nexit 1\n")
		ok, why := systemdUsable()
		if ok {
			t.Error("reported usable with no reachable systemd --user session")
		}
		if !strings.Contains(why, "bus") {
			t.Errorf("reason does not explain anything: %q", why)
		}
	})
	t.Run("a working session is usable", func(t *testing.T) {
		fakeSystemctl(t, "#!/bin/sh\necho 'XDG_RUNTIME_DIR=/run/user/1000'\nexit 0\n")
		if ok, why := systemdUsable(); !ok {
			t.Errorf("reported unusable against a working systemctl: %s", why)
		}
	})
}

// TestServiceManagerUsable_Linux exercises serviceManagerUsable's own dispatch: given
// "linux" it must call systemdUsable, not launchdUsable — the two report through
// entirely different fakes, so a swapped branch would read the wrong one's stub.
func TestServiceManagerUsable_Linux(t *testing.T) {
	t.Run("linux dispatches to systemdUsable", func(t *testing.T) {
		fakeSystemctl(t, "#!/bin/sh\necho 'Failed to connect to bus' >&2\nexit 1\n")
		ok, why := serviceManagerUsable("linux")
		if ok {
			t.Error("reported usable with no reachable systemd --user session")
		}
		if !strings.Contains(why, "bus") {
			t.Errorf("reason = %q, want it to come from systemdUsable, not launchdUsable", why)
		}
	})
}
