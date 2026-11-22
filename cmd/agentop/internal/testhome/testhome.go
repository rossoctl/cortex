// Package testhome keeps agentop's tests out of the real user profile.
//
// os.UserHomeDir reads HOME on Linux and macOS but USERPROFILE on Windows, so a test that sets
// only HOME reads and writes the developer's real home on Windows: its ~/.cortex, ~/.codex and
// ~/.claude. Set points both at one directory. Isolate, called from TestMain, points every home
// and profile variable at a fresh temporary directory for the whole run, so a test that sets no
// home at all cannot reach the real profile either.
//
// Only test code imports this package.
package testhome

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Set points the home directory at dir for the rest of the test: HOME and USERPROFILE both, so
// os.UserHomeDir returns dir on every platform. dir may be "" to test a missing home. Like
// t.Setenv, it cannot be used in a parallel test.
func Set(t testing.TB, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}

// Temp points the home directory at a new t.TempDir() and returns it.
func Temp(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	Set(t, dir)
	return dir
}

// unset are variables that, left as they are, point a test at real directories in the
// profile: the XDG base directories (unset, they derive from HOME), and the agents' own homes.
var unset = []string{
	"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR",
	"CODEX_HOME", "CLAUDE_CONFIG_DIR",
}

// Isolate points HOME and USERPROFILE, and Windows' APPDATA and LOCALAPPDATA, at a fresh
// temporary home for the rest of the process, and unsets the variables that name directories
// inside the real profile. Call it in TestMain before m.Run, and the returned cleanup after:
//
//	cleanup := testhome.Isolate()
//	code := m.Run()
//	cleanup()
//	os.Exit(code)
//
// Child processes a test starts inherit the same environment. Isolate panics if it cannot make
// the directory: running the tests against the real profile instead is worse than not running.
func Isolate() (cleanup func()) {
	root, err := os.MkdirTemp("", "agentop-test-home-")
	if err != nil {
		panic("testhome: " + err.Error())
	}
	isolatedRoot = root
	home := filepath.Join(root, "home")
	appData := filepath.Join(home, "AppData", "Roaming")
	localAppData := filepath.Join(home, "AppData", "Local")
	for _, d := range []string{appData, localAppData} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			panic("testhome: " + err.Error())
		}
	}
	for k, v := range map[string]string{"HOME": home, "USERPROFILE": home, "APPDATA": appData, "LOCALAPPDATA": localAppData} {
		if err := os.Setenv(k, v); err != nil {
			panic("testhome: " + err.Error())
		}
	}
	for _, k := range unset {
		os.Unsetenv(k)
	}
	return func() { os.RemoveAll(root) }
}

// isolatedRoot is the temporary home Isolate made, or "".
var isolatedRoot string

// AssertIsolated fails the test unless the home directory is inside the temporary one Isolate
// made: a guard, in each package whose TestMain isolates, that the isolation is still there.
// Call it before the test sets a home of its own.
func AssertIsolated(t testing.TB) {
	t.Helper()
	if isolatedRoot == "" {
		t.Fatal("testhome.Isolate was not called: this package's TestMain must isolate the home directory")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir: %v", err)
	}
	if rel, err := filepath.Rel(isolatedRoot, home); err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("the home directory is %s, outside the isolated %s", home, isolatedRoot)
	}
}
