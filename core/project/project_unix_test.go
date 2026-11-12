//go:build unix

package project

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO at .git or at commondir must not block Root: open(2) on a FIFO waits for a writer.
func TestRootFIFO(t *testing.T) {
	for _, c := range []struct{ name, fifo string }{
		{".git", "r/.git"},
		{"commondir", "main/.git/worktrees/w/commondir"},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := tempDir(t)
			mkdirs(t, b, "r", "main/.git/worktrees/w")
			if c.name == "commondir" {
				writeFile(t, filepath.Join(b, "r/.git"), "gitdir: "+filepath.Join(b, "main/.git/worktrees/w")+"\n")
			}
			if err := syscall.Mkfifo(filepath.Join(b, c.fifo), 0o644); err != nil {
				t.Fatal(err)
			}
			done := make(chan string, 1)
			go func() { done <- Root(filepath.Join(b, "r"), "") }()
			select {
			case got := <-done:
				if want := filepath.Join(b, "r"); got != want {
					t.Fatalf("Root = %q; want %q", got, want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Root blocked on a FIFO")
			}
		})
	}
}

// A .git lookup that fails for a reason other than non-existence stops the walk: the inner
// level was not ruled out, so the outer repo must not be named.
func TestRootStopsOnLookupError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	b := tempDir(t)
	mkdirs(t, b, ".git", "inner")
	inner := filepath.Join(b, "inner")
	if err := os.Chmod(inner, 0o600); err != nil { // no search bit: Lstat(inner/.git) gives EACCES
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(inner, 0o755) })
	if got := Root(inner, ""); got != inner {
		t.Fatalf("Root = %q; want %q", got, inner)
	}
}
