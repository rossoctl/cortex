package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/cmd/agentop/updatestatus"
)

var updatesNow = time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC)

// updatesScene is a service install with doctorNow at updatesNow and st in
// ~/.cortex/update.json.
func updatesScene(t *testing.T, st updatestatus.Status) doctorScene {
	t.Helper()
	sc := newDoctorScene(t)
	sc.installed(t)
	saved := doctorNow
	doctorNow = func() time.Time { return updatesNow }
	t.Cleanup(func() { doctorNow = saved })
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sc.home, ".cortex", updatestatus.FileName), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return sc
}

func updatesTime(t time.Time) string { return t.Local().Format("2006-01-02 15:04") }

func TestDoctorUpdatesOK(t *testing.T) {
	at := updatesNow.Add(-time.Hour)
	sc := updatesScene(t, updatestatus.Status{CheckedAt: at, Latest: "v9.9.9", Installed: "v9.9.9", Result: updatestatus.ResultCurrent})
	code, out := sc.doctor(t)
	wantLines(t, code, 0, out, "  · updates      auto on · current at "+updatesTime(at)+"\n")
}

func TestDoctorUpdatesWaiting(t *testing.T) {
	at, after := updatesNow.Add(-time.Hour), updatesNow.Add(time.Hour)
	sc := updatesScene(t, updatestatus.Status{CheckedAt: at, Latest: "v9.9.10", Installed: "v9.9.9",
		Result: updatestatus.ResultWaiting, InstallAfter: after})
	code, out := sc.doctor(t)
	wantLines(t, code, 0, out, "  · updates      auto on · waiting at "+updatesTime(at)+" · installs v9.9.10 after "+updatesTime(after)+"\n")
}

func TestDoctorUpdatesFailed(t *testing.T) {
	at := updatesNow.Add(-time.Hour)
	sc := updatesScene(t, updatestatus.Status{CheckedAt: at, Latest: "v9.9.10", Installed: "v9.9.9",
		Result: updatestatus.ResultFailed, Error: "exit status 1\ninstall.sh: download failed"})
	code, out := sc.doctor(t)
	wantLines(t, code, 1, out, "\n"+markLine("✗", "updates")+"update to v9.9.10 failed at "+updatesTime(at)+
		"\n      exit status 1\n      install.sh: download failed\n      see ~/.cortex/update.log\n")
}

func TestDoctorUpdatesCheckFailed(t *testing.T) {
	at := updatesNow.Add(-time.Hour)
	sc := updatesScene(t, updatestatus.Status{CheckedAt: at, Installed: "v9.9.9",
		Result: updatestatus.ResultCheckFailed, Error: "could not read the newest release\nmore"})
	code, out := sc.doctor(t)
	wantLines(t, code, 0, out, "\n"+markLine("!", "updates")+"the last check failed at "+updatesTime(at)+
		": could not read the newest release\n      fix: see ~/.cortex/update.log\n")
}

func TestDoctorUpdatesStale(t *testing.T) {
	at := updatesNow.Add(-100 * time.Hour)
	sc := updatesScene(t, updatestatus.Status{CheckedAt: at, Latest: "v9.9.9", Installed: "v9.9.9", Result: updatestatus.ResultCurrent})
	code, out := sc.doctor(t)
	wantLines(t, code, 0, out, "\n"+markLine("!", "updates")+"no update check since "+updatesTime(at)+
		"\n      fix: see ~/.cortex/update.log\n")
}

func TestDoctorUpdatesNoCheckYet(t *testing.T) {
	sc := newDoctorScene(t)
	sc.installed(t)
	code, out := sc.doctor(t)
	wantLines(t, code, 0, out, "\n"+markLine("!", "updates")+"no update check yet\n      fix: see ~/.cortex/update.log\n")
}

func TestDoctorUpdatesDisabled(t *testing.T) {
	sc := updatesScene(t, updatestatus.Status{CheckedAt: updatesNow.Add(-100 * time.Hour), Result: updatestatus.ResultFailed})
	cfg := filepath.Join(sc.home, ".cortex", "config.yaml")
	b, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, append(b, "\nupdate:\n  auto: false\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := sc.doctor(t)
	wantLines(t, code, 0, out, "  · updates      automatic updates off (update.auto: false)\n")
}

func TestDoctorUpdatesNoService(t *testing.T) {
	sc := newDoctorScene(t)
	var pid int
	stopProcessOnCleanup(t, &pid)
	sc.installed(t, "--no-service")
	pid = readPIDFile(filepath.Join(sc.home, ".cortex", "proxy.pid"))
	_, out := sc.doctor(t)
	if want := "  · updates      not automatic without the service; agentop update installs a release\n"; !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
}
