package main

import (
	"bytes"
	"encoding/xml"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestRenderUpdaterUnitDarwin(t *testing.T) {
	p := servicePathsFixture(t)
	got := renderUpdaterUnitFor("darwin", p)
	agentop := filepath.Join(filepath.Dir(p.binary), "agentop")
	for _, want := range []string{
		"<string>" + agentop + "</string>",
		"<string>update</string>", "<string>--yes</string>", "<string>--auto</string>",
		"<string>" + updaterLabel + "</string>",
		"<key>AgentopVersion</key><string>" + version + "</string>",
		filepath.Join(filepath.Dir(p.logFile), "update.log"),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plist lacks %q:\n%s", want, got)
		}
	}
	// The proxy starts the updater; launchd must never start it on its own.
	for _, bad := range []string{"RunAtLoad", "KeepAlive", "StartInterval"} {
		if strings.Contains(got, bad) {
			t.Errorf("plist has %s:\n%s", bad, got)
		}
	}
	var v any
	if err := xml.Unmarshal([]byte(got), &v); err != nil {
		t.Errorf("plist is not XML: %v\n%s", err, got)
	}
}

func TestRenderUpdaterUnitLinux(t *testing.T) {
	p := servicePathsFixture(t)
	got := renderUpdaterUnitFor("linux", p)
	agentop := filepath.Join(filepath.Dir(p.binary), "agentop")
	log := filepath.Join(filepath.Dir(p.logFile), "update.log")
	for _, want := range []string{
		"Type=oneshot\n",
		"ExecStart=" + shQuote(agentop) + " update --yes --auto\n",
		"StandardOutput=append:" + log + "\n",
		"StandardError=append:" + log + "\n",
		"X-AgentopVersion=" + version + "\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("unit lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "[Install]") {
		t.Errorf("unit has an [Install] section, so it could be enabled:\n%s", got)
	}
}

func TestUpdaterUnitCurrent(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		p := servicePathsFixture(t)
		if updaterUnitCurrent(goos, p) {
			t.Errorf("%s: current with no file", goos)
		}
		if err := os.WriteFile(p.updaterFile, []byte(renderUpdaterUnitFor(goos, p)), 0o600); err != nil {
			t.Fatal(err)
		}
		if !updaterUnitCurrent(goos, p) {
			t.Errorf("%s: not current with the rendered file", goos)
		}
		if err := os.WriteFile(p.updaterFile, []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
		if updaterUnitCurrent(goos, p) {
			t.Errorf("%s: current with a stale file", goos)
		}
	}
}

// fakeSupervisorWithUpdater is fakeSupervisor plus a launchctl that logs each call and
// keeps the updater label's load state apart from the proxy's. It returns the call log
// and the updater's state file, which exists while the updater is loaded.
func fakeSupervisorWithUpdater(t *testing.T) (calls, updaterLoaded string) {
	t.Helper()
	loaded := fakeSupervisor(t)
	updaterLoaded = filepath.Join(t.TempDir(), "updater-loaded")
	calls, logLine := callLog(t)
	installStub(t, "launchctl", `#!/bin/sh
`+logLine+`
case "$1" in
  print)
    case "$2" in
      */`+updaterLabel+`) if [ -f '`+updaterLoaded+`' ]; then exit 0; fi
             echo 'Could not find service' >&2; exit 113 ;;
      */*/*) if [ -f '`+loaded+`' ]; then echo 'state = running'; exit 0; fi
             echo 'Could not find service' >&2; exit 113 ;;
    esac
    exit 0 ;;
  bootstrap)
    case "$3" in
      *`+updaterLabel+`.plist) : > '`+updaterLoaded+`' ;;
      *) : > '`+loaded+`' ;;
    esac ;;
  bootout)
    case "$2" in
      */`+updaterLabel+`) if [ -f '`+updaterLoaded+`' ]; then rm -f '`+updaterLoaded+`'; exit 0; fi
             echo 'Boot-out failed: 3: No such process' >&2; exit 3 ;;
    esac
    if [ -f '`+loaded+`' ]; then rm -f '`+loaded+`'; exit 0; fi
    echo 'Boot-out failed: 3: No such process' >&2; exit 3 ;;
  enable) rm -f '`+loaded+`.disabled' ;;
  disable) : > '`+loaded+`.disabled' ;;
esac
exit 0
`)
	return calls, updaterLoaded
}

func updaterTarget() string { return "gui/" + strconv.Itoa(os.Getuid()) + "/" + updaterLabel }

func TestServiceInstallWritesUpdaterUnit(t *testing.T) {
	calls, _ := fakeSupervisorWithUpdater(t)
	sc := newServiceScene(t)
	var out, errb bytes.Buffer
	if r := runServiceInstall(sc.p, 0, false, false, &out, &errb); r.exit != 0 {
		t.Fatalf("install = %+v\n%s%s", r, out.String(), errb.String())
	}
	for _, f := range []string{sc.p.unitFile, sc.p.updaterFile} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s not written: %v", f, err)
		}
	}
	if !updaterUnitCurrent(runtime.GOOS, sc.p) {
		t.Error("the updater file is not what renderUpdaterUnitFor writes")
	}
	if runtime.GOOS == "darwin" {
		want := "bootstrap gui/" + strconv.Itoa(os.Getuid()) + " " + sc.p.updaterFile
		if log := readCallLog(t, calls); !slices.Contains(log, want) {
			t.Errorf("no %q in the launchctl calls:\n%s", want, strings.Join(log, "\n"))
		}
	}
}

