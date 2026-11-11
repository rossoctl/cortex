package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestWaitBootedOut_RealLaunchd drives real launchctl against a throwaway label,
// reproducing the failure a real upgrade hit: `launchctl bootout` returns while
// teardown is still in progress, and bootstrapping into that window fails with
// "Bootstrap failed: 5: Input/output error". Our own teardown is not instant — the
// supervisor forwards SIGTERM and waits for the proxy to exit, and a previous release's
// waited out a 15s drain — so the window is wide enough to lose. A trivial job dies fast
// enough to hide it, which is why every test starting from nothing, or running uninstall
// first, passed.
// TestBootoutWaitIsWiredIn pins the CALL SITE, not just the helper.
//
// Verified by mutation: deleting the waitBootedOut call from loadService left this
// entire suite green — the same shape of gap that let the EIO bug ship. Reading the
// source is the idiom already used by TestStopIsDurable, and unlike the launchd test
// below it runs on Linux CI.
func TestBootoutWaitIsWiredIn(t *testing.T) {
	src, err := os.ReadFile("cmd_service_platform.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	w := strings.Index(body, "waitBootedOutf(target, serviceBootoutTimeout")
	if w < 0 {
		t.Fatal("loadService no longer waits for bootout to complete; " +
			"an upgrade over a running service will fail with launchctl EIO")
	}
	b := strings.Index(body, `"launchctl", "bootstrap"`)
	if b < 0 {
		t.Fatal("no bootstrap call found")
	}
	if w > b {
		t.Error("the bootout wait comes AFTER bootstrap; it has to precede it")
	}
	// And the bootout whose completion we wait for must still be issued before it.
	o := strings.Index(body, `"launchctl", "bootout", target`)
	if o < 0 || o > w {
		t.Error("bootout is not issued before the wait")
	}
}

func TestWaitBootedOut_RealLaunchd(t *testing.T) {
	// Four skip paths meant this could report success having executed no assertion —
	// on the very machine a release is built from. AGENTOP_LAUNCHD_TESTS=required turns
	// every skip into a failure, so a release check can prove the race was exercised
	// rather than hope it was. This bug shipped because a path was never exercised; the
	// guard against it should not be silently skippable. requireRealLaunchd covers the
	// first two (not macOS; no launchctl or no desktop session); the other two are below.
	requireRealLaunchd(t)
	skip := launchdSkip(t)
	uid := strconv.Itoa(os.Getuid())
	label := "io.rossoctl.cortex.test.bootout"
	target := "gui/" + uid + "/" + label
	dir := t.TempDir()

	// A job that is deliberately slow to die, like the supervisor.
	script := filepath.Join(dir, "slow.sh")
	if err := os.WriteFile(script,
		[]byte("#!/bin/sh\ntrap 'sleep 4; exit 0' TERM\nwhile :; do sleep 1; done\n"), 0o700); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	plist := filepath.Join(dir, label+".plist")
	body := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>` + label + `</string>
  <key>ProgramArguments</key><array><string>` + script + `</string></array>
  <key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
</dict></plist>`
	if err := os.WriteFile(plist, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("launchctl", "bootout", target).Run() //nolint:errcheck
		_ = exec.Command("pkill", "-f", script).Run()          //nolint:errcheck
	})

	_ = exec.Command("launchctl", "bootout", target).Run() //nolint:errcheck
	waitBootedOut(target, 10*time.Second)
	if out, err := exec.Command("launchctl", "bootstrap", "gui/"+uid, plist).CombinedOutput(); err != nil {
		skip("cannot bootstrap a test agent here: %v: %s", err, out)
		return
	}
	_ = exec.Command("launchctl", "kickstart", "-p", target).Run() //nolint:errcheck

	// The race only exists while a job is actually RUNNING and slow to die. Without
	// this guard the test passed in 0.07s against a job launchd had never started —
	// green, and proving nothing. launchd refuses to start agents added mid-session in
	// some domains (see supervise.go), so skip loudly rather than pass vacuously.
	running := false
	for i := 0; i < 20; i++ {
		out, _ := exec.Command("launchctl", "print", target).CombinedOutput()
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, "state = running") {
				running = true
			}
		}
		if running {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !running {
		skip("launchd would not start the test agent in this domain; cannot exercise the race")
		return
	}

	// Tear it down and confirm waitBootedOut does not return until the label is gone.
	_ = exec.Command("launchctl", "bootout", target).Run() //nolint:errcheck
	if !waitBootedOut(target, 20*time.Second) {
		t.Fatal("waitBootedOut timed out; teardown never completed")
	}
	// Deliberately NOT asserting how long it waited. That assertion was here and was
	// flaky: launchd sometimes tears the job down in milliseconds, so "must take at
	// least a second" failed on a correct implementation. The contract that matters is
	// the post-condition below — the label is gone, and the bootstrap that used to hit
	// EIO now succeeds.
	// The label must really be absent now, which is what makes the next bootstrap safe.
	if err := exec.Command("launchctl", "print", target).Run(); err == nil {
		t.Error("waitBootedOut returned true while the label is still in the domain")
	}
	// And the bootstrap that previously failed with EIO must now succeed.
	out, err := exec.Command("launchctl", "bootstrap", "gui/"+uid, plist).CombinedOutput()
	if err != nil {
		t.Errorf("bootstrap after waitBootedOut still failed: %v: %s", err, out)
	}
	if strings.Contains(string(out), "Input/output error") {
		t.Errorf("still hitting the EIO race: %s", out)
	}
}

// TestWaitGone_Progress covers the progress line deterministically.
//
// Up to 30s of silence right after "Setting up the launchd user agent..." is
// indistinguishable from a hang, and it lands on exactly the people who just hit the
// EIO failure. Driving waitGone with a fake predicate tests that without needing to
// hold a real launchd label half-torn-down, which is not something a test can arrange —
// the first version of this could only skip.
func TestWaitGone_Progress(t *testing.T) {
	t.Run("a fast teardown stays silent", func(t *testing.T) {
		var out bytes.Buffer
		if !waitGone(2*time.Second, &out, func() bool { return true }) {
			t.Fatal("reported not-gone for an immediately-gone label")
		}
		if out.Len() != 0 {
			t.Errorf("the common case should print nothing, got: %q", out.String())
		}
	})

	t.Run("a slow teardown announces, then confirms", func(t *testing.T) {
		var out bytes.Buffer
		start := time.Now()
		ok := waitGone(10*time.Second, &out, func() bool { return time.Since(start) > 1500*time.Millisecond })
		if !ok {
			t.Fatal("gave up on a label that did go away")
		}
		got := out.String()
		if !strings.Contains(got, "Waiting for the previous Cortex to stop") {
			t.Errorf("no progress line during a slow teardown: %q", got)
		}
		if !strings.Contains(got, "stopped.") {
			t.Errorf("announced the wait but never confirmed the end: %q", got)
		}
	})

	t.Run("a label that never goes away times out", func(t *testing.T) {
		var out bytes.Buffer
		if waitGone(300*time.Millisecond, &out, func() bool { return false }) {
			t.Error("claimed a still-present label was gone")
		}
	})

	t.Run("a nil writer is safe", func(t *testing.T) {
		if !waitGone(2*time.Second, nil, func() bool { return true }) {
			t.Error("nil progress writer broke the wait")
		}
	})
}
