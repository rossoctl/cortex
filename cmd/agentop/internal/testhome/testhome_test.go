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
	defer func(old string) { isolatedRoot = old }(isolatedRoot)

	cleanup := Isolate()
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
