//go:build unix

package project

import (
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
