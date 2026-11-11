//go:build darwin || linux

package peerproc

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCwd_OwnProcess(t *testing.T) {
	wd, _ := os.Getwd()
	want, _ := filepath.EvalSymlinks(wd)
	got, err := Cwd(int32(os.Getpid()))
	if err != nil || got != want {
		t.Fatalf("Cwd = %q, %v; want %q", got, err, want)
	}
	if _, err := Cwd(0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Cwd(0) err = %v", err)
	}
}