// A failed load removes the unit, so uninstall then has nothing to do. The updater file
// must go with it, or nothing removes it.
func TestServiceInstallFailedLoadRemovesUpdaterUnit(t *testing.T) {
	fakeSupervisor(t)
	sc := newServiceScene(t)
	// Only the proxy's load fails, so install gets past its preflight and writes both files.
	installStub(t, "launchctl", `#!/bin/sh
case "$1" in
  print) case "$2" in */*/*) echo 'Could not find service' >&2; exit 113 ;; esac; exit 0 ;;
  bootstrap) echo 'Bootstrap failed: 5: Input/output error' >&2; exit 5 ;;
esac
exit 0
`)
	installStub(t, "systemctl", `#!/bin/sh
case "$*" in
  *restart*|*start*) echo 'Job failed' >&2; exit 1 ;;
  *is-active*) echo inactive; exit 3 ;;
esac
exit 0
`)
	var out, errb bytes.Buffer
	if r := runServiceInstall(sc.p, 0, false, false, &out, &errb); r.exit == 0 {
		t.Fatalf("install with a failing load = %+v, want a failure\n%s%s", r, out.String(), errb.String())
	}
	if strings.Contains(errb.String(), "cannot manage") {
		t.Fatalf("install stopped at its preflight, before it wrote the files:\n%s", errb.String())
	}
	for _, f := range []string{sc.p.unitFile, sc.p.updaterFile} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s left after a failed load: %v\n%s", f, err, errb.String())
		}
	}
}

// The updater job runs `agentop update --auto`, which runs this install. A bootout of
// the updater would kill the run before it finishes.
func TestServiceInstallNeverBootsOutTheUpdater(t *testing.T) {
	calls, updaterLoaded := fakeSupervisorWithUpdater(t)
	if err := os.WriteFile(updaterLoaded, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sc := newServiceScene(t)
	var out, errb bytes.Buffer
	for _, restart := range []bool{false, true} {
		if r := runServiceInstall(sc.p, 0, restart, false, &out, &errb); r.exit != 0 {
			t.Fatalf("install (restart=%v) = %+v\n%s%s", restart, r, out.String(), errb.String())
		}
	}
	for _, c := range readCallLog(t, calls) {
		if strings.HasPrefix(c, "bootout") && strings.Contains(c, updaterLabel) {
			t.Errorf("install booted out the updater: %q", c)
		}
		if strings.HasPrefix(c, "bootstrap") && strings.Contains(c, sc.p.updaterFile) {
			t.Errorf("install bootstrapped an updater that was loaded: %q", c)
		}
	}
	if _, err := os.Stat(updaterLoaded); err != nil {
		t.Error("the updater is no longer loaded")
	}
}

func TestServiceUninstallRemovesUpdaterUnit(t *testing.T) {
	calls, _ := fakeSupervisorWithUpdater(t)
	sc := newServiceScene(t)
	var out, errb bytes.Buffer
	if r := runServiceInstall(sc.p, 0, false, false, &out, &errb); r.exit != 0 {
		t.Fatalf("install = %+v\n%s%s", r, out.String(), errb.String())
	}
	if !removeService(sc.p, &errb) {
		t.Fatalf("removeService failed: %s", errb.String())
	}
	for _, f := range []string{sc.p.unitFile, sc.p.updaterFile} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s still on disk (stat: %v)", f, err)
		}
	}
	if runtime.GOOS == "darwin" {
		want := "bootout " + updaterTarget()
		if log := readCallLog(t, calls); !slices.Contains(log, want) {
			t.Errorf("no %q in the launchctl calls:\n%s", want, strings.Join(log, "\n"))
		}
	}
}

// An install that predates the updater must get it at the next install, not
// "Already current".
func TestServiceIsCurrentFalseWithoutUpdaterUnit(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			p := currentServiceFixture(t, goos)
			if !serviceIsCurrent(goos, p) {
				t.Fatal("fixture is not current")
			}
			if err := os.Remove(p.updaterFile); err != nil {
				t.Fatal(err)
			}
			if serviceIsCurrent(goos, p) {
				t.Error("current with no updater unit")
			}
		})
	}

	t.Run("install writes it again", func(t *testing.T) {
		fakeSupervisorWithUpdater(t)
		sc := newServiceScene(t)
		var out, errb bytes.Buffer
		if r := runServiceInstall(sc.p, 0, false, false, &out, &errb); r.exit != 0 {
			t.Fatalf("install = %+v\n%s%s", r, out.String(), errb.String())
		}
		if r := runServiceInstall(sc.p, 0, false, false, &out, &errb); !r.alreadyCurrent {
			t.Fatalf("second install = %+v, want already current", r)
		}
		if err := os.Remove(sc.p.updaterFile); err != nil {
			t.Fatal(err)
		}
		out.Reset()
		r := runServiceInstall(sc.p, 0, false, false, &out, &errb)
		if r.exit != 0 || r.alreadyCurrent || strings.Contains(out.String(), "Already current") {
			t.Errorf("install with no updater = %+v, want a real install\n%s", r, out.String())
		}
		if _, err := os.Stat(sc.p.updaterFile); err != nil {
			t.Errorf("updater not written again: %v", err)
		}
	})
}
