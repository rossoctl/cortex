package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteState_TornWriteBlocksEveryLaterEnable reproduces #1565's deadliness
// as a sequence, on a record built the way writeState builds it:
//
//  1. enable writes the record with os.WriteFile — truncate, then write.
//  2. agentop is killed mid-write: the file now holds a prefix of the JSON.
//  3. readState fails on it, and writeState REFUSES to overwrite — so every
//     later enable only warns, forever, and the user's Prior settings are
//     stuck in a file nothing can read.
//
// The fix is writeFileAtomic (CreateTemp + fsync + rename): a kill mid-write
// leaves the PREVIOUS record intact, so step 2 cannot produce this state.
// This test documents the failure mode as a sequence — it tears the record by
// hand, so it passes on main too and is NOT the regression guard; that is
// TestWriteState_FailedWriteKeepsThePreviousRecord below.
func TestWriteState_TornWriteBlocksEveryLaterEnable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude-code-state.json")

	// Step 1: a successful enable.
	prior := "https://before.example"
	st := managedState{
		Settings: "/settings.json",
		Prior:    map[string]*string{"ANTHROPIC_BASE_URL": &prior},
		Written:  map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:47600"},
	}
	if err := writeState(path, st); err != nil {
		t.Fatalf("first writeState: %v", err)
	}

	// Step 2: the kill mid-write. Simulated by truncating the file to a
	// prefix — exactly what os.WriteFile leaves between O_TRUNC and the
	// write completing.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b[:len(b)/2], 0o600); err != nil {
		t.Fatal(err)
	}

	// Step 3: every later enable refuses, and the Prior is unrecoverable.
	if _, err := readState(path); err == nil {
		t.Fatal("readState accepted a torn record")
	}
	err = writeState(path, st)
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("writeState after a torn write: %v, want the refusal that blocks every later enable", err)
	}
}

// TestWriteState_FailedWriteKeepsThePreviousRecord is the regression guard for
// writeFileAtomic in writeState: a write that cannot complete must leave the
// previous record fully readable. writeState used os.WriteFile, which opens
// with O_TRUNC — a kill in that window could destroy the only copy of the
// Prior. The failure is simulated with a directory the new code cannot create
// its temp file in; the old code still "succeeded" there, because truncating
// an EXISTING file needs no directory write permission.
func TestWriteState_FailedWriteKeepsThePreviousRecord(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("directory permissions do not bind root")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "claude-code-state.json")
	prior := "https://before.example"
	base := managedState{
		Settings: "/settings.json",
		Prior:    map[string]*string{"ANTHROPIC_BASE_URL": &prior},
		Written:  map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:47600"},
	}
	if err := writeState(path, base); err != nil {
		t.Fatalf("first writeState: %v", err)
	}

	// A re-enable with a changed Written must write; make that write fail.
	next := base
	next.Written = map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:47700"}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := writeState(path, next); err == nil {
		_ = os.Chmod(dir, 0o700)
		t.Fatal("writeState succeeded with the directory unwritable — it truncated the record in place")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	got, err := readState(path)
	if err != nil {
		t.Fatalf("the previous record did not survive the failed write: %v", err)
	}
	if got.Written["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:47600" {
		t.Errorf("record after a failed write is %v, want the original one", got.Written)
	}
}

// TestWriteSettings_SymlinkReplaced reproduces #1565's silent dotfiles break:
// writeSettings writes path+".tmp" and renames it over path, and rename over a
// symlink replaces the LINK with a regular file — so a ~/.claude/settings.json
// managed as a symlink into a dotfiles repo stops tracking, and the repo's copy
// silently goes stale while agentop believes it configured the user's settings.
func TestWriteSettings_SymlinkReplaced(t *testing.T) {
	dir := t.TempDir()
	dotfiles := filepath.Join(dir, "dotfiles")
	if err := os.MkdirAll(dotfiles, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dotfiles, "settings.json")
	if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if err := writeSettings(link, map[string]any{"env": map[string]any{"ANTHROPIC_BASE_URL": "http://127.0.0.1:47600"}}); err != nil {
		t.Fatalf("writeSettings over a symlink: %v", err)
	}

	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("writeSettings replaced the symlink with a regular file — the dotfiles repo's copy silently goes stale")
	}
	// And the target must carry the write, or the link is hollow.
	tb, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(tb), "47600") {
		t.Error("the write did not reach the symlink's target")
	}
}

// TestWriters_IgnoreAPlantedTmpSymlink pins the #1565 tmp-name fix at the
// writers that had it: writeSettings, bobWriteKeys and codexEnvFile.write all
// used to write path+".tmp", and os.WriteFile follows a symlink planted at
// that name — an open redirect for anything able to create files in the
// directory (userconfig.go guards its own copy against exactly this). The
// writers now go through writeFileAtomic, whose CreateTemp name cannot be
// planted, so the plant sits untouched and the write lands where it was aimed.
func TestWriters_IgnoreAPlantedTmpSymlink(t *testing.T) {
	writers := []struct {
		name  string
		write func(path string) error
	}{
		{"writeSettings", func(p string) error {
			return writeSettings(p, map[string]any{"routed": true})
		}},
		{"bobWriteKeys", func(p string) error {
			return bobWriteKeys(p, []bobEdit{{key: "routed", value: true}})
		}},
		{"codexEnvFile.write", func(p string) error {
			return (&codexEnvFile{lines: []string{"ROUTED=1"}}).write(p)
		}},
	}
	for _, w := range writers {
		t.Run(w.name, func(t *testing.T) {
			dir := t.TempDir()
			settings := filepath.Join(dir, "settings.json")
			if err := os.WriteFile(settings, []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			victim := filepath.Join(dir, "stolen")
			if err := os.WriteFile(victim, []byte("untouched\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(victim, settings+".tmp"); err != nil {
				t.Fatal(err)
			}

			if err := w.write(settings); err != nil {
				t.Fatalf("%s past a planted tmp symlink: %v", w.name, err)
			}

			if got, _ := os.ReadFile(victim); string(got) != "untouched\n" {
				t.Errorf("the planted tmp symlink redirected the write: victim now %q", got)
			}
			got, err := os.ReadFile(settings)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(got), "routed") && !strings.Contains(string(got), "ROUTED") {
				t.Errorf("the write did not land at the real path: %q", got)
			}
		})
	}
}
