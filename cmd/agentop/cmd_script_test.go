package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCortex answers /reload/status as a proxy that reloads every write, and
// /v1/pipeline with a script slot for dir "moca-sandbox" whose counts are what the
// test set, or no such slot when present is false.
type fakeCortex struct {
	srv                *httptest.Server
	present            atomic.Bool
	loaded, notStarted atomic.Int32
	refuse             atomic.Bool
	polls              atomic.Int32
	// reloadedAfter, when set, has /reload/status report its last success as
	// later than any write, as a proxy that has picked a change up does. Unset,
	// last_success is the time of the poll, which is also after any write.
	reloadedAfter atomic.Bool
}

func newFakeCortex(t *testing.T) *fakeCortex {
	t.Helper()
	f := &fakeCortex{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/reload/status":
			// A refusal, as the poll recognises one: reloads_failed rising after the
			// poll's first answer, with last_success left behind the write. Poll 1 is
			// serverTarget's probe and poll 2 the poll's baseline, so it rises at 3.
			n := f.polls.Add(1)
			st := map[string]any{"last_success": time.Now(), "reloads_ok": 1, "reloads_failed": 0}
			if f.reloadedAfter.Load() {
				st["last_success"] = time.Now().Add(time.Hour)
			}
			if f.refuse.Load() {
				st["last_success"] = time.Now().Add(-time.Hour)
				if n >= 3 {
					st["reloads_failed"], st["last_error"] = 1, "refused by the test"
				}
			}
			_ = json.NewEncoder(w).Encode(st)
		case "/v1/pipeline":
			out := []map[string]any{{"name": "inference-parser"}}
			if f.present.Load() {
				out = append(out, map[string]any{"name": "script", "config": map[string]any{"dir": "moca-sandbox", "fail_closed": true},
					"metrics": []map[string]any{{"name": "scripts loaded", "value": f.loaded.Load()}, {"name": "scripts not started", "value": f.notStarted.Load()}}})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"inbound": []any{}, "outbound": out})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCortex) addr() string { return strings.TrimPrefix(f.srv.URL, "http://") }

// scriptEnv is a scratch HOME with a config whose stats and session API are
// statsAddr and apiAddr, and returns the config path and ~/.cortex/plugins.
func scriptEnv(t *testing.T, statsAddr, apiAddr string) (cfg, plugins string) {
	t.Helper()
	path := serverEnvWithSessions(t, statsAddr, apiAddr, "")
	home := filepath.Dir(path)
	return path, filepath.Join(home, ".cortex", "plugins")
}

func runScriptCmd(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = runScript(args, &out, &errb)
	return code, out.String(), errb.String()
}

