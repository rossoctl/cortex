package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPathBlockIsInstallShsBytes(t *testing.T) {
	got := pathBlock("/home/u/.local/bin")
	want := "\n# added by Cortex (rossoctl/cortex) — delete these two lines to undo\n" +
		"export PATH=\"/home/u/.local/bin:$PATH\"\n"
	if got != want {
		t.Errorf("pathBlock = %q, want %q", got, want)
	}
}

func TestPathProfileFollowsInstallSh(t *testing.T) {
	home := t.TempDir()
	if got := pathProfile("/bin/zsh", home); got != filepath.Join(home, ".zshrc") {
		t.Errorf("zsh → %s", got)
	}
	if got := pathProfile("/bin/bash", home); got != filepath.Join(home, ".bash_profile") {
		t.Errorf("bash with no profile → %s, want .bash_profile", got)
	}
	if err := os.WriteFile(filepath.Join(home, ".profile"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := pathProfile("/usr/bin/bash", home); got != filepath.Join(home, ".profile") {
		t.Errorf("bash with only .profile → %s", got)
	}
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := pathProfile("/usr/bin/bash", home); got != filepath.Join(home, ".bashrc") {
		t.Errorf("bash with .bashrc and .profile → %s, want .bashrc", got)
	}
	if err := os.WriteFile(filepath.Join(home, ".bash_profile"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := pathProfile("/usr/bin/bash", home); got != filepath.Join(home, ".bash_profile") {
		t.Errorf("bash with all three profiles → %s, want .bash_profile", got)
	}
	for _, sh := range []string{"/usr/bin/fish", "", "/bin/sh"} {
		if got := pathProfile(sh, home); got != "" {
			t.Errorf("%q → %q, want none", sh, got)
		}
	}
}

func TestOnPath(t *testing.T) {
	if !onPath("/h/.local/bin:/usr/bin", "/h/.local/bin") {
		t.Error("first entry not found")
	}
	if !onPath("/usr/bin:/h/.local/bin:/bin", "/h/.local/bin") {
		t.Error("middle entry not found")
	}
	if onPath("/usr/bin:/h/.local/bin2", "/h/.local/bin") {
		t.Error("prefix matched as an entry")
	}
	if !onPath("/usr/bin:/h/.local/bin", "/h/.local/bin") {
		t.Error("last entry not found")
	}
	if onPath("/usr/bin:/opt/h/.local/bin", "/h/.local/bin") {
		t.Error("suffix matched as an entry")
	}
}

func TestSnapshotRestoresBytesModeAndAbsence(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "f")
	if err := os.WriteFile(f, []byte("before"), 0o640); err != nil {
		t.Fatal(err)
	}
	// WriteFile's mode passes through the umask; set it so 0640 is what we snapshot.
	if err := os.Chmod(f, 0o640); err != nil {
		t.Fatal(err)
	}
	s, err := snapshotFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, []byte("after-and-longer"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.restore(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f)
	fi, _ := os.Stat(f)
	if string(b) != "before" || fi.Mode().Perm() != 0o640 {
		t.Errorf("restored %q mode %v, want \"before\" 0640", b, fi.Mode().Perm())
	}

	gone := filepath.Join(dir, "absent")
	s2, err := snapshotFile(gone)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gone, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s2.restore(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Error("a file that did not exist survived restore")
	}
}

func TestSnapshotRestoresASymlinkAsASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	link := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	s, err := snapshotFile(link)
	if err != nil {
		t.Fatal(err)
	}
	// What writeSettings does since #1565: write THROUGH the link, which
	// survives, and the target carries the bytes.
	if err := writeFileAtomic(link, []byte(`{"env":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(link); err != nil || got != target {
		t.Fatalf("writeFileAtomic replaced the link: %q (%v), want a link to %s", got, err, target)
	}
	if b, _ := os.ReadFile(target); string(b) != `{"env":{}}` {
		t.Fatalf("the write did not reach the link's target: %q", b)
	}
	// Restore is for the day something ELSE breaks the link — a tool that
	// still renames over it. It must put the symlink back as a symlink.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(link, []byte(`{"replaced":"by another tool"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.restore(); err != nil {
		t.Fatal(err)
	}
	got, err := os.Readlink(link)
	if err != nil || got != target {
		t.Errorf("restore left %q (%v), want a link to %s", got, err, target)
	}
	// The target's bytes are part of the state now that writes follow the
	// link: undo must take the write back out of the dotfiles repo too, or the
	// user's settings still route to a proxy the undo just removed.
	if b, err := os.ReadFile(target); err != nil || string(b) != "{}" {
		t.Errorf("restore left the link's target at %q (%v), want the snapshot's original bytes", b, err)
	}
}

func TestSnapshotRestoresASymlinkUnderAMissingParent(t *testing.T) {
	sub := filepath.Join(t.TempDir(), "claude")
	link := filepath.Join(sub, "settings.json")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../real", link); err != nil {
		t.Fatal(err)
	}
	s, err := snapshotFile(link)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}
	if err := s.restore(); err != nil {
		t.Fatalf("restoring a link whose directory is gone: %v", err)
	}
	if got, err := os.Readlink(link); err != nil || got != "../real" {
		t.Errorf("restore left %q (%v), want a link to ../real", got, err)
	}
	if entries, _ := os.ReadDir(sub); len(entries) != 1 {
		t.Errorf("the link's directory holds %d entries, want only the link", len(entries))
	}
}

// A rename that fails leaves no temp link behind: a directory now where the link
// was makes the restore fail, and the parent holds only that directory.
func TestSymlinkRestoreRemovesItsTempLinkWhenTheRenameFails(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink("real", link); err != nil {
		t.Fatal(err)
	}
	s, err := snapshotFile(link)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(link, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.restore(); err == nil {
		t.Fatal("restoring a link over a directory succeeded")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("a failed link restore left %d entries, want only the directory", len(entries))
	}
}

func TestSnapshotFileReturnsNothingWhenItCannotRead(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a file this user cannot read")
	}
	f := filepath.Join(t.TempDir(), "locked")
	if err := os.WriteFile(f, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f, 0); err != nil {
		t.Fatal(err)
	}
	s, err := snapshotFile(f)
	if err == nil {
		t.Fatal("snapshotFile read a mode-0 file")
	}
	if s.existed {
		t.Error("a failed snapshot reports existed, so restore would rewrite the file")
	}
	_ = s.restore()
	if fi, err := os.Lstat(f); err != nil || fi.Size() != int64(len("keep")) {
		t.Errorf("restoring a failed snapshot touched the file (%v)", err)
	}
}

func TestWriteFileAtomicLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	if err := writeFileAtomic(filepath.Join(dir, "a"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("dir holds %d entries, want only the file", len(entries))
	}
}

func TestWriteFileAtomicRemovesItsTempFileOnFailure(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "occupied")
	if err := os.MkdirAll(filepath.Join(target, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(target, []byte("x"), 0o644); err == nil {
		t.Fatal("renaming over a non-empty directory succeeded")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("a failed write left %d entries, want only the directory", len(entries))
	}
}

func TestWriteFileAtomicCreatesAMissingParent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "not", "yet", "f")
	if err := writeFileAtomic(p, []byte("x"), 0o644); err != nil {
		t.Fatalf("writeFileAtomic under a missing parent: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "x" {
		t.Errorf("wrote %q, want \"x\"", b)
	}
}

func TestCopyFileCopiesContentAndSetsMode(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dst, 0o750); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(dst)
	fi, _ := os.Stat(dst)
	if string(b) != "payload" || fi.Mode().Perm() != 0o750 {
		t.Errorf("copyFile left %q mode %v, want \"payload\" 0750", b, fi.Mode().Perm())
	}
}

func TestSnapshotFileRefusesADirectory(t *testing.T) {
	_, err := snapshotFile(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "is not a regular file") {
		t.Errorf("snapshotFile(dir) error = %v, want a not-a-regular-file error", err)
	}
}

func TestHasPathMarker(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".zshrc")
	if hasPathMarker(p) {
		t.Error("a missing profile reported the marker")
	}
	if err := os.WriteFile(p, []byte("export X=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if hasPathMarker(p) {
		t.Error("a profile without the marker reported it")
	}
	if err := os.WriteFile(p, []byte("x\n"+pathBlock("/b")), 0o644); err != nil {
		t.Fatal(err)
	}
	if !hasPathMarker(p) {
		t.Error("the marker was not found")
	}
}
