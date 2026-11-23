package testhome

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Set points os.UserHomeDir at the directory on every platform: HOME alone is not read on
// Windows, USERPROFILE alone is not read elsewhere.
func TestSet(t *testing.T) {
	dir := Temp(t)
	if got, err := os.UserHomeDir(); err != nil || got != dir {
		t.Fatalf("os.UserHomeDir() = %q, %v; want %q", got, err, dir)
	}
	if os.Getenv("HOME") != dir || os.Getenv("USERPROFILE") != dir {
		t.Errorf("HOME = %q, USERPROFILE = %q; want both %q", os.Getenv("HOME"), os.Getenv("USERPROFILE"), dir)
	}
}

// Isolate moves every home and profile variable into a new temporary directory, unsets the
// ones that name directories in the real profile, and its cleanup removes the directory.
func TestIsolate(t *testing.T) {
	// t.Setenv first, so the test restores what Isolate changes.
	for _, k := range append([]string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA"}, unset...) {
		t.Setenv(k, filepath.Join(t.TempDir(), "real-"+k))
	}
	runtimeDir := filepath.Join(t.TempDir(), "run-user")
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	for _, k := range []string{"GOPATH", "GOMODCACHE", "GOCACHE", "GOENV"} {
		t.Setenv(k, "")
	}
	realHome, _ := os.UserHomeDir()
	defer func(old string) { isolatedRoot = old }(isolatedRoot)

	cleanup := Isolate()
	// The runtime directory is not the profile's: systemctl --user needs the real one.
	if got := os.Getenv("XDG_RUNTIME_DIR"); got != runtimeDir {
		t.Errorf("XDG_RUNTIME_DIR = %q, want it kept as %q", got, runtimeDir)
	}
	// The Go toolchain keeps the real home's directories.
	if got, want := os.Getenv("GOPATH"), filepath.Join(realHome, "go"); got != want {
		t.Errorf("GOPATH = %q, want the real default %q", got, want)
	}
	if got := os.Getenv("GOMODCACHE"); got != filepath.Join(realHome, "go", "pkg", "mod") {
		t.Errorf("GOMODCACHE = %q, want it under the real GOPATH", got)
	}
	for _, k := range []string{"GOCACHE", "GOENV"} {
		if v := os.Getenv(k); v == "" || strings.HasPrefix(v, isolatedRoot) {
			t.Errorf("%s = %q, want a real directory outside %q", k, v, isolatedRoot)
		}
	}
	// Real gives one test the real home back, and the isolated one returns after it.
	t.Run("Real", func(t *testing.T) {
		Real(t)
		if got, _ := os.UserHomeDir(); got != realHome {
			t.Errorf("after Real, os.UserHomeDir() = %q, want %q", got, realHome)
		}
	})
	if got, _ := os.UserHomeDir(); !strings.HasPrefix(got, isolatedRoot) {
		t.Errorf("after the Real subtest, os.UserHomeDir() = %q, want the isolated home back", got)
	}
	home, err := os.UserHomeDir()
	if err != nil || !strings.HasPrefix(home, isolatedRoot) || strings.Contains(home, "real-") {
		t.Fatalf("os.UserHomeDir() = %q, %v; want a directory inside %q", home, err, isolatedRoot)
	}
	for _, k := range []string{"APPDATA", "LOCALAPPDATA"} {
		if v := os.Getenv(k); !strings.HasPrefix(v, home) {
			t.Errorf("%s = %q, want it inside the temporary home %q", k, v, home)
		}
		if fi, err := os.Stat(os.Getenv(k)); err != nil || !fi.IsDir() {
			t.Errorf("%s does not exist as a directory: %v", k, err)
		}
	}
	for _, k := range unset {
		if v, ok := os.LookupEnv(k); ok {
			t.Errorf("%s is still set (%q)", k, v)
		}
	}
	AssertIsolated(t)

	root := isolatedRoot
	cleanup()
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("cleanup left %s: %v", root, err)
	}
}