func listJSON(t *testing.T, cfg string) map[string]scriptStatus {
	t.Helper()
	code, out, errOut := runScriptCmd(t, "list", "--json", "--config", cfg)
	if code != 0 {
		t.Fatalf("list: exit %d: %s", code, errOut)
	}
	var v struct {
		Scripts []scriptStatus `json:"scripts"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("list --json: %v\n%s", err, out)
	}
	m := map[string]scriptStatus{}
	for _, s := range v.Scripts {
		m[s.Name] = s
	}
	return m
}

func TestScript_ListShowsTheCatalogNotInstalled(t *testing.T) {
	cfg, _ := scriptEnv(t, closedAddr(t), closedAddr(t))
	st, ok := listJSON(t, cfg)["moca-sandbox"]
	if !ok || st.State != stateNotInstalled || !st.FailClosed || st.Scripts != 1 || st.Loaded != nil {
		t.Fatalf("moca-sandbox = %+v", st)
	}
}

// list --json is read by other tools (moca), so its field names are a contract.
func TestScript_ListJSONFieldNames(t *testing.T) {
	f := newFakeCortex(t)
	f.present.Store(true)
	f.loaded.Store(1)
	cfg, _ := scriptEnv(t, f.addr(), f.addr())
	_, out, _ := runScriptCmd(t, "list", "--json", "--config", cfg)
	var v map[string][]map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatal(err)
	}
	row := v["scripts"][0]
	for _, k := range []string{"name", "description", "state", "failClosed", "scripts", "loaded", "notStarted"} {
		if _, ok := row[k]; !ok {
			t.Errorf("list --json has no %q: %v", k, row)
		}
	}
}

func TestScript_InstallWithNoProxyWritesFilesAndSlot(t *testing.T) {
	cfg, plugins := scriptEnv(t, closedAddr(t), closedAddr(t))
	code, out, errOut := runScriptCmd(t, "install", "moca-sandbox", "--config", cfg)
	if code != 0 {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	if !strings.Contains(out, "Cortex is not running") {
		t.Errorf("out = %q", out)
	}
	info, err := os.Stat(filepath.Join(plugins, "moca-sandbox", "moca-sandbox.py"))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("script: %v %v", err, info)
	}
	if _, err := os.Stat(filepath.Join(plugins, "moca-sandbox", "entry.yaml")); err == nil {
		t.Error("entry.yaml was copied into the slot")
	}
	text := readConfig(t, cfg)
	if !strings.Contains(text, "      - name: script\n        config:\n          dir: moca-sandbox\n          fail_closed: true\n") {
		t.Errorf("config:\n%s", text)
	}
	if st := listJSON(t, cfg)["moca-sandbox"]; st.State != stateInstalled {
		t.Errorf("state after install = %s", st.State)
	}
	// Again: nothing to do.
	code, out, _ = runScriptCmd(t, "install", "moca-sandbox", "--config", cfg)
	if code != 0 || !strings.Contains(out, "already installed") {
		t.Errorf("second install: exit %d, %q", code, out)
	}
	if got := readConfig(t, cfg); got != text {
		t.Errorf("second install changed the config")
	}
}

func TestScript_InstallChecksTheSlotLoaded(t *testing.T) {
	f := newFakeCortex(t)
	f.present.Store(true)
	f.loaded.Store(1)
	cfg, _ := scriptEnv(t, f.addr(), f.addr())
	code, out, errOut := runScriptCmd(t, "install", "moca-sandbox", "--config", cfg)
	if code != 0 || !strings.Contains(out, "Installed moca-sandbox") {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
}

// A script that does not start: install takes back the slot and the files.
func TestScript_InstallRollsBackAScriptThatDidNotStart(t *testing.T) {
	f := newFakeCortex(t)
	f.present.Store(true)
	f.notStarted.Store(1)
	cfg, plugins := scriptEnv(t, f.addr(), f.addr())
	before := readConfig(t, cfg)
	code, _, errOut := runScriptCmd(t, "install", "moca-sandbox", "--config", cfg)
	if code != 1 || !strings.Contains(errOut, "0 of 1 scripts started") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if got := readConfig(t, cfg); got != before {
		t.Errorf("the slot was left in the config:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(plugins, "moca-sandbox")); !os.IsNotExist(err) {
		t.Errorf("the folder was left: %v", err)
	}
}

func TestScript_InstallRollsBackARefusedReload(t *testing.T) {
	f := newFakeCortex(t)
	f.refuse.Store(true)
	cfg, plugins := scriptEnv(t, f.addr(), f.addr())
	before := readConfig(t, cfg)
	code, _, errOut := runScriptCmd(t, "install", "moca-sandbox", "--config", cfg)
	if code != 1 || !strings.Contains(errOut, "refused by the test") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if got := readConfig(t, cfg); got != before {
		t.Errorf("config not put back")
	}
	if _, err := os.Stat(filepath.Join(plugins, "moca-sandbox")); !os.IsNotExist(err) {
		t.Errorf("the folder was left: %v", err)
	}
}

func TestScript_StatesFromTheFiles(t *testing.T) {
	cfg, plugins := scriptEnv(t, closedAddr(t), closedAddr(t))
	if code, _, e := runScriptCmd(t, "install", "moca-sandbox", "--config", cfg); code != 0 {
		t.Fatal(e)
	}
	script := filepath.Join(plugins, "moca-sandbox", "moca-sandbox.py")
	rec := filepath.Join(plugins, "moca-sandbox", scriptRecord)
	orig, _ := os.ReadFile(script)

	// An earlier release's copy: the file matches its record, the record not this release.
	old := []byte("#!/usr/bin/env python3\n# an earlier release\n")
	os.WriteFile(script, old, 0o700)
	b, _ := json.Marshal(installRecord{Version: "v0.0.1", Files: map[string]string{"moca-sandbox.py": sum(old)}})
	os.WriteFile(rec, b, 0o600)
	if st := listJSON(t, cfg)["moca-sandbox"]; st.State != stateOutdated {
		t.Fatalf("state = %s, want outdated", st.State)
	}
	if code, out, e := runScriptCmd(t, "install", "moca-sandbox", "--config", cfg); code != 0 || !strings.Contains(out, "Updated") {
		t.Fatalf("update: %d %s%s", code, out, e)
	}
	if got, _ := os.ReadFile(script); !bytes.Equal(got, orig) {
		t.Error("update did not write this release's script")
	}

	// Edited by hand: modified, and install refuses it.
	os.WriteFile(script, append(orig, []byte("# my change\n")...), 0o700)
	if st := listJSON(t, cfg)["moca-sandbox"]; st.State != stateModified {
		t.Fatalf("state = %s, want modified", st.State)
	}
	if code, _, e := runScriptCmd(t, "install", "moca-sandbox", "--config", cfg); code != 1 || !strings.Contains(e, "remove moca-sandbox") {
		t.Fatalf("install over an edit: %d %s", code, e)
	}

	// The slot taken out by hand: incomplete.
	os.WriteFile(script, orig, 0o700)
	text := readConfig(t, cfg)
	os.WriteFile(cfg, []byte(strings.Replace(text, "      - name: script\n        config:\n          dir: moca-sandbox\n          fail_closed: true\n", "", 1)), 0o600)
	if st := listJSON(t, cfg)["moca-sandbox"]; st.State != stateIncomplete {
		t.Fatalf("state = %s, want incomplete", st.State)
	}
	if code, _, e := runScriptCmd(t, "install", "moca-sandbox", "--config", cfg); code != 0 {
		t.Fatalf("repair: %s", e)
	}
	if st := listJSON(t, cfg)["moca-sandbox"]; st.State != stateInstalled {
		t.Fatalf("state after repair = %s", st.State)
	}
}

// An edited copy is modified whether or not its slot is there, so install leaves
// the edit alone. Files matching their record with the slot gone stay incomplete.
func TestScript_AnEditWithoutItsSlotIsModified(t *testing.T) {
	cfg, plugins := scriptEnv(t, closedAddr(t), closedAddr(t))
	if code, _, e := runScriptCmd(t, "install", "moca-sandbox", "--config", cfg); code != 0 {
		t.Fatal(e)
	}
	slotless := strings.Replace(readConfig(t, cfg), "      - name: script\n        config:\n          dir: moca-sandbox\n          fail_closed: true\n", "", 1)
	os.WriteFile(cfg, []byte(slotless), 0o600)
	dir := filepath.Join(plugins, "moca-sandbox")
	script := filepath.Join(dir, "moca-sandbox.py")

	// An earlier release's copy, unedited: incomplete.
	old := []byte("#!/usr/bin/env python3\n# an earlier release\n")
	os.WriteFile(script, old, 0o700)
	b, _ := json.Marshal(installRecord{Version: "v0.0.1", Files: map[string]string{"moca-sandbox.py": sum(old)}})
	os.WriteFile(filepath.Join(dir, scriptRecord), b, 0o600)
	if st := listJSON(t, cfg)["moca-sandbox"]; st.State != stateIncomplete {
		t.Fatalf("outdated copy without its slot: state = %s, want incomplete", st.State)
	}

	// The same copy edited: modified, and install refuses it.
	edited := append(append([]byte{}, old...), "# my change\n"...)
	os.WriteFile(script, edited, 0o700)
	if st := listJSON(t, cfg)["moca-sandbox"]; st.State != stateModified {
		t.Fatalf("edited copy without its slot: state = %s, want modified", st.State)
	}
	if code, _, e := runScriptCmd(t, "install", "moca-sandbox", "--config", cfg); code != 1 || !strings.Contains(e, "remove moca-sandbox") {
		t.Fatalf("install over an edit: %d %s", code, e)
	}
	if got, _ := os.ReadFile(script); !bytes.Equal(got, edited) {
		t.Errorf("install replaced the edit:\n%s", got)
	}
	if readConfig(t, cfg) != slotless {
		t.Error("install added the slot although it refused")
	}
}

func TestScript_RemoveTakesBackWhatInstallAdded(t *testing.T) {
	cfg, plugins := scriptEnv(t, closedAddr(t), closedAddr(t))
	before := readConfig(t, cfg)
	runScriptCmd(t, "install", "moca-sandbox", "--config", cfg)
	code, out, errOut := runScriptCmd(t, "remove", "moca-sandbox", "--config", cfg)
	if code != 0 || !strings.Contains(out, "Removed moca-sandbox") {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	if got := readConfig(t, cfg); got != before {
		t.Errorf("config after remove differs from before install:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(plugins, "moca-sandbox")); !os.IsNotExist(err) {
		t.Errorf("folder left: %v", err)
	}
	if code, out, _ := runScriptCmd(t, "remove", "moca-sandbox", "--config", cfg); code != 0 || !strings.Contains(out, "not installed") {
		t.Errorf("second remove: %d %q", code, out)
	}
}

func TestScript_RemoveRefusesAFileInstallDidNotWrite(t *testing.T) {
	cfg, plugins := scriptEnv(t, closedAddr(t), closedAddr(t))
	runScriptCmd(t, "install", "moca-sandbox", "--config", cfg)
	mine := filepath.Join(plugins, "moca-sandbox", "20-mine.py")
	os.WriteFile(mine, []byte("#!/bin/sh\n"), 0o700)
	code, _, errOut := runScriptCmd(t, "remove", "moca-sandbox", "--config", cfg)
	if code != 1 || !strings.Contains(errOut, "20-mine.py") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Errorf("the user's file is gone: %v", err)
	}
	if !strings.Contains(readConfig(t, cfg), "dir: moca-sandbox") {
		t.Error("the slot was removed although remove refused")
	}
}

// A folder of the same name that install did not write is not taken over.
func TestScript_InstallRefusesAFolderItDidNotWrite(t *testing.T) {
	cfg, plugins := scriptEnv(t, closedAddr(t), closedAddr(t))
	os.MkdirAll(filepath.Join(plugins, "moca-sandbox"), 0o700)
	os.WriteFile(filepath.Join(plugins, "moca-sandbox", "mine.py"), []byte("#!/bin/sh\n"), 0o700)
	code, _, errOut := runScriptCmd(t, "install", "moca-sandbox", "--config", cfg)
	if code != 1 || !strings.Contains(errOut, "mine.py") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
}

func TestScript_UnknownNameListsTheCatalog(t *testing.T) {
	cfg, _ := scriptEnv(t, closedAddr(t), closedAddr(t))
	code, _, errOut := runScriptCmd(t, "install", "nope", "--config", cfg)
	if code != 2 || !strings.Contains(errOut, "moca-sandbox") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
}

// A folder holding this release's files but no record — copied by hand as the
// v0.9.2 guide said, or left by an install that stopped before the record — is
// adopted: install writes the record, and remove can then take it out.
func TestScript_InstallAdoptsAHandCopiedFolder(t *testing.T) {
	cfg, plugins := scriptEnv(t, closedAddr(t), closedAddr(t))
	e, err := findCatalogEntry("moca-sandbox")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(plugins, "moca-sandbox")
	os.MkdirAll(dir, 0o700)
	for n, b := range e.Files {
		os.WriteFile(filepath.Join(dir, n), b, 0o700)
	}
	if st := listJSON(t, cfg)["moca-sandbox"]; st.State != stateIncomplete {
		t.Fatalf("hand-copied folder, no slot: state = %s, want incomplete", st.State)
	}
	if code, out, e := runScriptCmd(t, "install", "moca-sandbox", "--config", cfg); code != 0 {
		t.Fatalf("install: %d %s%s", code, out, e)
	}
	if st := listJSON(t, cfg)["moca-sandbox"]; st.State != stateInstalled {
		t.Fatalf("after install: state = %s", st.State)
	}
	if code, _, e := runScriptCmd(t, "remove", "moca-sandbox", "--config", cfg); code != 0 {
		t.Fatalf("remove: %s", e)
	}
}

// handCopy leaves what the v0.9.2 guide did: this release's files in the folder,
// no record, and the slot when withSlot is set.
func handCopy(t *testing.T, cfg, plugins string, withSlot bool) string {
	t.Helper()
	dir := filepath.Join(plugins, "moca-sandbox")
	if withSlot {
		if code, _, e := runScriptCmd(t, "install", "moca-sandbox", "--config", cfg); code != 0 {
			t.Fatal(e)
		}
		if err := os.Remove(filepath.Join(dir, scriptRecord)); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	e, err := findCatalogEntry("moca-sandbox")
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(dir, 0o700)
	for n, b := range e.Files {
		os.WriteFile(filepath.Join(dir, n), b, 0o700)
	}
	return dir
}

// remove takes out a hand-copied folder as install would have written it, with
// or without its slot, and still refuses one holding a file of the user's.
func TestScript_RemoveTakesOutAHandCopiedFolder(t *testing.T) {
	for _, withSlot := range []bool{true, false} {
		t.Run(map[bool]string{true: "with its slot", false: "without its slot"}[withSlot], func(t *testing.T) {
			cfg, plugins := scriptEnv(t, closedAddr(t), closedAddr(t))
			before := readConfig(t, cfg)
			dir := handCopy(t, cfg, plugins, withSlot)
			code, out, errOut := runScriptCmd(t, "remove", "moca-sandbox", "--config", cfg)
			if code != 0 || !strings.Contains(out, "Removed moca-sandbox") {
				t.Fatalf("exit %d: %s%s", code, out, errOut)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Errorf("folder left: %v", err)
			}
			if got := readConfig(t, cfg); got != before {
				t.Errorf("config after remove differs from before:\n%s", got)
			}
		})
	}
	t.Run("plus a file of the user's", func(t *testing.T) {
		cfg, plugins := scriptEnv(t, closedAddr(t), closedAddr(t))
		dir := handCopy(t, cfg, plugins, true)
		os.WriteFile(filepath.Join(dir, "20-mine.py"), []byte("#!/bin/sh\n"), 0o700)
		if code, _, errOut := runScriptCmd(t, "remove", "moca-sandbox", "--config", cfg); code != 1 || !strings.Contains(errOut, "20-mine.py") {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		if _, err := os.Stat(filepath.Join(dir, "moca-sandbox.py")); err != nil {
			t.Errorf("a file was removed although remove refused: %v", err)
		}
	})
}

// install over a hand-copied folder that already has its slot records it, so a
// later release that changes a script finds the folder outdated, which install
// updates, rather than modified, which it refuses.
func TestScript_InstallRecordsAHandCopiedFolderWithItsSlot(t *testing.T) {
	cfg, plugins := scriptEnv(t, closedAddr(t), closedAddr(t))
	dir := handCopy(t, cfg, plugins, true)
	code, out, errOut := runScriptCmd(t, "install", "moca-sandbox", "--config", cfg)
	if code != 0 || !strings.Contains(out, "already installed") {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	if _, ok := readRecord(dir); !ok {
		t.Fatal("install left the folder without a record")
	}
	e, err := findCatalogEntry("moca-sandbox")
	if err != nil {
		t.Fatal(err)
	}
	later := e
	later.Files = map[string][]byte{}
	for n, b := range e.Files {
		later.Files[n] = append(append([]byte{}, b...), "\n# a later release\n"...)
	}
	tgt, err := loadScriptTarget(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if st := stateOf(tgt, later); st != stateOutdated {
		t.Fatalf("a later release sees the adopted folder as %s, want outdated", st)
	}
}

// An empty folder with no record — what Cortex's reloader creates for a slot it
// watches — counts as no folder at all.
func TestScript_AnEmptyFolderIsNoFolder(t *testing.T) {
	cfg, plugins := scriptEnv(t, closedAddr(t), closedAddr(t))
	os.MkdirAll(filepath.Join(plugins, "moca-sandbox"), 0o700)
	if st := listJSON(t, cfg)["moca-sandbox"]; st.State != stateNotInstalled {
		t.Fatalf("empty folder: state = %s, want not installed", st.State)
	}
	if code, out, e := runScriptCmd(t, "install", "moca-sandbox", "--config", cfg); code != 0 {
		t.Fatalf("install over an empty folder: %d %s%s", code, out, e)
	}
}

// An update removes the files the earlier release wrote that this one does not
// ship, so the slot stops running them and remove can still take the folder out.
func TestScript_UpdateRemovesFilesTheReleaseDropped(t *testing.T) {
	cfg, plugins := scriptEnv(t, closedAddr(t), closedAddr(t))
	if code, _, e := runScriptCmd(t, "install", "moca-sandbox", "--config", cfg); code != 0 {
		t.Fatal(e)
	}
	dir := filepath.Join(plugins, "moca-sandbox")
	dropped := []byte("#!/bin/sh\n# shipped by an earlier release only\n")
	os.WriteFile(filepath.Join(dir, "10-old.sh"), dropped, 0o700)
	rec, _ := readRecord(dir)
	rec.Version = "v0.0.1"
	rec.Files["10-old.sh"] = sum(dropped)
	b, _ := json.Marshal(rec)
	os.WriteFile(filepath.Join(dir, scriptRecord), b, 0o600)
	if st := listJSON(t, cfg)["moca-sandbox"]; st.State != stateOutdated {
		t.Fatalf("state = %s, want outdated", st.State)
	}
	if code, out, e := runScriptCmd(t, "install", "moca-sandbox", "--config", cfg); code != 0 || !strings.Contains(out, "Updated") {
		t.Fatalf("update: %d %s%s", code, out, e)
	}
	if _, err := os.Stat(filepath.Join(dir, "10-old.sh")); !os.IsNotExist(err) {
		t.Errorf("the dropped file is still there: %v", err)
	}
	if code, _, e := runScriptCmd(t, "remove", "moca-sandbox", "--config", cfg); code != 0 {
		t.Fatalf("remove after update: %s", e)
	}
}

// An update whose new scripts do not start puts the earlier files back, so a
// fail_closed slot keeps running the scripts that worked. The check waits for
// the reload the new files trigger rather than reading the old build's counts.
func TestScript_AFailedUpdateRestoresTheEarlierFiles(t *testing.T) {
	f := newFakeCortex(t)
	f.present.Store(true)
	f.loaded.Store(1)
	cfg, plugins := scriptEnv(t, f.addr(), f.addr())
	if code, _, e := runScriptCmd(t, "install", "moca-sandbox", "--config", cfg); code != 0 {
		t.Fatal(e)
	}
	dir := filepath.Join(plugins, "moca-sandbox")
	old := []byte("#!/usr/bin/env python3\n# an earlier release\n")
	script := filepath.Join(dir, "moca-sandbox.py")
	os.WriteFile(script, old, 0o700)
	b, _ := json.Marshal(installRecord{Version: "v0.0.1", Files: map[string]string{"moca-sandbox.py": sum(old)}})
	os.WriteFile(filepath.Join(dir, scriptRecord), b, 0o600)

	// The old build still reports 1/1 until the reload; after it, nothing starts.
	f.reloadedAfter.Store(true)
	f.loaded.Store(0)
	f.notStarted.Store(1)
	code, _, errOut := runScriptCmd(t, "install", "moca-sandbox", "--config", cfg)
	if code != 1 || !strings.Contains(errOut, "did not load") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if got, _ := os.ReadFile(script); !bytes.Equal(got, old) {
		t.Errorf("the earlier script was not put back:\n%s", got)
	}
	if st := listJSON(t, cfg)["moca-sandbox"]; st.State != stateOutdated {
		t.Errorf("state after a failed update = %s, want outdated", st.State)
	}
}
