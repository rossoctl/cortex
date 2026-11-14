package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/cmd/agentop/updatestatus"
	"github.com/rossoctl/cortex/core/config"
)

var autoNow = time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC)

// fakeRelease is what the fake installer saw.
type fakeRelease struct {
	ran   bool
	ref   string
	flags []string
}

// autoScene fixes updateNow, serves releaseAssetURL with a Last-Modified of
// released (a 404 when released is zero), and puts a fake in place of the
// installer that returns fail.
func autoScene(t *testing.T, released time.Time, fail error) *fakeRelease {
	t.Helper()
	savedNow, savedURL, savedFn := updateNow, releaseAssetURL, installReleaseFn
	t.Cleanup(func() { updateNow, releaseAssetURL, installReleaseFn = savedNow, savedURL, savedFn })
	updateNow = func() time.Time { return autoNow }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if released.IsZero() || !strings.HasSuffix(r.URL.Path, "/checksums.txt") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Last-Modified", released.UTC().Format(http.TimeFormat))
	}))
	t.Cleanup(srv.Close)
	releaseAssetURL = srv.URL + "/%s/checksums.txt"
	fake := &fakeRelease{}
	installReleaseFn = func(_ *setupEnv, ref string, flags []string, _, _ io.Writer) error {
		fake.ran, fake.ref, fake.flags = true, ref, flags
		return fail
	}
	return fake
}

