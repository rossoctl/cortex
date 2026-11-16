package archive

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// skipWithoutUnixModes skips a test that reads or sets Unix permission bits. On Windows
// os.Chmod only toggles the read-only attribute, which a directory ignores, and a file's mode
// reads back as 0666: access is governed by ACLs instead.
func skipWithoutUnixModes(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix permission bits; access is governed by ACLs")
	}
}

// blockRenames makes a rename that creates an entry in root, or that moves sessionDir, fail
// until release is called. On Unix a read-only root refuses the new entry while data/
// underneath stays writable. Windows ignores that permission, but it will not rename a
// directory while a file under it is open, so there the test holds every segment open, as
// concurrent readers would.
func blockRenames(t *testing.T, root, sessionDir string) (release func()) {
	t.Helper()
	if runtime.GOOS != "windows" {
		if err := os.Chmod(root, 0o500); err != nil {
			t.Fatal(err)
		}
		release = func() { os.Chmod(root, 0o700) }
		t.Cleanup(release)
		return release
	}
	var held []*os.File
	release = func() {
		for _, f := range held {
			f.Close()
		}
		held = nil
	}
	t.Cleanup(release)
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl.zst") {
			f, err := os.Open(filepath.Join(sessionDir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			held = append(held, f)
		}
	}
	if len(held) == 0 {
		t.Fatalf("no segment in %s to hold open", sessionDir)
	}
	return release
}