func readStatus(t *testing.T, sc doctorScene) updatestatus.Status {
	t.Helper()
	s, err := updatestatus.Read(filepath.Join(sc.home, ".cortex", updatestatus.FileName))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The scene installs v9.9.9 of both binaries.
type autoRow struct {
	name, latest    string
	agentop, cortex string        // --version scripts; "" keeps the scene's
	config          string        // appended to config.yaml
	age             time.Duration // of the release; 0 serves a 404
	fail            error
	want            string
	ran             bool
}

var autoUpdateRows = []autoRow{
	{name: "disabled", latest: "v9.9.10", config: "update:\n  auto: false\n", age: 25 * time.Hour, want: updatestatus.ResultDisabled},
	// An opt-out holds when another field fails validation.
	{name: "disabled, config invalid", latest: "v9.9.10", config: "update:\n  auto: false\ncost_ledger:\n  retention_days: 1\n", age: 25 * time.Hour, want: updatestatus.ResultDisabled},
	// config.Load expands ${VAR}, so the opt-out does too.
	{name: "disabled by env", latest: "v9.9.10", config: "update:\n  auto: ${AGENTOP_TEST_UPDATE_AUTO}\n", age: 25 * time.Hour, want: updatestatus.ResultDisabled},
	{name: "latest not a release", latest: "v9.9.10-rc.1", age: 25 * time.Hour, want: updatestatus.ResultCheckFailed},
	{name: "not-a-release", latest: "v9.9.10", agentop: "#!/bin/sh\necho agentop dev\n", age: 25 * time.Hour, want: updatestatus.ResultNotARelease},
	{name: "current", latest: "v9.9.9", age: 25 * time.Hour, want: updatestatus.ResultCurrent},
	{name: "waiting", latest: "v9.9.10", age: 23 * time.Hour, want: updatestatus.ResultWaiting},
	{name: "wait set", latest: "v9.9.10", config: "update:\n  wait: 72h\n", age: 71 * time.Hour, want: updatestatus.ResultWaiting},
	{name: "wait 0", latest: "v9.9.10", config: "update:\n  wait: \"0\"\n", age: time.Minute, want: updatestatus.ResultUpdated, ran: true},
	// A bad wait keeps the default and does not cancel auto: false.
	{name: "wait invalid", latest: "v9.9.10", config: "update:\n  wait: soon\n", age: 23 * time.Hour, want: updatestatus.ResultWaiting},
	// A wrong-type field does not cancel auto: false either.
	{name: "wait list, disabled", latest: "v9.9.10", config: "update:\n  auto: false\n  wait: [24h]\n", age: 25 * time.Hour, want: updatestatus.ResultDisabled},
	{name: "check_every map, disabled", latest: "v9.9.10", config: "update:\n  auto: false\n  check_every: {h: 6}\n", age: 25 * time.Hour, want: updatestatus.ResultDisabled},
	// A wrong-type auto is not an opt-out.
	{name: "auto string", latest: "v9.9.10", config: "update:\n  auto: \"false\"\n", age: 25 * time.Hour, want: updatestatus.ResultUpdated, ran: true},
	{name: "auto number", latest: "v9.9.10", config: "update:\n  auto: 0\n", age: 25 * time.Hour, want: updatestatus.ResultUpdated, ran: true},
	{name: "auto list", latest: "v9.9.10", config: "update:\n  auto: [false]\n  wait: [1h]\n", age: 25 * time.Hour, want: updatestatus.ResultUpdated, ran: true},
	{name: "wait invalid, disabled", latest: "v9.9.10", config: "update:\n  auto: false\n  wait: soon\n", age: 25 * time.Hour, want: updatestatus.ResultDisabled},
	{name: "check-failed latest", latest: "", age: 25 * time.Hour, want: updatestatus.ResultCheckFailed},
	{name: "check-failed time", latest: "v9.9.10", want: updatestatus.ResultCheckFailed},
	{name: "updated", latest: "v9.9.10", age: 25 * time.Hour, want: updatestatus.ResultUpdated, ran: true},
	{name: "failed", latest: "v9.9.10", age: 25 * time.Hour, fail: errors.New("installer broke"), want: updatestatus.ResultFailed, ran: true},
	// A cpex cortex reports the tag too, and is not current.
	{name: "cpex current", latest: "v9.9.9", cortex: "#!/bin/sh\necho cortex (cpex) v9.9.9\n", age: 25 * time.Hour, want: updatestatus.ResultUpdated, ran: true},
	// A half-done swap is repaired, not reported as current.
	{name: "mismatch", latest: "v9.9.9", cortex: "#!/bin/sh\necho cortex v9.9.8\n", age: 25 * time.Hour, want: updatestatus.ResultUpdated, ran: true},
	// The kill switch: Latest moved back.
	{name: "downgrade", latest: "v9.9.8", age: 25 * time.Hour, want: updatestatus.ResultUpdated, ran: true},
}

func runAutoRow(t *testing.T, i int) (int, doctorScene, *fakeRelease) {
	t.Helper()
	c := autoUpdateRows[i]
	t.Setenv("AGENTOP_TEST_UPDATE_AUTO", "false")
	sc := newDoctorScene(t)
	sc.installed(t)
	bin := filepath.Join(sc.home, ".local", "bin")
	if c.agentop != "" {
		writeExe(t, filepath.Join(bin, "agentop"), c.agentop)
	}
	if c.cortex != "" {
		writeExe(t, filepath.Join(bin, "cortex"), c.cortex)
	}
	if c.config != "" {
		f, err := os.OpenFile(filepath.Join(sc.home, ".cortex", "config.yaml"), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.WriteString(c.config)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	latestIs(t, c.latest)
	var released time.Time
	if c.age != 0 {
		released = autoNow.Add(-c.age)
	}
	fake := autoScene(t, released, c.fail)
	var out, errb bytes.Buffer
	code := runUpdate([]string{"--auto"}, &out, &errb)
	if code != 0 {
		t.Logf("stdout %q stderr %q", out.String(), errb.String())
	}
	return code, sc, fake
}

func TestAutoUpdateDecisions(t *testing.T) {
	for i, c := range autoUpdateRows {
		t.Run(c.name, func(t *testing.T) {
			_, sc, fake := runAutoRow(t, i)
			s := readStatus(t, sc)
			if s.Result != c.want || fake.ran != c.ran {
				t.Fatalf("result %q ran %v, want %q %v (error %q)", s.Result, fake.ran, c.want, c.ran, s.Error)
			}
			if !s.CheckedAt.Equal(autoNow) || s.Latest != c.latest {
				t.Errorf("checkedAt %v latest %q, want %v %q", s.CheckedAt, s.Latest, autoNow, c.latest)
			}
			if c.agentop == "" && s.Installed != "v9.9.9" {
				t.Errorf("installed %q, want v9.9.9", s.Installed)
			}
			switch c.name {
			case "waiting":
				if want := autoNow.Add(-c.age).Add(config.DefaultUpdateWait); !s.InstallAfter.Equal(want) {
					t.Errorf("installAfter %v, want %v", s.InstallAfter, want)
				}
			case "check-failed latest":
				if s.Error != "could not read the newest release" {
					t.Errorf("error %q", s.Error)
				}
			case "check-failed time":
				if s.Error == "" {
					t.Error("no error text")
				}
			case "updated":
				if fake.ref != c.latest || !slices.Contains(fake.flags, "--ref="+c.latest) || !slices.Contains(fake.flags, "--yes") {
					t.Errorf("installer ref %q flags %q", fake.ref, fake.flags)
				}
			case "failed":
				if !strings.Contains(s.Error, "installer broke") {
					t.Errorf("error %q lacks the installer's", s.Error)
				}
			}
		})
	}
}

func TestAutoUpdateExitsZero(t *testing.T) {
	for i, c := range autoUpdateRows {
		t.Run(c.name, func(t *testing.T) {
			if code, _, _ := runAutoRow(t, i); code != 0 {
				t.Errorf("exit %d, want 0", code)
			}
		})
	}
}

func TestUpdateLock(t *testing.T) {
	sc := newDoctorScene(t)
	sc.installed(t)
	latestIs(t, "v9.9.10")
	fake := autoScene(t, autoNow.Add(-73*time.Hour), nil)
	unlock, held, err := lockUpdate(filepath.Join(sc.home, ".cortex"))
	if err != nil || held {
		t.Fatalf("lock: held %v err %v", held, err)
	}
	defer unlock()
	var out, errb bytes.Buffer
	if code := runUpdate([]string{"--auto"}, &out, &errb); code != 0 {
		t.Errorf("--auto exit %d, want 0", code)
	}
	if _, err := os.Stat(filepath.Join(sc.home, ".cortex", updatestatus.FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("--auto under a held lock wrote the status file (%v)", err)
	}
	errb.Reset()
	if code := runUpdate(nil, &out, &errb); code != 1 || !strings.Contains(errb.String(), "another update is running") {
		t.Errorf("manual exit %d stderr %q, want 1 and the held message", code, errb.String())
	}
	if fake.ran {
		t.Error("the installer ran under a held lock")
	}
}

func TestUpdateAutoRejectsRef(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runUpdate([]string{"--auto", "--ref=main"}, &out, &errb); code != 2 {
		t.Errorf("exit %d, want 2: %s", code, errb.String())
	}
}

func TestManualUpdateWritesStatus(t *testing.T) {
	t.Run("current", func(t *testing.T) {
		sc := newDoctorScene(t)
		sc.installed(t)
		latestIs(t, "v9.9.9")
		autoScene(t, time.Time{}, nil)
		var out, errb bytes.Buffer
		if code := runUpdate(nil, &out, &errb); code != 0 || !strings.HasPrefix(out.String(), "Already at") {
			t.Fatalf("exit %d stdout %q", code, out.String())
		}
		if s := readStatus(t, sc); s.Result != updatestatus.ResultCurrent || s.Latest != "v9.9.9" || s.Installed != "v9.9.9" {
			t.Errorf("status %+v", s)
		}
	})
	t.Run("failed", func(t *testing.T) {
		sc := newDoctorScene(t)
		sc.installed(t)
		latestIs(t, "v9.9.10")
		autoScene(t, time.Time{}, errors.New("installer broke"))
		var out, errb bytes.Buffer
		if code := runUpdate([]string{"--yes"}, &out, &errb); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		if s := readStatus(t, sc); s.Result != updatestatus.ResultFailed || !strings.Contains(s.Error, "installer broke") {
			t.Errorf("status %+v", s)
		}
	})
}

func TestUpdateStatusFileMode(t *testing.T) {
	_, sc, _ := runAutoRow(t, slices.IndexFunc(autoUpdateRows, func(r autoRow) bool { return r.name == "current" }))
	fi, err := os.Stat(filepath.Join(sc.home, ".cortex", updatestatus.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
}
